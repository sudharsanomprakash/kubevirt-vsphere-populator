// Package target prepares, flushes and verifies the destination of a
// population: either a raw image file on a filesystem volume or a raw block
// device.
package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
)

// ErrCapacity is wrapped by errors caused by an undersized destination.
var ErrCapacity = errors.New("insufficient destination capacity")

// Kind is the destination type.
type Kind string

const (
	KindFile  Kind = "file"
	KindBlock Kind = "block"
)

// VerifyProgress receives the number of bytes read back so far.
type VerifyProgress func(done, total int64)

// VerifyResult describes a read-back verification.
type VerifyResult struct {
	Digest []byte `json:"-"`
	// Mode is "O_DIRECT" when the page cache was bypassed, or
	// "buffered+fadvise" when the cache was dropped with POSIX_FADV_DONTNEED
	// before a buffered read.
	Mode    string        `json:"mode"`
	Bytes   int64         `json:"bytes"`
	Elapsed time.Duration `json:"elapsed"`
}

// Destination is a population target. The worker calls Prepare, converts
// into ConvertPath, then Flush, Verify and Commit; Abort on any failure.
type Destination interface {
	Kind() Kind
	// ConvertPath is the path qemu-img writes to.
	ConvertPath() string
	// Existing reports whether ConvertPath already exists and must be
	// overwritten in place (block devices) rather than created (files).
	Existing() bool
	// Prepare checks capacity and exclusivity before any data is transferred.
	Prepare(virtualSize int64) error
	// Flush makes the converted data durable (and clears stale bytes beyond
	// the virtual size of block devices when configured).
	Flush(ctx context.Context) error
	// Verify reads back the first virtualSize bytes and returns their chunked digest.
	Verify(ctx context.Context, progress VerifyProgress) (*VerifyResult, error)
	// Commit publishes a verified result (renames files into place).
	Commit() error
	// Abort removes partial output and releases resources. It is safe to
	// call after Commit and more than once.
	Abort()
	// Close releases resources after a successful Commit.
	Close() error
}

const readChunk = 4 << 20

// readBack computes the chunked digest of the first size bytes of path,
// bypassing the page cache with O_DIRECT when possible.
func readBack(ctx context.Context, path string, size int64, progress VerifyProgress) (*VerifyResult, error) {
	start := time.Now()
	res := &VerifyResult{Bytes: size}
	d := digest.New(0)

	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECT, 0)
	direct := err == nil
	if !direct {
		f, err = os.Open(path)
		if err != nil {
			return nil, err
		}
		// Drop cached pages so that the read comes from the device.
		_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		res.Mode = "buffered+fadvise"
	} else {
		res.Mode = "O_DIRECT"
	}
	defer func() { _ = f.Close() }()

	buf := alignedBuffer(readChunk, 4096)
	var done int64
	for done < size {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		want := min(int64(len(buf)), size-done)
		if direct && want%4096 != 0 {
			// O_DIRECT needs block-aligned lengths; finish the unaligned tail
			// with a buffered read after dropping its cached pages.
			tail, err := readTail(path, done, want)
			if err != nil {
				return nil, err
			}
			_, _ = d.Write(tail)
			break
		}
		n, err := f.ReadAt(buf[:want], done)
		if int64(n) < want {
			if err == nil || errors.Is(err, io.EOF) {
				err = fmt.Errorf("short read at offset %d: got %d of %d bytes: %w", done, n, want, io.ErrUnexpectedEOF)
			}
			return nil, err
		}
		_, _ = d.Write(buf[:n])
		done += int64(n)
		if progress != nil {
			progress(done, size)
		}
	}
	res.Digest = d.Sum()
	res.Elapsed = time.Since(start)
	return res, nil
}

func readTail(path string, off, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	_ = unix.Fadvise(int(f.Fd()), off, n, unix.FADV_DONTNEED)
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, fmt.Errorf("read tail at %d: %w", off, err)
	}
	return b, nil
}

func alignedBuffer(size, align int) []byte {
	b := make([]byte, size+align)
	addr := int(uintptrOf(b))
	off := (align - addr%align) % align
	return b[off : off+size]
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func availableBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * st.Bsize, nil
}

// AvailableBytes returns the space available to unprivileged users in the
// filesystem containing dir.
func AvailableBytes(dir string) (int64, error) { return availableBytes(filepath.Clean(dir)) }
