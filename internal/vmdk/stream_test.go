package vmdk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
)

// makeRaw returns a pseudo-random raw disk image of the given size in which
// only some regions contain data, so that both allocated and unallocated
// grains appear in the encoded stream.
func makeRaw(t testing.TB, size int64, seed uint64) []byte {
	t.Helper()
	raw := make([]byte, size)
	rng := rand.New(rand.NewPCG(seed, seed+1))
	for off := int64(0); off < size; off += 64 * 1024 {
		switch rng.IntN(4) {
		case 0: // leave zero
		case 1: // random data
			end := min(off+64*1024, size)
			for i := off; i < end; i++ {
				raw[i] = byte(rng.Uint32())
			}
		case 2: // a single non-zero sector somewhere in the grain
			s := off + int64(rng.IntN(128))*512
			if s+512 <= size {
				for i := s; i < s+512; i++ {
					raw[i] = 0xA5
				}
			}
		case 3: // compressible pattern
			end := min(off+64*1024, size)
			for i := off; i < end; i++ {
				raw[i] = byte(i / 4096)
			}
		}
	}
	return raw
}

func encode(t testing.TB, raw []byte, opts WriterOptions) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := WriteStreamOptimized(&buf, bytes.NewReader(raw), int64(len(raw)), opts); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func rawDigest(raw []byte) []byte {
	d := digest.New(0)
	_, _ = d.Write(raw)
	return d.Sum()
}

type memSink struct{ img []byte }

func (m *memSink) Grain(off int64, data []byte) error {
	copy(m.img[off:], data)
	return nil
}

func TestRoundTripSizes(t *testing.T) {
	sizes := []int64{
		512,                    // a single sector
		64 * 1024,              // exactly one grain
		64*1024 + 512,          // one grain plus one sector (odd size)
		3*1024*1024 + 7*512,    // not a multiple of the grain size
		32*1024*1024 + 64*1024, // spans more than one grain table (512 grains = 32 MiB)
	}
	for i, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			raw := makeRaw(t, size, uint64(i)+1)
			stream := encode(t, raw, WriterOptions{})
			sink := &memSink{img: make([]byte, size)}
			var last Progress
			res, err := Decode(bytes.NewReader(stream), DecodeOptions{Sink: sink, OnProgress: func(p Progress) { last = p }})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !res.Ordered || res.Digest == nil {
				t.Fatalf("expected ordered stream with digest")
			}
			if !bytes.Equal(res.Digest, rawDigest(raw)) {
				t.Fatalf("digest mismatch")
			}
			if !bytes.Equal(sink.img, raw) {
				t.Fatalf("sink reconstruction mismatch")
			}
			if res.VirtualSize() != size {
				t.Fatalf("virtual size %d, want %d", res.VirtualSize(), size)
			}
			if res.StreamBytes != int64(len(stream)) || last.StreamBytes != int64(len(stream)) {
				t.Fatalf("stream bytes %d/%d, want %d", res.StreamBytes, last.StreamBytes, len(stream))
			}
			// The random-access decoder must agree.
			ra, err := DecodeAt(bytes.NewReader(stream), int64(len(stream)), *res.Footer, DecodeOptions{})
			if err != nil {
				t.Fatalf("DecodeAt: %v", err)
			}
			if !bytes.Equal(ra.Digest, res.Digest) || ra.Grains != res.Grains {
				t.Fatalf("DecodeAt disagrees: grains %d vs %d", ra.Grains, res.Grains)
			}
		})
	}
}

func TestAllZeroDisk(t *testing.T) {
	raw := make([]byte, 5*1024*1024)
	stream := encode(t, raw, WriterOptions{})
	res, err := Decode(bytes.NewReader(stream), DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Grains != 0 || res.AllocatedBytes != 0 {
		t.Fatalf("expected no grains, got %d", res.Grains)
	}
	if !bytes.Equal(res.Digest, rawDigest(raw)) {
		t.Fatal("digest of all-zero disk mismatch")
	}
	// Explicit zero grains must produce the same digest.
	res2, err := Decode(bytes.NewReader(encode(t, raw, WriterOptions{KeepZeroGrains: true})), DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res2.Digest, res.Digest) || res2.Grains != 80 {
		t.Fatalf("explicit zero grains: grains=%d", res2.Grains)
	}
}

func TestShuffledGrainsRequireRandomAccessPass(t *testing.T) {
	raw := makeRaw(t, 8*1024*1024, 42)
	stream := encode(t, raw, WriterOptions{ShuffleSeed: 7, KeepZeroGrains: true})
	sink := &memSink{img: make([]byte, len(raw))}
	res, err := Decode(bytes.NewReader(stream), DecodeOptions{Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ordered || res.Digest != nil {
		t.Fatal("shuffled stream must be reported as unordered without a digest")
	}
	if !bytes.Equal(sink.img, raw) {
		t.Fatal("sink reconstruction mismatch for shuffled stream")
	}
	ra, err := DecodeAt(bytes.NewReader(stream), int64(len(stream)), *res.Footer, DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ra.Digest, rawDigest(raw)) {
		t.Fatal("random-access digest mismatch")
	}
}

func TestTruncatedStreamsFail(t *testing.T) {
	raw := makeRaw(t, 2*1024*1024, 3)
	stream := encode(t, raw, WriterOptions{})
	for _, cut := range []int{0, 100, 512, 1024, 64 * 1024, 64*1024 + 13, len(stream) / 2, len(stream) - 1024, len(stream) - 512, len(stream) - 1} {
		_, err := Decode(bytes.NewReader(stream[:cut]), DecodeOptions{})
		if err == nil {
			t.Fatalf("truncation at %d of %d accepted", cut, len(stream))
		}
		if !errors.Is(err, ErrFormat) {
			t.Fatalf("truncation at %d: error %v does not wrap ErrFormat", cut, err)
		}
	}
}

func TestTrailingData(t *testing.T) {
	raw := makeRaw(t, 1024*1024, 5)
	stream := encode(t, raw, WriterOptions{})
	padded := append(append([]byte{}, stream...), make([]byte, 4096)...)
	res, err := Decode(bytes.NewReader(padded), DecodeOptions{})
	if err != nil {
		t.Fatalf("zero padding rejected: %v", err)
	}
	if res.TrailingBytes != 4096 {
		t.Fatalf("trailing bytes %d", res.TrailingBytes)
	}
	garbage := append(append([]byte{}, stream...), []byte("extra")...)
	if _, err := Decode(bytes.NewReader(garbage), DecodeOptions{}); err == nil {
		t.Fatal("non-zero trailing data accepted")
	}
}

func TestMissingFooterRejected(t *testing.T) {
	raw := makeRaw(t, 1024*1024, 9)
	if _, err := Decode(bytes.NewReader(encode(t, raw, WriterOptions{OmitFooter: true})), DecodeOptions{}); err == nil {
		t.Fatal("stream without footer accepted")
	}
}

func TestWrongCreateTypeRejected(t *testing.T) {
	raw := makeRaw(t, 1024*1024, 9)
	_, err := Decode(bytes.NewReader(encode(t, raw, WriterOptions{CreateType: "monolithicSparse"})), DecodeOptions{})
	if err == nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("wrong createType: %v", err)
	}
}

func TestOnHeaderAbortsEarly(t *testing.T) {
	raw := makeRaw(t, 4*1024*1024, 11)
	stream := encode(t, raw, WriterOptions{})
	sentinel := errors.New("capacity mismatch")
	r := &countingReader{r: bytes.NewReader(stream)}
	_, err := Decode(r, DecodeOptions{OnHeader: func(h Header, d Descriptor) error {
		if h.VirtualSize() != int64(len(raw)) || d.CreateType != "streamOptimized" {
			t.Errorf("unexpected header %+v", h)
		}
		return sentinel
	}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
	if r.n >= int64(len(stream)) {
		t.Fatalf("decoder consumed the whole stream (%d bytes) before OnHeader aborted", r.n)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// handStream assembles a stream from explicit grain markers so that malformed
// grain sequences can be expressed.
type handGrain struct {
	lba        uint64
	compressed []byte // used verbatim when non-nil
	data       []byte // compressed with zlib otherwise
}

func handStream(t *testing.T, capacity uint64, grains []handGrain) []byte {
	t.Helper()
	h := Header{Magic: Magic, Version: 3, Flags: FlagValidNewlineTest | FlagCompressed | FlagMarkers,
		Capacity: capacity, GrainSize: 128, DescriptorOffset: 1, DescriptorSize: 1, NumGTEsPerGT: 512,
		GDOffset: GDAtEnd, OverHead: 128, SingleEndLineChar: '\n', NonEndLineChar: ' ',
		DoubleEndLineChar1: '\r', DoubleEndLineChar2: '\n', CompressAlgorithm: CompressionDeflate}
	var b bytes.Buffer
	pad := func() {
		if r := b.Len() % SectorSize; r != 0 {
			b.Write(make([]byte, SectorSize-r))
		}
	}
	b.Write(h.Marshal())
	fmt.Fprintf(&b, "# Disk DescriptorFile\nversion=1\nCID=1\nparentCID=ffffffff\ncreateType=\"streamOptimized\"\nRDONLY %d SPARSE \"x.vmdk\"\n", capacity)
	pad()
	b.Write(make([]byte, 126*SectorSize))
	for _, g := range grains {
		c := g.compressed
		if c == nil {
			var z bytes.Buffer
			zw := zlib.NewWriter(&z)
			_, _ = zw.Write(g.data)
			_ = zw.Close()
			c = z.Bytes()
		}
		var m [12]byte
		binary.LittleEndian.PutUint64(m[0:], g.lba)
		binary.LittleEndian.PutUint32(m[8:], uint32(len(c)))
		b.Write(m[:])
		b.Write(c)
		pad()
	}
	meta := func(typ uint32, sectors uint64) {
		var s [SectorSize]byte
		binary.LittleEndian.PutUint64(s[0:], sectors)
		binary.LittleEndian.PutUint32(s[12:], typ)
		b.Write(s[:])
	}
	gdSector := uint64(b.Len()/SectorSize) + 1
	meta(MarkerGD, 1)
	b.Write(make([]byte, SectorSize))
	meta(MarkerFooter, 1)
	f := h
	f.GDOffset = gdSector
	b.Write(f.Marshal())
	meta(MarkerEOS, 0)
	return b.Bytes()
}

func TestMalformedGrains(t *testing.T) {
	grain := bytes.Repeat([]byte{1}, 64*1024)
	valid := handStream(t, 1024, []handGrain{{lba: 0, data: grain}, {lba: 128, data: grain}})
	if _, err := Decode(bytes.NewReader(valid), DecodeOptions{}); err != nil {
		t.Fatalf("hand-built valid stream rejected: %v", err)
	}
	corrupt := func() []byte {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		_, _ = zw.Write(grain)
		_ = zw.Close()
		c := z.Bytes()
		c[len(c)-1] ^= 0xff // break the Adler-32 checksum
		return c
	}()
	cases := map[string][]handGrain{
		"duplicate":        {{lba: 0, data: grain}, {lba: 0, data: grain}},
		"misaligned":       {{lba: 3, data: grain}},
		"beyond capacity":  {{lba: 1024, data: grain}},
		"short grain":      {{lba: 0, data: grain[:1000]}},
		"oversized grain":  {{lba: 0, data: append(append([]byte{}, grain...), 7)}},
		"bad checksum":     {{lba: 0, compressed: corrupt}},
		"not zlib":         {{lba: 0, compressed: []byte("definitely not deflate data")}},
		"huge size claim":  {{lba: 0, compressed: make([]byte, 300*1024)}},
		"descending order": {{lba: 128, data: grain}, {lba: 0, data: grain}},
	}
	for name, gs := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := Decode(bytes.NewReader(handStream(t, 1024, gs)), DecodeOptions{})
			if name == "descending order" {
				// Valid but unordered: no streaming digest.
				if err != nil || res.Ordered || res.Digest != nil {
					t.Fatalf("descending order: err=%v ordered=%v", err, res.Ordered)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s accepted", name)
			}
			if !errors.Is(err, ErrFormat) {
				t.Fatalf("%s: %v does not wrap ErrFormat", name, err)
			}
		})
	}
}

func TestLastPartialGrain(t *testing.T) {
	// Capacity of 200 sectors: the second grain covers only 72 sectors. Both a
	// full padded grain and an exact partial grain are accepted.
	g0 := bytes.Repeat([]byte{2}, 64*1024)
	partial := bytes.Repeat([]byte{3}, 72*512)
	padded := append(append([]byte{}, partial...), make([]byte, 64*1024-len(partial))...)
	for name, data := range map[string][]byte{"exact": partial, "padded": padded} {
		res, err := Decode(bytes.NewReader(handStream(t, 200, []handGrain{{lba: 0, data: g0}, {lba: 128, data: data}})), DecodeOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := append(append([]byte{}, g0...), partial...)
		if !bytes.Equal(res.Digest, rawDigest(want)) {
			t.Fatalf("%s: digest mismatch", name)
		}
	}
}

func TestHeaderValidation(t *testing.T) {
	base := Header{Magic: Magic, Version: 3, Flags: FlagValidNewlineTest | FlagCompressed | FlagMarkers,
		Capacity: 2048, GrainSize: 128, DescriptorOffset: 1, DescriptorSize: 1, NumGTEsPerGT: 512,
		GDOffset: GDAtEnd, OverHead: 128, SingleEndLineChar: '\n', NonEndLineChar: ' ',
		DoubleEndLineChar1: '\r', DoubleEndLineChar2: '\n', CompressAlgorithm: CompressionDeflate}
	if err := base.ValidateStreamOptimized(); err != nil {
		t.Fatal(err)
	}
	mut := map[string]func(*Header){
		"version":      func(h *Header) { h.Version = 4 },
		"uncompressed": func(h *Header) { h.Flags &^= FlagCompressed },
		"no markers":   func(h *Header) { h.Flags &^= FlagMarkers },
		"algorithm":    func(h *Header) { h.CompressAlgorithm = 2 },
		"newline":      func(h *Header) { h.DoubleEndLineChar1 = '\n' },
		"capacity 0":   func(h *Header) { h.Capacity = 0 },
		"capacity big": func(h *Header) { h.Capacity = MaxCapacitySectors + 1 },
		"grain odd":    func(h *Header) { h.GrainSize = 100 },
		"grain small":  func(h *Header) { h.GrainSize = 4 },
		"gtes":         func(h *Header) { h.NumGTEsPerGT = 128 },
		"overhead":     func(h *Header) { h.OverHead = 1 },
	}
	for name, m := range mut {
		h := base
		m(&h)
		if err := h.ValidateStreamOptimized(); err == nil {
			t.Errorf("%s: invalid header accepted", name)
		}
	}
	b := base.Marshal()
	b[0] = 'X'
	if _, err := ParseHeader(b); err == nil {
		t.Error("bad magic accepted")
	}
	back, err := ParseHeader(base.Marshal())
	if err != nil || back != base {
		t.Fatalf("header round trip: %v %+v", err, back)
	}
}

func FuzzDecode(f *testing.F) {
	raw := makeRaw(f, 256*1024, 1)
	f.Add(encode(f, raw, WriterOptions{}))
	f.Add(encode(f, raw, WriterOptions{ShuffleSeed: 3}))
	f.Add([]byte("KDMV"))
	f.Fuzz(func(t *testing.T, data []byte) {
		res, err := Decode(bytes.NewReader(data), DecodeOptions{})
		if err == nil && res.Footer != nil {
			_, _ = DecodeAt(bytes.NewReader(data), int64(len(data)), *res.Footer, DecodeOptions{})
		}
	})
}
