// Package digest implements the chunked SHA-256 digest used to compare a
// decoded guest disk with the bytes read back from a destination volume.
//
// The digest of a byte sequence of length N is defined as:
//
//	SHA-256( "kvsp-chunked-sha256-v1\x00" || uint64be(N) || uint64be(C) ||
//	         SHA-256(chunk_0) || SHA-256(chunk_1) || ... )
//
// where C is the chunk size (1 MiB by default) and chunk_i is the i-th
// C-byte slice of the sequence (the final chunk may be shorter). Hashing each
// chunk independently lets producers skip the SHA-256 computation for
// all-zero chunks (the digest of a zero chunk is a constant) without changing
// the result, which matters for large sparse disks. The construction is not a
// plain SHA-256 of the image; use Sum over a whole image (for example with the
// worker "digest" subcommand) to reproduce it independently.
package digest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
)

// DefaultChunkSize is the chunk size used by the worker.
const DefaultChunkSize = 1 << 20

const domain = "kvsp-chunked-sha256-v1\x00"

// Chunked computes the chunked SHA-256 digest. It is not safe for concurrent use.
type Chunked struct {
	chunkSize  int
	outer      hash.Hash
	inner      hash.Hash
	buf        []byte // pending bytes of the current chunk
	zeroDigest [sha256.Size]byte
	zeroBuf    []byte
	total      uint64
	sum        []byte
}

// New returns a digest with the given chunk size. The declared total size must
// equal the number of bytes written before Sum is called.
func New(chunkSize int) *Chunked {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	c := &Chunked{
		chunkSize: chunkSize,
		outer:     sha256.New(),
		inner:     sha256.New(),
		buf:       make([]byte, 0, chunkSize),
		zeroBuf:   make([]byte, chunkSize),
	}
	c.zeroDigest = sha256.Sum256(c.zeroBuf)
	return c
}

// Size returns the number of bytes consumed so far.
func (c *Chunked) Size() uint64 { return c.total }

// Write adds p to the digest. It never returns an error.
func (c *Chunked) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if len(c.buf) == 0 && len(p) >= c.chunkSize {
			c.emitChunk(p[:c.chunkSize])
			p = p[c.chunkSize:]
			continue
		}
		take := min(c.chunkSize-len(c.buf), len(p))
		c.buf = append(c.buf, p[:take]...)
		p = p[take:]
		if len(c.buf) == c.chunkSize {
			c.emitChunk(c.buf)
			c.buf = c.buf[:0]
		}
	}
	c.total += uint64(n)
	return n, nil
}

// WriteZeros adds n zero bytes to the digest without hashing full zero chunks.
func (c *Chunked) WriteZeros(n int64) {
	for n > 0 {
		if len(c.buf) == 0 && n >= int64(c.chunkSize) {
			c.outer.Write(c.zeroDigest[:])
			c.total += uint64(c.chunkSize)
			n -= int64(c.chunkSize)
			continue
		}
		take := min(int64(c.chunkSize-len(c.buf)), n)
		_, _ = c.Write(c.zeroBuf[:take])
		n -= take
	}
}

func (c *Chunked) emitChunk(chunk []byte) {
	if len(chunk) == c.chunkSize && isZero(chunk) {
		c.outer.Write(c.zeroDigest[:])
		return
	}
	c.inner.Reset()
	c.inner.Write(chunk)
	c.outer.Write(c.inner.Sum(nil))
}

// Sum finalizes the digest. Further writes are not allowed.
func (c *Chunked) Sum() []byte {
	if c.sum != nil {
		return c.sum
	}
	if len(c.buf) > 0 {
		c.emitChunk(c.buf)
		c.buf = c.buf[:0]
	}
	final := sha256.New()
	final.Write([]byte(domain))
	var hdr [16]byte
	binary.BigEndian.PutUint64(hdr[0:8], c.total)
	binary.BigEndian.PutUint64(hdr[8:16], uint64(c.chunkSize))
	final.Write(hdr[:])
	final.Write(c.outer.Sum(nil))
	c.sum = final.Sum(nil)
	return c.sum
}

// SumHex returns the hex encoding of Sum.
func (c *Chunked) SumHex() string { return hex.EncodeToString(c.Sum()) }

// Reader computes the chunked digest of exactly size bytes read from r.
// It returns io.ErrUnexpectedEOF if r ends early.
func Reader(r io.Reader, size int64, chunkSize int) ([]byte, error) {
	c := New(chunkSize)
	buf := make([]byte, c.chunkSize)
	remaining := size
	for remaining > 0 {
		want := int(min(int64(len(buf)), remaining))
		n, err := io.ReadFull(r, buf[:want])
		_, _ = c.Write(buf[:n])
		remaining -= int64(n)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return c.Sum(), nil
}

// isZero reports whether b contains only zero bytes.
func isZero(b []byte) bool {
	// Compare eight bytes at a time; the tail is handled bytewise.
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if binary.LittleEndian.Uint64(b[i:]) != 0 {
			return false
		}
	}
	for ; i < len(b); i++ {
		if b[i] != 0 {
			return false
		}
	}
	return true
}

// IsZero reports whether b contains only zero bytes.
func IsZero(b []byte) bool { return isZero(b) }
