package vmdk

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
)

// Progress describes how far decoding has advanced.
type Progress struct {
	// StreamBytes is the number of exported stream bytes consumed.
	StreamBytes int64
	// GuestOffset is the end offset (in guest disk bytes) of the highest grain decoded so far.
	GuestOffset int64
	// VirtualSize is the guest disk size in bytes.
	VirtualSize int64
	// Grains is the number of grains decoded so far.
	Grains int64
}

// Sink receives decoded grains. Off is the guest byte offset and data holds
// exactly the grain bytes that lie within the virtual disk size. The data
// slice is reused after Grain returns.
type Sink interface {
	Grain(off int64, data []byte) error
}

// DecodeOptions controls Decode and DecodeAt.
type DecodeOptions struct {
	// OnHeader is called once the header and descriptor have been validated,
	// before any grain is decoded. Returning an error aborts decoding.
	OnHeader func(Header, Descriptor) error
	// OnProgress is called after every grain and metadata marker.
	OnProgress func(Progress)
	// Sink optionally receives decoded grains.
	Sink Sink
	// DigestChunkSize is the chunk size of the guest disk digest (default digest.DefaultChunkSize).
	DigestChunkSize int
}

// Result summarizes a decoded stream.
type Result struct {
	Header     Header     `json:"header"`
	Descriptor Descriptor `json:"descriptor"`
	Footer     *Header    `json:"footer,omitempty"`
	// StreamBytes is the number of bytes up to and including the end-of-stream marker.
	StreamBytes int64 `json:"streamBytes"`
	// TrailingBytes is the number of (zero) bytes that followed the end-of-stream marker.
	TrailingBytes int64 `json:"trailingBytes"`
	// Grains is the number of grain markers.
	Grains int64 `json:"grains"`
	// AllocatedBytes is the number of guest bytes covered by grains.
	AllocatedBytes int64 `json:"allocatedBytes"`
	GTMarkers      int   `json:"gtMarkers"`
	GDMarkers      int   `json:"gdMarkers"`
	// Ordered reports whether grains appeared in strictly ascending guest order.
	Ordered bool `json:"ordered"`
	// Digest is the chunked SHA-256 of the full guest disk (zero-filled where no
	// grain is present). It is nil when the grains were not in ascending order;
	// use DecodeAt on a staged copy in that case.
	Digest []byte `json:"-"`
}

// VirtualSize returns the guest disk size in bytes.
func (r *Result) VirtualSize() int64 { return r.Header.VirtualSize() }

type decoder struct {
	r       *bufio.Reader
	pos     int64
	opts    DecodeOptions
	hdr     Header
	res     *Result
	bitmap  []uint64
	sector  []byte
	payload []byte
	grain   []byte
	zr      io.ReadCloser
	hash    *digest.Chunked
	next    int64 // next guest offset expected by the in-order digest
	highest int64
}

// Decode reads a complete streamOptimized VMDK from r, validating its
// structure and every grain, and computes the guest disk digest when the
// grains are in ascending order. It fails on truncated input, corrupt or
// over-long compressed grains, duplicate or misaligned grains, unknown
// markers, inconsistent footers and non-zero data after the end-of-stream
// marker.
func Decode(r io.Reader, opts DecodeOptions) (*Result, error) {
	d := &decoder{
		r:      bufio.NewReaderSize(r, 1<<20),
		opts:   opts,
		res:    &Result{Ordered: true},
		sector: make([]byte, SectorSize),
	}
	if err := d.run(); err != nil {
		return d.res, err
	}
	return d.res, nil
}

func (d *decoder) readFull(b []byte) error {
	n, err := io.ReadFull(d.r, b)
	d.pos += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: stream truncated at byte %d: %w", ErrFormat, d.pos, io.ErrUnexpectedEOF)
		}
		return err
	}
	return nil
}

func (d *decoder) progress() {
	if d.opts.OnProgress != nil {
		d.opts.OnProgress(Progress{
			StreamBytes: d.pos,
			GuestOffset: d.highest,
			VirtualSize: d.hdr.VirtualSize(),
			Grains:      d.res.Grains,
		})
	}
}

func (d *decoder) run() error {
	if err := d.readFull(d.sector); err != nil {
		return err
	}
	h, err := ParseHeader(d.sector)
	if err != nil {
		return err
	}
	if err := h.ValidateStreamOptimized(); err != nil {
		return err
	}
	if h.GDOffset != GDAtEnd {
		return formatErr("header gdOffset is %d; a streamed export must place the grain directory at the end", h.GDOffset)
	}
	d.hdr = h
	d.res.Header = h

	// Read the rest of the overhead area, which contains the descriptor.
	over := make([]byte, int64(h.OverHead-1)*SectorSize)
	if err := d.readFull(over); err != nil {
		return err
	}
	if h.DescriptorSize == 0 {
		return formatErr("stream has no embedded descriptor")
	}
	start := int64(h.DescriptorOffset-1) * SectorSize
	desc, err := ParseDescriptor(over[start : start+int64(h.DescriptorSize)*SectorSize])
	if err != nil {
		return err
	}
	if err := desc.ValidateStreamOptimized(h); err != nil {
		return err
	}
	d.res.Descriptor = desc
	if d.opts.OnHeader != nil {
		if err := d.opts.OnHeader(h, desc); err != nil {
			return err
		}
	}

	grainBytes := h.GrainBytes()
	d.bitmap = make([]uint64, (h.NumGrains()+63)/64)
	d.grain = make([]byte, grainBytes)
	d.payload = make([]byte, 0, maxCompressed(grainBytes)+SectorSize)
	d.hash = digest.New(d.opts.DigestChunkSize)

	for {
		markerPos := d.pos
		if err := d.readFull(d.sector); err != nil {
			return err
		}
		val := binary.LittleEndian.Uint64(d.sector[0:])
		size := binary.LittleEndian.Uint32(d.sector[8:])
		if size != 0 {
			if err := d.grainMarker(markerPos, val, size); err != nil {
				return err
			}
			d.progress()
			continue
		}
		typ := binary.LittleEndian.Uint32(d.sector[12:])
		done, err := d.metadataMarker(markerPos, val, typ)
		if err != nil {
			return err
		}
		d.progress()
		if done {
			break
		}
	}
	d.res.StreamBytes = d.pos

	if d.res.Footer == nil {
		return formatErr("stream ended without a footer")
	}
	if d.res.GDMarkers == 0 {
		return formatErr("stream ended without a grain directory")
	}

	// Anything after the end-of-stream marker must be zero padding.
	buf := make([]byte, 64*1024)
	for {
		n, err := d.r.Read(buf)
		if n > 0 {
			if !digest.IsZero(buf[:n]) {
				return formatErr("non-zero data after the end-of-stream marker at byte %d", d.pos)
			}
			d.pos += int64(n)
			d.res.TrailingBytes += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	if d.res.Ordered {
		d.hash.WriteZeros(h.VirtualSize() - d.next)
		d.res.Digest = d.hash.Sum()
	}
	return nil
}

// maxCompressed bounds the compressed size of a grain. Deflate expands
// incompressible input by a few bytes per 16 KiB block; a factor of two is a
// generous bound that still limits memory use for hostile input.
func maxCompressed(grainBytes int64) int64 { return 2*grainBytes + 1024 }

func (d *decoder) grainMarker(markerPos int64, lba uint64, size uint32) error {
	h := d.hdr
	if int64(size) > maxCompressed(h.GrainBytes()) {
		return formatErr("grain at sector %d claims %d compressed bytes (limit %d)", lba, size, maxCompressed(h.GrainBytes()))
	}
	if lba%h.GrainSize != 0 {
		return formatErr("grain at sector %d is not aligned to the %d-sector grain size", lba, h.GrainSize)
	}
	if lba >= h.Capacity {
		return formatErr("grain at sector %d is beyond capacity %d", lba, h.Capacity)
	}
	idx := lba / h.GrainSize
	if d.bitmap[idx/64]&(1<<(idx%64)) != 0 {
		return formatErr("duplicate grain for sector %d at stream offset %d", lba, markerPos)
	}
	d.bitmap[idx/64] |= 1 << (idx % 64)

	// The marker sector holds the first 500 payload bytes; the payload is
	// padded to a sector boundary.
	total := (12 + int64(size) + SectorSize - 1) / SectorSize * SectorSize
	d.payload = append(d.payload[:0], d.sector[12:]...)
	if rest := total - SectorSize; rest > 0 {
		d.payload = d.payload[:SectorSize-12+rest]
		if err := d.readFull(d.payload[SectorSize-12:]); err != nil {
			return err
		}
	}
	compressed := d.payload[:size]

	off := int64(lba) * SectorSize
	want := min(h.GrainBytes(), h.VirtualSize()-off)
	n, err := d.inflate(compressed)
	if err != nil {
		return formatErr("grain at sector %d: %v", lba, err)
	}
	if int64(n) < want {
		return formatErr("grain at sector %d decompressed to %d bytes, want %d", lba, n, want)
	}
	data := d.grain[:want]

	if d.opts.Sink != nil {
		if err := d.opts.Sink.Grain(off, data); err != nil {
			return err
		}
	}
	if d.res.Ordered {
		if off < d.next {
			d.res.Ordered = false
		} else {
			d.hash.WriteZeros(off - d.next)
			_, _ = d.hash.Write(data)
			d.next = off + want
		}
	}
	d.res.Grains++
	d.res.AllocatedBytes += want
	d.highest = max(d.highest, off+want)
	return nil
}

// inflate decompresses one zlib stream into d.grain and returns the number of
// bytes produced. It fails if the stream is corrupt, fails its checksum, or
// produces more than one grain of data.
func (d *decoder) inflate(compressed []byte) (int, error) {
	return inflateInto(&d.zr, compressed, d.grain)
}

func inflateInto(zr *io.ReadCloser, compressed, out []byte) (int, error) {
	br := bytes.NewReader(compressed)
	if *zr == nil {
		r, err := zlib.NewReader(br)
		if err != nil {
			return 0, err
		}
		*zr = r
	} else if err := (*zr).(zlib.Resetter).Reset(br, nil); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(*zr, out)
	switch {
	case err == nil:
		// Exactly one grain was produced; the stream must end here. Reading
		// to EOF also verifies the Adler-32 checksum.
		var extra [1]byte
		m, err2 := (*zr).Read(extra[:])
		if m > 0 {
			return n, errors.New("decompressed data exceeds the grain size")
		}
		if !errors.Is(err2, io.EOF) {
			if err2 == nil {
				err2 = errors.New("compressed stream did not terminate")
			}
			return n, err2
		}
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		// Short grain (only valid for the final partial grain; the caller checks the length).
	default:
		return n, err
	}
	return n, nil
}

func (d *decoder) metadataMarker(markerPos int64, sectors uint64, typ uint32) (bool, error) {
	h := d.hdr
	maxGDSectors := (h.NumGTs()*4 + SectorSize - 1) / SectorSize
	switch typ {
	case MarkerEOS:
		if sectors != 0 {
			return false, formatErr("end-of-stream marker at %d has non-zero size %d", markerPos, sectors)
		}
		return true, nil
	case MarkerGT:
		if sectors != uint64(h.NumGTEsPerGT)*4/SectorSize {
			return false, formatErr("grain table marker at %d has size %d sectors", markerPos, sectors)
		}
		d.res.GTMarkers++
	case MarkerGD:
		if sectors == 0 || sectors > maxGDSectors+1 {
			return false, formatErr("grain directory marker at %d has size %d sectors (max %d)", markerPos, sectors, maxGDSectors+1)
		}
		d.res.GDMarkers++
	case MarkerFooter:
		if sectors != 1 {
			return false, formatErr("footer marker at %d has size %d sectors", markerPos, sectors)
		}
		if d.res.Footer != nil {
			return false, formatErr("second footer at %d", markerPos)
		}
	default:
		return false, formatErr("unknown metadata marker type %d at stream offset %d", typ, markerPos)
	}
	body := make([]byte, int64(sectors)*SectorSize)
	if err := d.readFull(body); err != nil {
		return false, err
	}
	if typ == MarkerFooter {
		f, err := ParseHeader(body)
		if err != nil {
			return false, fmt.Errorf("footer: %w", err)
		}
		if f.Capacity != h.Capacity || f.GrainSize != h.GrainSize || f.Flags != h.Flags ||
			f.CompressAlgorithm != h.CompressAlgorithm || f.NumGTEsPerGT != h.NumGTEsPerGT {
			return false, formatErr("footer does not match header (capacity %d/%d, grain %d/%d, flags 0x%x/0x%x)",
				f.Capacity, h.Capacity, f.GrainSize, h.GrainSize, f.Flags, h.Flags)
		}
		if f.GDOffset == GDAtEnd || f.GDOffset == 0 {
			return false, formatErr("footer does not locate the grain directory")
		}
		d.res.Footer = &f
	}
	return false, nil
}
