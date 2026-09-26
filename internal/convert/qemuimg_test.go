package convert

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/testutil"
)

func TestArgs(t *testing.T) {
	dev := Args("in.vmdk", "/dev/x", ConvertOptions{Target: ExistingDevice, Coroutines: 4})
	file := Args("in.vmdk", "/v/disk.img.partial", ConvertOptions{Target: NewFile})
	for _, a := range [][]string{dev, file} {
		// qemu-img must never be told that the target already reads as
		// zeros: a reused volume would keep stale bytes where the source has zeros.
		if slices.Contains(a, "--target-is-zero") {
			t.Fatalf("unsafe flag in %v", a)
		}
		// The source format is always forced; no probing of untrusted input.
		if i := slices.Index(a, "-f"); i < 0 || a[i+1] != "vmdk" {
			t.Fatalf("source format not forced: %v", a)
		}
		if i := slices.Index(a, "-O"); i < 0 || a[i+1] != "raw" {
			t.Fatalf("output format not raw: %v", a)
		}
	}
	if !slices.Contains(dev, "-n") || slices.Contains(file, "-n") {
		t.Fatalf("-n must be used only for existing devices: %v / %v", dev, file)
	}
}

func TestFindAndErrors(t *testing.T) {
	testutil.RequireQemuImg(t)
	ctx := context.Background()
	q, err := Find(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if q.Version == "" {
		t.Fatal("empty version")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.vmdk")
	if err := os.WriteFile(bad, []byte("not a vmdk at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CheckStaged(ctx, bad, 512); err == nil {
		t.Fatal("garbage accepted by CheckStaged")
	}
	err = q.Convert(ctx, bad, filepath.Join(dir, "out.raw"), ConvertOptions{})
	if err == nil || !strings.Contains(err.Error(), "exited with status") {
		t.Fatalf("expected qemu-img failure, got %v", err)
	}
	if _, err := Find(ctx, filepath.Join(dir, "missing-qemu-img")); err == nil {
		t.Fatal("missing binary accepted")
	}
}

func TestConvertCancellation(t *testing.T) {
	testutil.RequireQemuImg(t)
	// A fake qemu-img that never finishes stands in for a hung conversion.
	dir := t.TempDir()
	fake := filepath.Join(dir, "qemu-img")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	q := &QemuImg{Path: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := q.Convert(ctx, "a", "b", ConvertOptions{})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected cancellation error, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancellation did not stop the subprocess promptly")
	}
}

func TestProgressParsing(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "qemu-img")
	script := "#!/bin/sh\nprintf '    (0.00/100%%)\\r    (50.00/100%%)\\r    (100.00/100%%)\\r\\n'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var got []float64
	q := &QemuImg{Path: fake}
	if err := q.Convert(context.Background(), "a", "b", ConvertOptions{Progress: func(p float64) { got = append(got, p) }}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []float64{0, 50, 100}) {
		t.Fatalf("progress %v", got)
	}
}
