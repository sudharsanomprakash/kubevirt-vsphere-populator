package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DefaultImageName is the file KubeVirt expects in a filesystem-mode volume.
const DefaultImageName = "disk.img"

// File writes a raw image into a directory (the mount point of a filesystem
// volume). The image is converted into "<name>.partial", flushed, verified and
// then renamed to "<name>", so the final name only ever refers to a complete
// image. Any existing image with the final name is replaced; its old bytes
// are never reused because the partial file is always newly created.
type File struct {
	dir, name   string
	virtualSize int64
	lock        *os.File
	finalized   bool
	// FileMode is applied to the published image (default 0660).
	FileMode os.FileMode
}

// NewFile returns a file destination in dir.
func NewFile(dir, name string) *File {
	if name == "" {
		name = DefaultImageName
	}
	return &File{dir: filepath.Clean(dir), name: name, FileMode: 0o660}
}

func (f *File) Kind() Kind          { return KindFile }
func (f *File) partial() string     { return filepath.Join(f.dir, f.name+".partial") }
func (f *File) final() string       { return filepath.Join(f.dir, f.name) }
func (f *File) ConvertPath() string { return f.partial() }
func (f *File) FinalPath() string   { return f.final() }
func (f *File) lockPath() string    { return filepath.Join(f.dir, ".vsphere-populator.lock") }
func (f *File) VirtualSize() int64  { return f.virtualSize }
func (f *File) String() string      { return "file " + f.final() }
func (f *File) Close() error        { return f.unlock() }
func (f *File) unlock() error {
	if f.lock == nil {
		return nil
	}
	err := f.lock.Close()
	f.lock = nil
	return err
}

// Prepare takes an exclusive lock on the volume, removes stale partial output
// and checks that the filesystem can hold a fully allocated image.
func (f *File) Prepare(virtualSize int64) error {
	st, err := os.Stat(f.dir)
	if err != nil {
		return fmt.Errorf("destination directory: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("destination %s is not a directory", f.dir)
	}
	lock, err := os.OpenFile(f.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("destination volume is locked by another writer: %w", err)
	}
	f.lock = lock
	if err := os.Remove(f.partial()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale partial image: %w", err)
	}
	avail, err := availableBytes(f.dir)
	if err != nil {
		return fmt.Errorf("statfs destination: %w", err)
	}
	// An existing image is only replaced at the end, so both may coexist.
	if avail < virtualSize {
		return fmt.Errorf("%w: filesystem has %d bytes available, the disk needs %d", ErrCapacity, avail, virtualSize)
	}
	f.virtualSize = virtualSize
	return nil
}

// Existing is false: qemu-img creates the partial image.
func (f *File) Existing() bool { return false }

// Flush checks the converted file size and flushes it to stable storage.
func (f *File) Flush(ctx context.Context) error {
	p, err := os.OpenFile(f.partial(), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open converted image: %w", err)
	}
	defer func() { _ = p.Close() }()
	st, err := p.Stat()
	if err != nil {
		return err
	}
	if st.Size() != f.virtualSize {
		return fmt.Errorf("converted image is %d bytes, want %d", st.Size(), f.virtualSize)
	}
	if err := p.Chmod(f.FileMode); err != nil {
		return fmt.Errorf("chmod image: %w", err)
	}
	if err := p.Sync(); err != nil {
		return fmt.Errorf("fsync image: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Verify reads back the partial image (before it is published).
func (f *File) Verify(ctx context.Context, progress VerifyProgress) (*VerifyResult, error) {
	return readBack(ctx, f.partial(), f.virtualSize, progress)
}

// Commit renames the verified image to its final name and flushes the directory.
func (f *File) Commit() error {
	if err := os.Rename(f.partial(), f.final()); err != nil {
		return fmt.Errorf("publish image: %w", err)
	}
	if err := fsyncDir(f.dir); err != nil {
		return fmt.Errorf("fsync destination directory: %w", err)
	}
	f.finalized = true
	return nil
}

// Abort removes partial output and releases the lock.
func (f *File) Abort() {
	if !f.finalized {
		_ = os.Remove(f.partial())
	}
	_ = f.unlock()
}
