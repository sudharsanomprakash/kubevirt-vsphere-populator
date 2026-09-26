package target_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/convert"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/target"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/testutil"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vmdk"
)

// sparseRaw builds a disk where most grains are zero, some are data, and some
// single sectors inside otherwise-zero grains are set.
func sparseRaw(size int64, seed uint64) []byte {
	raw := make([]byte, size)
	rng := rand.New(rand.NewPCG(seed, 99))
	for off := int64(0); off < size; off += 64 * 1024 {
		switch rng.IntN(5) {
		case 0:
			end := min(off+64*1024, size)
			for i := off; i < end; i++ {
				raw[i] = byte(rng.Uint32())
			}
		case 1:
			s := off + int64(rng.IntN(128))*512
			if s+512 <= size {
				copy(raw[s:], bytes.Repeat([]byte{0x5a}, 512))
			}
		}
	}
	return raw
}

func stage(t *testing.T, raw []byte) (string, *vmdk.Result) {
	t.Helper()
	var buf bytes.Buffer
	if _, err := vmdk.WriteStreamOptimized(&buf, bytes.NewReader(raw), int64(len(raw)), vmdk.WriterOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := vmdk.Decode(bytes.NewReader(buf.Bytes()), vmdk.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "staged.vmdk")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, res
}

func populate(t *testing.T, dst target.Destination, raw []byte) *target.VerifyResult {
	t.Helper()
	ctx := context.Background()
	q, err := convert.Find(ctx, testutil.RequireQemuImg(t))
	if err != nil {
		t.Fatal(err)
	}
	staged, res := stage(t, raw)
	if _, err := q.CheckStaged(ctx, staged, int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	if err := dst.Prepare(int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	defer dst.Abort()
	opts := convert.ConvertOptions{Target: convert.NewFile}
	if dst.Existing() {
		opts.Target = convert.ExistingDevice
	}
	var lastPct float64
	opts.Progress = func(p float64) { lastPct = p }
	if err := q.Convert(ctx, staged, dst.ConvertPath(), opts); err != nil {
		t.Fatal(err)
	}
	if lastPct != 100 {
		t.Errorf("last qemu-img progress %.2f, want 100", lastPct)
	}
	if err := dst.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	vr, err := dst.Verify(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vr.Digest, res.Digest) {
		t.Fatalf("destination digest differs from decoded source digest")
	}
	if err := dst.Commit(); err != nil {
		t.Fatal(err)
	}
	return vr
}

func fill(n int64, b byte) []byte { return bytes.Repeat([]byte{b}, int(n)) }

func TestFileTargetReplacesPrefilledImage(t *testing.T) {
	dir := t.TempDir()
	raw := sparseRaw(5*1024*1024+3*512, 1)
	// A previous, larger image full of 0xFF and a stale partial file.
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), fill(8<<20, 0xff), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img.partial"), fill(1<<20, 0xee), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := target.NewFile(dir, "")
	populate(t, dst, raw)
	got, err := os.ReadFile(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("published image differs from the source raw disk")
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.img.partial")); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}
	st, _ := os.Stat(filepath.Join(dir, "disk.img"))
	if st.Mode().Perm() != 0o660 {
		t.Fatalf("image mode %v", st.Mode())
	}
	_ = dst.Close()
}

func TestFileTargetIsSparse(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, 64<<20)
	copy(raw[10<<20:], fill(4096, 1))
	dst := target.NewFile(dir, "")
	populate(t, dst, raw)
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "disk.img"), &st); err != nil {
		t.Fatal(err)
	}
	if alloc := st.Blocks * 512; alloc > 8<<20 {
		t.Logf("note: image allocates %d bytes (filesystem may not support holes)", alloc)
	}
}

func TestFileTargetLock(t *testing.T) {
	dir := t.TempDir()
	a := target.NewFile(dir, "")
	if err := a.Prepare(1 << 20); err != nil {
		t.Fatal(err)
	}
	defer a.Abort()
	b := target.NewFile(dir, "")
	if err := b.Prepare(1 << 20); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("second writer not rejected: %v", err)
	}
}

func TestFileTargetCapacity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount a small tmpfs")
	}
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "size=4m"); err != nil {
		t.Skipf("cannot mount tmpfs: %v", err)
	}
	defer unix.Unmount(dir, 0)
	dst := target.NewFile(dir, "")
	err := dst.Prepare(16 << 20)
	if !errors.Is(err, target.ErrCapacity) {
		t.Fatalf("expected capacity error, got %v", err)
	}
	dst.Abort()
}

func TestFileTargetAbortRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	dst := target.NewFile(dir, "")
	if err := dst.Prepare(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst.ConvertPath(), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst.Abort()
	if _, err := os.Stat(dst.ConvertPath()); !os.IsNotExist(err) {
		t.Fatal("partial not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.img")); !os.IsNotExist(err) {
		t.Fatal("final image must not exist after abort")
	}
}

func TestFileTargetRejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	dst := target.NewFile(dir, "")
	if err := dst.Prepare(2 << 20); err != nil {
		t.Fatal(err)
	}
	defer dst.Abort()
	if err := os.WriteFile(dst.ConvertPath(), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dst.Flush(context.Background()); err == nil {
		t.Fatal("size mismatch accepted")
	}
}

func TestBlockTargetPrefilledRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	raw := sparseRaw(3*1024*1024+512, 2)
	devSize := int64(6 << 20)
	if err := os.WriteFile(path, fill(devSize, 0xff), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := target.NewBlock(path)
	dst.AllowRegularFile = true
	populate(t, dst, raw)
	_ = dst.Close()
	got, _ := os.ReadFile(path)
	if int64(len(got)) != devSize {
		t.Fatalf("device size changed to %d", len(got))
	}
	if !bytes.Equal(got[:len(raw)], raw) {
		t.Fatal("zero regions of the source were not cleared on the prefilled target")
	}
	if !digest.IsZero(got[len(raw):]) {
		t.Fatal("tail beyond the virtual size still holds old bytes")
	}
}

func TestBlockTargetTooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := target.NewBlock(path)
	dst.AllowRegularFile = true
	if err := dst.Prepare(2 << 20); !errors.Is(err, target.ErrCapacity) {
		t.Fatalf("expected capacity error, got %v", err)
	}
}

func TestBlockTargetRejectsRegularFileByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := target.NewBlock(path).Prepare(1 << 20); err == nil {
		t.Fatal("regular file accepted as block device")
	}
}

// loopDevice attaches a loop device backed by a file filled with fillByte.
func loopDevice(t *testing.T, size int64, fillByte byte) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("loop devices need root")
	}
	if _, err := exec.LookPath("losetup"); err != nil {
		t.Skip("losetup not installed")
	}
	backing := filepath.Join(t.TempDir(), "backing.img")
	if err := os.WriteFile(backing, fill(size, fillByte), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("losetup", "--find", "--show", backing).Output()
	if err != nil {
		t.Skipf("cannot attach loop device: %v", err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", dev).Run() })
	return dev
}

func TestBlockTargetLoopDevicePrefilled(t *testing.T) {
	testutil.RequireQemuImg(t)
	devSize := int64(12 << 20)
	dev := loopDevice(t, devSize, 0xff)
	raw := sparseRaw(9*1024*1024+7*512, 3)
	dst := target.NewBlock(dev)
	vr := populate(t, dst, raw)
	t.Logf("verify mode %s, tail zero mode %s", vr.Mode, dst.TailZeroMode)
	_ = dst.Close()
	f, err := os.Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	got := make([]byte, devSize)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:len(raw)], raw) {
		t.Fatal("device content differs from source")
	}
	if !digest.IsZero(got[len(raw):]) {
		t.Fatal("device tail not cleared")
	}
}

func TestBlockTargetExclusive(t *testing.T) {
	dev := loopDevice(t, 4<<20, 0)
	a := target.NewBlock(dev)
	if err := a.Prepare(1 << 20); err != nil {
		t.Fatal(err)
	}
	defer a.Abort()
	b := target.NewBlock(dev)
	if err := b.Prepare(1 << 20); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second exclusive open not rejected: %v", err)
	}
}
