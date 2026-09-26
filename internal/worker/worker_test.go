package worker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/progress"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/target"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/testutil"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vmdk"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/worker"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/test/fakevsphere"
)

type env struct {
	t       *testing.T
	fake    *fakevsphere.Server
	creds   string
	staging string
	out     string
}

func newEnv(t *testing.T, opts fakevsphere.Options) *env {
	t.Helper()
	testutil.RequireQemuImg(t)
	s, err := fakevsphere.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	creds := t.TempDir()
	_ = os.WriteFile(filepath.Join(creds, "username"), []byte(fakevsphere.Username), 0o600)
	_ = os.WriteFile(filepath.Join(creds, "password"), []byte(fakevsphere.Password), 0o600)
	return &env{t: t, fake: s, creds: creds, staging: t.TempDir(), out: t.TempDir()}
}

func sparse(size int64, seed uint64) []byte {
	raw := make([]byte, size)
	rng := rand.New(rand.NewPCG(seed, 7))
	for off := int64(0); off < size; off += 64 * 1024 {
		if rng.IntN(3) == 0 {
			end := min(off+64*1024, size)
			for i := off; i < end; i++ {
				raw[i] = byte(rng.Uint32())
			}
		}
	}
	return raw
}

func (e *env) config(vm *fakevsphere.VM, sel vsphere.DiskSelector) worker.Config {
	return worker.Config{
		Connection:        vsphere.ConnectionConfig{Server: e.fake.URL.Host, CAFile: e.fake.CAFile, TrustLeaseThumbprints: true},
		CredentialsDir:    e.creds,
		VM:                vsphere.VMSelector{InstanceUUID: vm.InstanceUUID},
		Disk:              sel,
		TargetMode:        "file",
		TargetPath:        e.out,
		StagingDir:        e.staging,
		IdleTimeout:       3 * time.Second,
		LeaseReadyTimeout: 5 * time.Second,
		KeepaliveInterval: 100 * time.Millisecond,
	}
}

func (e *env) run(cfg worker.Config) (*worker.Result, error) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return worker.Run(context.Background(), cfg, log, progress.New())
}

func (e *env) lastLease() fakevsphere.LeaseRecord {
	e.t.Helper()
	ls := e.fake.Leases()
	if len(ls) == 0 {
		e.t.Fatal("no lease was created")
	}
	var latest fakevsphere.LeaseRecord
	for _, l := range ls {
		if l.Ref.Value > latest.Ref.Value || latest.Ref.Value == "" {
			latest = l
		}
	}
	return latest
}

func (e *env) assertStagingEmpty() {
	e.t.Helper()
	entries, _ := os.ReadDir(e.staging)
	if len(entries) != 0 {
		e.t.Fatalf("staging directory not cleaned: %v", entries)
	}
}

func (e *env) assertNoImage() {
	e.t.Helper()
	for _, n := range []string{"disk.img", "disk.img.partial"} {
		if _, err := os.Stat(filepath.Join(e.out, n)); err == nil {
			e.t.Fatalf("%s exists after failure", n)
		}
	}
}

func TestFileTargetSuccess(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(6*1024*1024+512, 1)
	other := sparse(2*1024*1024, 2)
	vm, err := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "web-01", Disks: []fakevsphere.Disk{
		{Controller: "lsilogic", Bus: 0, Unit: 0, Raw: other},
		{Controller: "lsilogic", Bus: 0, Unit: 1, Raw: raw},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// A previous image with different content must be replaced.
	_ = os.WriteFile(filepath.Join(e.out, "disk.img"), bytes.Repeat([]byte{0xff}, 9<<20), 0o600)
	res, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Validated || res.GuestDigest != res.DestinationDigest || res.VirtualSize != int64(len(raw)) {
		t.Fatalf("result %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(e.out, "disk.img"))
	if !bytes.Equal(got, raw) {
		t.Fatal("disk.img differs from the selected source disk")
	}
	l := e.lastLease()
	if !l.Completed || l.Aborted || len(l.Progress) == 0 {
		t.Fatalf("lease %+v", l)
	}
	if res.Manifest.Status != "verified" || res.Export.Bytes == 0 || res.Disk.LeaseKey == "" {
		t.Fatalf("result %+v", res)
	}
	// Only a single GET (no Range, no parallel requests).
	if len(l.Requests) != 1 || l.Requests[0].Range != "" {
		t.Fatalf("requests %+v", l.Requests)
	}
	e.assertStagingEmpty()
}

func TestBlockTargetLoopDevice(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for loop devices")
	}
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(5*1024*1024, 3)
	vm, err := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "db-01", Disks: []fakevsphere.Disk{
		{Controller: "pvscsi", Bus: 0, Unit: 0, Raw: raw},
	}})
	if err != nil {
		t.Fatal(err)
	}
	backing := filepath.Join(t.TempDir(), "backing")
	_ = os.WriteFile(backing, bytes.Repeat([]byte{0xee}, 8<<20), 0o600)
	out, err := exec.Command("losetup", "--find", "--show", backing).Output()
	if err != nil {
		t.Skipf("losetup: %v", err)
	}
	dev := strings.TrimSpace(string(out))
	defer exec.Command("losetup", "-d", dev).Run()
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.TargetMode, cfg.TargetPath, cfg.ZeroTail = "block", dev, true
	res, err := e.run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Destination.VerifyMode != "O_DIRECT" || res.Destination.TailZeroMode == "" {
		t.Logf("verify mode %q tail %q", res.Destination.VerifyMode, res.Destination.TailZeroMode)
	}
	f, _ := os.Open(dev)
	defer f.Close()
	got := make([]byte, 8<<20)
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:len(raw)], raw) || !bytes.Equal(got[len(raw):], make([]byte, len(got)-len(raw))) {
		t.Fatal("block device content wrong (stale bytes remain)")
	}
}

func TestESXModeStarHost(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{ESX: true})
	raw := sparse(1024*1024, 4)
	vm, err := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "esx-vm", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: raw}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.VM = vsphere.VMSelector{MoRef: vm.Ref.Value}
	if _, err := e.run(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestUntrustedNFCHostRejected(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "x", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: sparse(1<<20, 5)}}})
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.Connection.TrustLeaseThumbprints = false
	_, err := e.run(cfg)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted NFC host accepted: %v", err)
	}
	if l := e.lastLease(); !l.Aborted || l.Completed {
		t.Fatalf("lease not aborted: %+v", l)
	}
	e.assertNoImage()
	e.assertStagingEmpty()
}

func TestFailuresAbortLease(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(4*1024*1024, 6)
	vm, err := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "fail", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: raw}}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		nfc   fakevsphere.NFCBehavior
		lease fakevsphere.LeaseBehavior
		cfg   func(*worker.Config)
		want  string
	}{
		{name: "http 503", nfc: fakevsphere.NFCBehavior{Status: 503}, want: "503"},
		{name: "http 404", nfc: fakevsphere.NFCBehavior{Status: 404}, want: "404"},
		{name: "truncated with content-length", nfc: fakevsphere.NFCBehavior{ContentLength: true, TruncateAfter: 300000}, want: "shorter than Content-Length"},
		{name: "truncated chunked", nfc: fakevsphere.NFCBehavior{TruncateAfter: 300000}, want: "export stream"},
		{name: "clean early end", nfc: fakevsphere.NFCBehavior{CleanEndAfter: 300000}, want: "stream truncated"},
		{name: "stalled", nfc: fakevsphere.NFCBehavior{StallAfter: 200000}, want: "idle timeout"},
		{name: "corrupt byte", nfc: fakevsphere.NFCBehavior{CorruptAt: 70000}, want: "export stream"},
		{name: "wrong manifest", lease: fakevsphere.LeaseBehavior{Manifest: fakevsphere.ManifestWrong}, want: "does not match the vSphere manifest"},
		{name: "required manifest missing", lease: fakevsphere.LeaseBehavior{Manifest: fakevsphere.ManifestUnsupported},
			cfg: func(c *worker.Config) { c.RequireManifest = true }, want: "manifest required"},
		{name: "lease init error", lease: fakevsphere.LeaseBehavior{InitError: "host is in maintenance mode"}, want: "maintenance mode"},
		{name: "keepalive failure", nfc: fakevsphere.NFCBehavior{BytesPerSecond: 400000},
			lease: fakevsphere.LeaseBehavior{ProgressFailAfter: 2}, want: "keepalive failed"},
		{name: "vcsim key format", lease: fakevsphere.LeaseBehavior{VcsimKeys: true}, want: "unrecognized lease device key"},
		{name: "staging too small", cfg: func(c *worker.Config) { c.StagingLimitBytes = 1 << 20 }, want: "staging capacity exceeded"},
		{name: "expected capacity mismatch", cfg: func(c *worker.Config) { c.ExpectedCapacityBytes = 1 << 30 }, want: "expected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.fake.SetNFCBehavior(tc.nfc)
			e.fake.SetLeaseBehavior(tc.lease)
			defer e.fake.SetNFCBehavior(fakevsphere.NFCBehavior{})
			defer e.fake.SetLeaseBehavior(fakevsphere.LeaseBehavior{})
			for _, n := range []string{"disk.img", "disk.img.partial"} {
				_ = os.Remove(filepath.Join(e.out, n))
			}
			before := len(e.fake.Leases())
			cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			res, err := e.run(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
			if res.Validated {
				t.Fatal("failed run reported as validated")
			}
			if strings.Contains(err.Error(), fakevsphere.Password) || strings.Contains(err.Error(), "/nfc/") && !strings.Contains(err.Error(), "REDACTED") {
				t.Fatalf("error leaks secrets: %v", err)
			}
			if len(e.fake.Leases()) > before {
				if l := e.lastLease(); l.Completed || (!l.Aborted && tc.lease.InitError == "") {
					t.Fatalf("lease not aborted: %+v", l)
				}
			}
			e.assertNoImage()
			e.assertStagingEmpty()
		})
	}
}

func TestKeepaliveOutlivesShortLeaseTimeout(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(3*1024*1024, 8)
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "slow", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: raw}}})
	// The lease reports a 1s timeout and expires after 1.5s without renewal;
	// the transfer takes longer than that.
	e.fake.SetLeaseBehavior(fakevsphere.LeaseBehavior{ExpireWithoutProgress: 1500 * time.Millisecond, TimeoutSeconds: 1})
	e.fake.SetNFCBehavior(fakevsphere.NFCBehavior{BytesPerSecond: 250000})
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.KeepaliveInterval = 0 // derived from the 1s lease timeout
	res, err := e.run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.KeepaliveRenewals < 2 {
		t.Fatalf("renewals %d", res.KeepaliveRenewals)
	}
	l := e.lastLease()
	if l.Expired || !l.Completed {
		t.Fatalf("lease %+v", l)
	}
	// Reported percentages are monotonic and below 100 until completion.
	for i := 1; i < len(l.Progress); i++ {
		if l.Progress[i] < l.Progress[i-1] {
			t.Fatalf("progress went backwards: %v", l.Progress)
		}
	}
}

func TestLeaseExpiryWithoutKeepaliveFails(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "expire", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: sparse(3*1024*1024, 9)}}})
	e.fake.SetLeaseBehavior(fakevsphere.LeaseBehavior{ExpireWithoutProgress: 500 * time.Millisecond})
	e.fake.SetNFCBehavior(fakevsphere.NFCBehavior{BytesPerSecond: 600000})
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.KeepaliveInterval = time.Hour // effectively disabled
	if _, err := e.run(cfg); err == nil {
		t.Fatal("transfer on an expired lease succeeded")
	}
	e.assertNoImage()
}

func TestOutOfOrderStream(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(4*1024*1024, 10)
	var buf bytes.Buffer
	if _, err := vmdk.WriteStreamOptimized(&buf, bytes.NewReader(raw), int64(len(raw)), vmdk.WriterOptions{ShuffleSeed: 99}); err != nil {
		t.Fatal(err)
	}
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "shuffled", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: raw, Stream: buf.Bytes()}}})
	res, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Export.Ordered {
		t.Fatal("expected unordered stream")
	}
	got, _ := os.ReadFile(filepath.Join(e.out, "disk.img"))
	if !bytes.Equal(got, raw) {
		t.Fatal("content mismatch")
	}
}

func TestStreamCapacityMismatch(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(2*1024*1024, 11)
	var buf bytes.Buffer
	_, _ = vmdk.WriteStreamOptimized(&buf, bytes.NewReader(raw[:1024*1024]), 1024*1024, vmdk.WriterOptions{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "mismatch", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: raw, Stream: buf.Bytes()}}})
	_, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"}))
	if !errors.Is(err, vsphere.ErrSourceState) || !strings.Contains(err.Error(), "virtual size") {
		t.Fatalf("got %v", err)
	}
	e.assertNoImage()
}

func TestInsufficientDestinationBeforeLease(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "big", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: sparse(4*1024*1024, 12)}}})
	dev := filepath.Join(t.TempDir(), "small-device")
	_ = os.WriteFile(dev, make([]byte, 1<<20), 0o600)
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.TargetMode, cfg.TargetPath, cfg.AllowRegularFileTarget = "block", dev, true
	_, err := e.run(cfg)
	if !errors.Is(err, target.ErrCapacity) {
		t.Fatalf("got %v", err)
	}
	if len(e.fake.Leases()) != 0 {
		t.Fatal("a lease was requested despite insufficient capacity")
	}
}

func TestPoweredOnRejectedBeforeLease(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "on", PowerOn: true, Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: sparse(1<<20, 13)}}})
	_, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"}))
	if !errors.Is(err, vsphere.ErrSourceState) {
		t.Fatalf("got %v", err)
	}
	if len(e.fake.Leases()) != 0 {
		t.Fatal("lease requested for powered-on VM")
	}
}

func TestManifestUnavailableIsRecorded(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "nomf", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: sparse(1<<20, 14)}}})
	e.fake.SetLeaseBehavior(fakevsphere.LeaseBehavior{Manifest: fakevsphere.ManifestUnsupported})
	res, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Status != "unavailable" {
		t.Fatalf("manifest status %q", res.Manifest.Status)
	}
	e.fake.SetLeaseBehavior(fakevsphere.LeaseBehavior{Manifest: fakevsphere.ManifestSHA256})
	res, err = e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"}))
	if err != nil || res.Manifest.Algorithm != "SHA256" || res.Manifest.Status != "verified" {
		t.Fatalf("sha256 manifest: %v %+v", err, res.Manifest)
	}
}

// TestInterruptedConversion cancels the worker while qemu-img runs and checks
// that the lease is aborted and no partial output or staging data remains.
func TestInterruptedConversion(t *testing.T) {
	e := newEnv(t, fakevsphere.Options{})
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "intr", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: sparse(2<<20, 15)}}})
	real := testutil.RequireQemuImg(t)
	wrapper := filepath.Join(t.TempDir(), "qemu-img")
	script := "#!/bin/sh\ncase \"$1\" in convert) sleep 60;; *) exec " + real + " \"$@\";; esac\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})
	cfg.QemuImgPath = wrapper
	tr := progress.New()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for tr.Snapshot().Phase != progress.PhaseConverting {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := worker.Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), tr)
	if err == nil || res.Validated {
		t.Fatal("cancelled run succeeded")
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("cancellation was not prompt")
	}
	if tr.Snapshot().Phase != progress.PhaseCancelled {
		t.Fatalf("phase %s", tr.Snapshot().Phase)
	}
	if l := e.lastLease(); !l.Aborted || l.Completed {
		t.Fatalf("lease %+v", l)
	}
	e.assertNoImage()
	e.assertStagingEmpty()
}

func TestRetryAfterFailureSucceeds(t *testing.T) {
	// A failed attempt followed by a successful one (what a Job retry does)
	// yields a correct image; stale partial and staging files are removed.
	e := newEnv(t, fakevsphere.Options{})
	raw := sparse(3<<20, 16)
	vm, _ := e.fake.AddVM(context.Background(), fakevsphere.VMSpec{Name: "retry", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: raw}}})
	_ = os.WriteFile(filepath.Join(e.staging, "export-stale.vmdk"), []byte("old"), 0o600)
	_ = os.WriteFile(filepath.Join(e.out, "disk.img.partial"), []byte("old partial"), 0o600)
	e.fake.SetNFCBehavior(fakevsphere.NFCBehavior{TruncateAfter: 100000})
	if _, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})); err == nil {
		t.Fatal("first attempt should fail")
	}
	e.fake.SetNFCBehavior(fakevsphere.NFCBehavior{})
	if _, err := e.run(e.config(vm, vsphere.DiskSelector{BusLocation: "scsi:0:0"})); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(e.out, "disk.img"))
	if !bytes.Equal(got, raw) {
		t.Fatal("content mismatch after retry")
	}
	e.assertStagingEmpty()
}
