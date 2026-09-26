package vmdk

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
)

// DecodeAt decodes a staged streamOptimized VMDK through its grain directory
// and grain tables, visiting the guest disk in ascending offset order. It is
// used when the stream's grains were not in ascending order, and in tests to
// cross-check the streaming decoder. The footer must come from a successful
// Decode of the same bytes.
func DecodeAt(r io.ReaderAt, size int64, footer Header, opts DecodeOptions) (*Result, error) {
	sector := make([]byte, SectorSize)
	if _, err := r.ReadAt(sector, 0); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	h, err := ParseHeader(sector)
	if err != nil {
		return nil, err
	}
	if err := h.ValidateStreamOptimized(); err != nil {
		return nil, err
	}
	if footer.Capacity != h.Capacity || footer.GrainSize != h.GrainSize {
		return nil, formatErr("footer does not match header")
	}
	res := &Result{Header: h, Footer: &footer, StreamBytes: size, Ordered: true}
	if opts.OnHeader != nil {
		if err := opts.OnHeader(h, Descriptor{}); err != nil {
			return res, err
		}
	}

	numGTs := h.NumGTs()
	gdBytes := int64(numGTs) * 4
	gdOff := int64(footer.GDOffset) * SectorSize
	if gdOff <= 0 || gdOff+gdBytes > size {
		return res, formatErr("grain directory at byte %d (%d bytes) is outside the %d-byte file", gdOff, gdBytes, size)
	}
	gd := make([]byte, gdBytes)
	if _, err := r.ReadAt(gd, gdOff); err != nil {
		return res, fmt.Errorf("read grain directory: %w", err)
	}

	grainBytes := h.GrainBytes()
	vsize := h.VirtualSize()
	gtBytes := int64(h.NumGTEsPerGT) * 4
	gt := make([]byte, gtBytes)
	grain := make([]byte, grainBytes)
	payload := make([]byte, maxCompressed(grainBytes)+SectorSize)
	var zr io.ReadCloser
	hash := digest.New(opts.DigestChunkSize)
	var next int64
	numGrains := h.NumGrains()

	for g := uint64(0); g < numGTs; g++ {
		gtSector := binary.LittleEndian.Uint32(gd[g*4:])
		if gtSector == 0 {
			continue // whole grain table unallocated
		}
		gtOff := int64(gtSector) * SectorSize
		if gtOff+gtBytes > size {
			return res, formatErr("grain table %d at byte %d is outside the file", g, gtOff)
		}
		if _, err := r.ReadAt(gt, gtOff); err != nil {
			return res, fmt.Errorf("read grain table %d: %w", g, err)
		}
		for e := uint64(0); e < uint64(h.NumGTEsPerGT); e++ {
			idx := g*uint64(h.NumGTEsPerGT) + e
			if idx >= numGrains {
				break
			}
			gte := binary.LittleEndian.Uint32(gt[e*4:])
			if gte <= 1 {
				continue // unallocated (0) or explicitly zeroed (1)
			}
			lba := idx * h.GrainSize
			markerOff := int64(gte) * SectorSize
			if markerOff+12 > size {
				return res, formatErr("grain %d marker at byte %d is outside the file", idx, markerOff)
			}
			if _, err := r.ReadAt(payload[:12], markerOff); err != nil {
				return res, err
			}
			mlba := binary.LittleEndian.Uint64(payload[0:])
			msize := binary.LittleEndian.Uint32(payload[8:])
			if mlba != lba {
				return res, formatErr("grain table entry for sector %d points to a marker for sector %d", lba, mlba)
			}
			if msize == 0 || int64(msize) > maxCompressed(grainBytes) || markerOff+12+int64(msize) > size {
				return res, formatErr("grain %d has invalid compressed size %d", idx, msize)
			}
			comp := payload[:msize]
			if _, err := r.ReadAt(comp, markerOff+12); err != nil {
				return res, err
			}
			off := int64(lba) * SectorSize
			want := min(grainBytes, vsize-off)
			n, err := inflateInto(&zr, comp, grain)
			if err != nil {
				return res, formatErr("grain at sector %d: %v", lba, err)
			}
			if int64(n) < want {
				return res, formatErr("grain at sector %d decompressed to %d bytes, want %d", lba, n, want)
			}
			data := grain[:want]
			if opts.Sink != nil {
				if err := opts.Sink.Grain(off, data); err != nil {
					return res, err
				}
			}
			hash.WriteZeros(off - next)
			_, _ = hash.Write(data)
			next = off + want
			res.Grains++
			res.AllocatedBytes += want
			if opts.OnProgress != nil {
				opts.OnProgress(Progress{StreamBytes: size, GuestOffset: next, VirtualSize: vsize, Grains: res.Grains})
			}
		}
	}
	hash.WriteZeros(vsize - next)
	res.Digest = hash.Sum()
	return res, nil
}
