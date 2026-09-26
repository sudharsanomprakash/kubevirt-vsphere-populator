package vmdk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
)

// WriterOptions controls WriteStreamOptimized. The zero value produces a
// stream laid out like the VMware-generated sample documented in
// docs/design.md: header, descriptor, grains in ascending order with a grain
// table after each table's grains, grain directory, footer and end-of-stream
// marker. The non-default options exist to produce malformed or unusual
// streams for tests.
type WriterOptions struct {
	// GrainSectors is the grain size in sectors (default 128, i.e. 64 KiB).
	GrainSectors uint64
	// ShuffleSeed, when non-zero, writes grains in a pseudo-random order
	// followed by all grain tables.
	ShuffleSeed uint64
	// OmitFooter drops the footer marker and footer.
	OmitFooter bool
	// KeepZeroGrains writes grains that contain only zeros instead of
	// leaving them unallocated.
	KeepZeroGrains bool
	// CreateType overrides the descriptor createType.
	CreateType string
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// WriteStreamOptimized encodes size bytes of raw guest disk read from src as a
// streamOptimized VMDK. Size must be a multiple of 512. It returns the number
// of bytes written.
func WriteStreamOptimized(w io.Writer, src io.ReaderAt, size int64, opts WriterOptions) (int64, error) {
	if size <= 0 || size%SectorSize != 0 {
		return 0, fmt.Errorf("size %d is not a positive multiple of %d", size, SectorSize)
	}
	grainSectors := opts.GrainSectors
	if grainSectors == 0 {
		grainSectors = 128
	}
	createType := opts.CreateType
	if createType == "" {
		createType = "streamOptimized"
	}
	capacity := uint64(size / SectorSize)
	h := Header{
		Magic: Magic, Version: 3,
		Flags:    FlagValidNewlineTest | FlagCompressed | FlagMarkers,
		Capacity: capacity, GrainSize: grainSectors,
		DescriptorOffset: 1, DescriptorSize: 1, NumGTEsPerGT: 512,
		GDOffset: GDAtEnd, OverHead: grainSectors,
		SingleEndLineChar: '\n', NonEndLineChar: ' ', DoubleEndLineChar1: '\r', DoubleEndLineChar2: '\n',
		CompressAlgorithm: CompressionDeflate,
	}
	desc := fmt.Sprintf("# Disk DescriptorFile\nversion=1\nCID=fffffffe\nparentCID=ffffffff\ncreateType=%q\n\n"+
		"# Extent description\nRDONLY %d SPARSE \"generated-stream.vmdk\"\n\n# The Disk Data Base\n#DDB\n\n"+
		"ddb.adapterType = \"lsilogic\"\nddb.virtualHWVersion = \"14\"\n", createType, capacity)
	if len(desc) > SectorSize {
		return 0, fmt.Errorf("descriptor too large")
	}

	cw := &countWriter{w: w}
	sector := make([]byte, SectorSize)
	writeSectors := func(b []byte) error {
		if _, err := cw.Write(b); err != nil {
			return err
		}
		if pad := (SectorSize - len(b)%SectorSize) % SectorSize; pad > 0 {
			_, err := cw.Write(make([]byte, pad))
			return err
		}
		return nil
	}
	if err := writeSectors(h.Marshal()); err != nil {
		return cw.n, err
	}
	if err := writeSectors([]byte(desc)); err != nil {
		return cw.n, err
	}
	if err := writeSectors(make([]byte, int64(h.OverHead-2)*SectorSize)); err != nil {
		return cw.n, err
	}

	grainBytes := int64(grainSectors) * SectorSize
	numGrains := h.NumGrains()
	numGTs := h.NumGTs()
	gtes := make([]uint32, numGTs*512)

	order := make([]uint64, numGrains)
	for i := range order {
		order[i] = uint64(i)
	}
	if opts.ShuffleSeed != 0 {
		rng := rand.New(rand.NewPCG(opts.ShuffleSeed, opts.ShuffleSeed^0x9e3779b97f4a7c15))
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	}

	buf := make([]byte, grainBytes)
	var comp bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&comp, zlib.DefaultCompression)
	writeGrain := func(idx uint64) error {
		off := int64(idx) * grainBytes
		n := min(grainBytes, size-off)
		if _, err := src.ReadAt(buf[:n], off); err != nil && err != io.EOF {
			return err
		}
		if !opts.KeepZeroGrains && digest.IsZero(buf[:n]) {
			return nil
		}
		clear(buf[n:])
		comp.Reset()
		zw.Reset(&comp)
		if _, err := zw.Write(buf[:grainBytes]); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		gtes[idx] = uint32(cw.n / SectorSize)
		marker := make([]byte, 12, 12+comp.Len())
		binary.LittleEndian.PutUint64(marker[0:], idx*grainSectors)
		binary.LittleEndian.PutUint32(marker[8:], uint32(comp.Len()))
		return writeSectors(append(marker, comp.Bytes()...))
	}
	writeMeta := func(typ uint32, body []byte) error {
		clear(sector)
		binary.LittleEndian.PutUint64(sector[0:], uint64(len(body)+SectorSize-1)/SectorSize)
		binary.LittleEndian.PutUint32(sector[12:], typ)
		if err := writeSectors(sector); err != nil {
			return err
		}
		if len(body) == 0 {
			return nil
		}
		return writeSectors(body)
	}
	gd := make([]byte, numGTs*4)
	writeGT := func(t uint64) error {
		body := make([]byte, 512*4)
		for e := range uint64(512) {
			binary.LittleEndian.PutUint32(body[e*4:], gtes[t*512+e])
		}
		binary.LittleEndian.PutUint32(gd[t*4:], uint32((cw.n+SectorSize)/SectorSize))
		return writeMeta(MarkerGT, body)
	}

	if opts.ShuffleSeed == 0 {
		for t := range numGTs {
			for e := range uint64(512) {
				if idx := t*512 + e; idx < numGrains {
					if err := writeGrain(idx); err != nil {
						return cw.n, err
					}
				}
			}
			if err := writeGT(t); err != nil {
				return cw.n, err
			}
		}
	} else {
		for _, idx := range order {
			if err := writeGrain(idx); err != nil {
				return cw.n, err
			}
		}
		for t := range numGTs {
			if err := writeGT(t); err != nil {
				return cw.n, err
			}
		}
	}

	gdSector := uint64((cw.n + SectorSize) / SectorSize)
	if err := writeMeta(MarkerGD, gd); err != nil {
		return cw.n, err
	}
	if !opts.OmitFooter {
		f := h
		f.GDOffset = gdSector
		if err := writeMeta(MarkerFooter, f.Marshal()); err != nil {
			return cw.n, err
		}
	}
	if err := writeMeta(MarkerEOS, nil); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}
