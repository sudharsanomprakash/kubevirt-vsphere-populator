package vmdk

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// BenchmarkDecode measures the streaming decoder (validation, inflate and
// guest digest) on in-memory streams, excluding network and disk I/O.
func BenchmarkDecode(b *testing.B) {
	for _, tc := range []struct {
		name string
		fill func([]byte)
	}{
		{"incompressible", func(p []byte) { _, _ = rand.Read(p) }},
		{"sparse", func(p []byte) {
			for i := 0; i < len(p); i += 1 << 20 {
				_, _ = rand.Read(p[i : i+4096])
			}
		}},
	} {
		raw := make([]byte, 128<<20)
		tc.fill(raw)
		stream := encode(b, raw, WriterOptions{})
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := Decode(bytes.NewReader(stream), DecodeOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
