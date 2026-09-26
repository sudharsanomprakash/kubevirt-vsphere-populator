package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// blkZeroOut is the BLKZEROOUT ioctl request (_IO(0x12, 127)).
const blkZeroOut = 0x127f

// Block writes a raw disk into a block device. The device is opened with
// O_EXCL for the duration of the population, which makes a concurrent
// O_EXCL open or mount of the same device on the same node fail.
type Block struct {
	path        string
	f           *os.File
	size        int64
	virtualSize int64
	// ZeroTail clears the bytes between the virtual disk size and the end
	// of the device so that no data from a previous user of the volume
	// remains visible to the guest.
	ZeroTail bool
	// AllowRegularFile permits a regular file in place of a block device.
	// It exists for tests only.
	AllowRegularFile bool
	// TailZeroMode records how the tail was cleared.
	TailZeroMode string
}

// NewBlock returns a block destination for the device at path.
func NewBlock(path string) *Block { return &Block{path: path, ZeroTail: true} }

func (b *Block) Kind() Kind          { return KindBlock }
func (b *Block) ConvertPath() string { return b.path }
func (b *Block) Existing() bool      { return true }
func (b *Block) DeviceSize() int64   { return b.size }
func (b *Block) String() string      { return "block device " + b.path }

// Prepare opens the device exclusively and checks its size.
func (b *Block) Prepare(virtualSize int64) error {
	flags := os.O_RDWR
	st, err := os.Stat(b.path)
	if err != nil {
		return fmt.Errorf("destination device: %w", err)
	}
	switch {
	case st.Mode()&os.ModeDevice != 0 && st.Mode()&os.ModeCharDevice == 0:
		flags |= unix.O_EXCL
	case st.Mode().IsRegular() && b.AllowRegularFile:
	default:
		return fmt.Errorf("destination %s is not a block device (mode %s)", b.path, st.Mode())
	}
	f, err := os.OpenFile(b.path, flags, 0)
	if err != nil {
		if errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("destination device %s is in use (mounted or opened exclusively by another writer): %w", b.path, err)
		}
		return fmt.Errorf("open destination device: %w", err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return fmt.Errorf("determine device size: %w", err)
	}
	if size < virtualSize {
		f.Close()
		return fmt.Errorf("%w: device has %d bytes, the disk needs %d", ErrCapacity, size, virtualSize)
	}
	b.f, b.size, b.virtualSize = f, size, virtualSize
	return nil
}

// Flush clears the tail (if configured) and flushes the device, including
// its volatile write cache.
func (b *Block) Flush(ctx context.Context) error {
	if b.f == nil {
		return errors.New("device not prepared")
	}
	if b.ZeroTail && b.size > b.virtualSize {
		mode, err := zeroRange(ctx, b.f, b.virtualSize, b.size-b.virtualSize)
		if err != nil {
			return fmt.Errorf("clear device tail: %w", err)
		}
		b.TailZeroMode = mode
	} else if b.size > b.virtualSize {
		b.TailZeroMode = "not-cleared"
	}
	if err := b.f.Sync(); err != nil {
		return fmt.Errorf("flush device: %w", err)
	}
	return nil
}

// Verify reads back the first virtualSize bytes of the device.
func (b *Block) Verify(ctx context.Context, progress VerifyProgress) (*VerifyResult, error) {
	return readBack(ctx, b.path, b.virtualSize, progress)
}

func (b *Block) Commit() error { return nil }

func (b *Block) Abort() { _ = b.Close() }

func (b *Block) Close() error {
	if b.f == nil {
		return nil
	}
	err := b.f.Close()
	b.f = nil
	return err
}

// zeroRange writes zeros over [off, off+n). Block devices use BLKZEROOUT,
// which the kernel guarantees reads back as zeros; unaligned edges, regular
// files and devices that reject the ioctl fall back to writing zero buffers.
func zeroRange(ctx context.Context, f *os.File, off, n int64) (string, error) {
	const align = 1 << 20
	mode := "write"
	end := off + n
	alignedStart := (off + align - 1) / align * align
	alignedEnd := end / align * align
	if st, err := f.Stat(); err == nil && st.Mode()&os.ModeDevice != 0 && alignedEnd > alignedStart {
		rng := [2]uint64{uint64(alignedStart), uint64(alignedEnd - alignedStart)}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), blkZeroOut, uintptr(unsafe.Pointer(&rng[0])))
		if errno == 0 {
			if err := writeZeros(ctx, f, off, alignedStart-off); err != nil {
				return "", err
			}
			if err := writeZeros(ctx, f, alignedEnd, end-alignedEnd); err != nil {
				return "", err
			}
			return "BLKZEROOUT", nil
		}
	}
	return mode, writeZeros(ctx, f, off, n)
}

func writeZeros(ctx context.Context, f *os.File, off, n int64) error {
	buf := make([]byte, min(n, 4<<20))
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		w := min(int64(len(buf)), n)
		m, err := f.WriteAt(buf[:w], off)
		if err != nil {
			return err
		}
		if int64(m) != w {
			return fmt.Errorf("short write at %d: %d of %d bytes", off, m, w)
		}
		off += w
		n -= w
	}
	return nil
}
