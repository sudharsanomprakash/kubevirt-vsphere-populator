package digest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// reference computes the digest directly from its definition.
func reference(data []byte, chunk int) []byte {
	outer := sha256.New()
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		s := sha256.Sum256(data[off:end])
		outer.Write(s[:])
	}
	f := sha256.New()
	f.Write([]byte(domain))
	var hdr [16]byte
	binary.BigEndian.PutUint64(hdr[0:], uint64(len(data)))
	binary.BigEndian.PutUint64(hdr[8:], uint64(chunk))
	f.Write(hdr[:])
	f.Write(outer.Sum(nil))
	return f.Sum(nil)
}

func TestMatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{0, 1, 4095, 4096, 4097, 3*4096 + 17} {
		data := make([]byte, size)
		for i := range data {
			if rng.IntN(3) == 0 {
				data[i] = byte(rng.Uint32())
			}
		}
		// Feed in irregular pieces, with zero runs through WriteZeros.
		d := New(4096)
		for off := 0; off < size; {
			n := min(1+rng.IntN(7000), size-off)
			if IsZero(data[off : off+n]) {
				d.WriteZeros(int64(n))
			} else {
				_, _ = d.Write(data[off : off+n])
			}
			off += n
		}
		if !bytes.Equal(d.Sum(), reference(data, 4096)) {
			t.Fatalf("size %d: digest differs from definition", size)
		}
		r, err := Reader(bytes.NewReader(data), int64(size), 4096)
		if err != nil || !bytes.Equal(r, reference(data, 4096)) {
			t.Fatalf("size %d: Reader mismatch (%v)", size, err)
		}
	}
}

func TestZeroRunsEquivalent(t *testing.T) {
	a := New(0)
	a.WriteZeros(10<<20 + 3)
	b := New(0)
	_, _ = b.Write(make([]byte, 10<<20+3))
	if !bytes.Equal(a.Sum(), b.Sum()) {
		t.Fatal("WriteZeros differs from writing zeros")
	}
}

func TestLengthMatters(t *testing.T) {
	a := New(0)
	a.WriteZeros(1 << 20)
	b := New(0)
	b.WriteZeros(1<<20 + 512)
	if bytes.Equal(a.Sum(), b.Sum()) {
		t.Fatal("different lengths must give different digests")
	}
}

func TestReaderShort(t *testing.T) {
	if _, err := Reader(bytes.NewReader(make([]byte, 10)), 11, 0); err == nil {
		t.Fatal("short input accepted")
	}
}
