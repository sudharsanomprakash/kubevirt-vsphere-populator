// Package vmdk parses the VMware "streamOptimized" sparse extent format that
// vSphere HttpNfcLease exports serve for virtual disks.
//
// The implementation follows the VMware Virtual Disk Format 1.1 specification
// (sparse extent header, grain markers and metadata markers). It is used by the
// worker to validate the exported stream, report progress, and compute an
// independent digest of the guest disk that is compared with the destination
// after the pinned qemu-img converter has written it. It never writes to the
// destination itself.
package vmdk

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Format constants from the VMware Virtual Disk Format 1.1 specification.
const (
	SectorSize = 512

	Magic   uint32 = 0x564d444b // "KDMV" in little-endian byte order
	GDAtEnd uint64 = 0xffffffffffffffff

	FlagValidNewlineTest uint32 = 1 << 0
	FlagRedundantGT      uint32 = 1 << 1
	FlagZeroedGTE        uint32 = 1 << 2
	FlagCompressed       uint32 = 1 << 16
	FlagMarkers          uint32 = 1 << 17

	CompressionNone    uint16 = 0
	CompressionDeflate uint16 = 1

	MarkerEOS    uint32 = 0
	MarkerGT     uint32 = 1
	MarkerGD     uint32 = 2
	MarkerFooter uint32 = 3
)

// Limits applied to untrusted headers.
const (
	// MaxCapacitySectors bounds the virtual size (64 TiB).
	MaxCapacitySectors uint64 = (64 << 40) / SectorSize
	// MaxGrainSectors bounds the grain size (8 MiB) and therefore the size
	// of a single decompression buffer.
	MaxGrainSectors uint64 = 16384
	// MinGrainSectors is the smallest grain size the specification allows (4 KiB).
	MinGrainSectors uint64 = 8
	// MaxDescriptorSectors bounds the embedded descriptor size (1 MiB).
	MaxDescriptorSectors uint64 = 2048
)

// ErrFormat is wrapped by all errors caused by malformed or unsupported input.
var ErrFormat = errors.New("vmdk format error")

func formatErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, args...))
}

// Header is a SparseExtentHeader.
type Header struct {
	Magic              uint32 `json:"magic"`
	Version            uint32 `json:"version"`
	Flags              uint32 `json:"flags"`
	Capacity           uint64 `json:"capacitySectors"`
	GrainSize          uint64 `json:"grainSizeSectors"`
	DescriptorOffset   uint64 `json:"descriptorOffsetSectors"`
	DescriptorSize     uint64 `json:"descriptorSizeSectors"`
	NumGTEsPerGT       uint32 `json:"numGTEsPerGT"`
	RGDOffset          uint64 `json:"rgdOffsetSectors"`
	GDOffset           uint64 `json:"gdOffsetSectors"`
	OverHead           uint64 `json:"overHeadSectors"`
	UncleanShutdown    uint8  `json:"uncleanShutdown"`
	SingleEndLineChar  byte   `json:"-"`
	NonEndLineChar     byte   `json:"-"`
	DoubleEndLineChar1 byte   `json:"-"`
	DoubleEndLineChar2 byte   `json:"-"`
	CompressAlgorithm  uint16 `json:"compressAlgorithm"`
}

// ParseHeader decodes a 512-byte sparse extent header. It only checks the magic
// number; use ValidateStreamOptimized for semantic checks.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < SectorSize {
		return Header{}, formatErr("header is %d bytes, want %d", len(b), SectorSize)
	}
	le := binary.LittleEndian
	h := Header{
		Magic:              le.Uint32(b[0:]),
		Version:            le.Uint32(b[4:]),
		Flags:              le.Uint32(b[8:]),
		Capacity:           le.Uint64(b[12:]),
		GrainSize:          le.Uint64(b[20:]),
		DescriptorOffset:   le.Uint64(b[28:]),
		DescriptorSize:     le.Uint64(b[36:]),
		NumGTEsPerGT:       le.Uint32(b[44:]),
		RGDOffset:          le.Uint64(b[48:]),
		GDOffset:           le.Uint64(b[56:]),
		OverHead:           le.Uint64(b[64:]),
		UncleanShutdown:    b[72],
		SingleEndLineChar:  b[73],
		NonEndLineChar:     b[74],
		DoubleEndLineChar1: b[75],
		DoubleEndLineChar2: b[76],
		CompressAlgorithm:  le.Uint16(b[77:]),
	}
	if h.Magic != Magic {
		return h, formatErr("bad magic 0x%08x (not a VMware sparse extent)", h.Magic)
	}
	return h, nil
}

// Marshal encodes the header into a 512-byte sector.
func (h Header) Marshal() []byte {
	b := make([]byte, SectorSize)
	le := binary.LittleEndian
	le.PutUint32(b[0:], h.Magic)
	le.PutUint32(b[4:], h.Version)
	le.PutUint32(b[8:], h.Flags)
	le.PutUint64(b[12:], h.Capacity)
	le.PutUint64(b[20:], h.GrainSize)
	le.PutUint64(b[28:], h.DescriptorOffset)
	le.PutUint64(b[36:], h.DescriptorSize)
	le.PutUint32(b[44:], h.NumGTEsPerGT)
	le.PutUint64(b[48:], h.RGDOffset)
	le.PutUint64(b[56:], h.GDOffset)
	le.PutUint64(b[64:], h.OverHead)
	b[72] = h.UncleanShutdown
	b[73] = h.SingleEndLineChar
	b[74] = h.NonEndLineChar
	b[75] = h.DoubleEndLineChar1
	b[76] = h.DoubleEndLineChar2
	le.PutUint16(b[77:], h.CompressAlgorithm)
	return b
}

// VirtualSize returns the guest-visible disk size in bytes.
func (h Header) VirtualSize() int64 { return int64(h.Capacity) * SectorSize }

// GrainBytes returns the grain size in bytes.
func (h Header) GrainBytes() int64 { return int64(h.GrainSize) * SectorSize }

// NumGrains returns the number of grains needed to cover the capacity.
func (h Header) NumGrains() uint64 { return (h.Capacity + h.GrainSize - 1) / h.GrainSize }

// NumGTs returns the number of grain tables needed to cover the capacity.
func (h Header) NumGTs() uint64 {
	per := uint64(h.NumGTEsPerGT)
	return (h.NumGrains() + per - 1) / per
}

// ValidateStreamOptimized checks that the header describes a compressed,
// marker-based stream whose grain directory follows the data (as produced by
// vSphere exports), with sane bounds for all sizes.
func (h Header) ValidateStreamOptimized() error {
	if h.Magic != Magic {
		return formatErr("bad magic 0x%08x", h.Magic)
	}
	if h.Version < 1 || h.Version > 3 {
		return formatErr("unsupported sparse extent version %d", h.Version)
	}
	if h.Flags&FlagCompressed == 0 || h.Flags&FlagMarkers == 0 {
		return formatErr("flags 0x%x lack compressed-grain and marker bits: not a streamOptimized extent", h.Flags)
	}
	if h.CompressAlgorithm != CompressionDeflate {
		return formatErr("unsupported compression algorithm %d", h.CompressAlgorithm)
	}
	if h.Flags&FlagValidNewlineTest != 0 &&
		(h.SingleEndLineChar != '\n' || h.NonEndLineChar != ' ' || h.DoubleEndLineChar1 != '\r' || h.DoubleEndLineChar2 != '\n') {
		return formatErr("newline detection characters are corrupted (file was transferred in text mode?)")
	}
	if h.Capacity == 0 || h.Capacity > MaxCapacitySectors {
		return formatErr("capacity %d sectors out of range", h.Capacity)
	}
	if h.GrainSize < MinGrainSectors || h.GrainSize > MaxGrainSectors || h.GrainSize&(h.GrainSize-1) != 0 {
		return formatErr("grain size %d sectors is not a supported power of two", h.GrainSize)
	}
	if h.NumGTEsPerGT != 512 {
		return formatErr("numGTEsPerGT is %d, the specification requires 512", h.NumGTEsPerGT)
	}
	if h.DescriptorSize > MaxDescriptorSectors {
		return formatErr("descriptor size %d sectors too large", h.DescriptorSize)
	}
	if h.DescriptorSize > 0 && h.DescriptorOffset == 0 {
		return formatErr("descriptor size set without offset")
	}
	if h.OverHead < 1 || h.OverHead < h.DescriptorOffset+h.DescriptorSize {
		return formatErr("overhead %d sectors does not cover header and descriptor", h.OverHead)
	}
	if h.OverHead > 1<<20 {
		return formatErr("overhead %d sectors too large", h.OverHead)
	}
	return nil
}
