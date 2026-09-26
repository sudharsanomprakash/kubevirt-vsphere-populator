package vsphere_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25/types"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/test/fakevsphere"
)

func startFake(t *testing.T, opts fakevsphere.Options) *fakevsphere.Server {
	t.Helper()
	s, err := fakevsphere.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func connect(t *testing.T, s *fakevsphere.Server) *vsphere.Client {
	t.Helper()
	c, err := vsphere.Connect(context.Background(), vsphere.ConnectionConfig{
		Server: s.URL.Host, Username: fakevsphere.Username, Password: fakevsphere.Password,
		CAFile: s.CAFile, TrustLeaseThumbprints: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Logout(context.Background()) })
	return c
}

func disk(ctrl string, bus, unit int32, size int) fakevsphere.Disk {
	raw := make([]byte, size)
	for i := range raw {
		if i%4096 < 16 {
			raw[i] = byte(unit + 1)
		}
	}
	return fakevsphere.Disk{Controller: ctrl, Bus: bus, Unit: unit, Raw: raw}
}

func TestTLSVerification(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	base := vsphere.ConnectionConfig{Server: s.URL.Host, Username: fakevsphere.Username, Password: fakevsphere.Password}

	// Default: system roots only; the self-signed simulator is rejected.
	if _, err := vsphere.Connect(ctx, base); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("untrusted server accepted: %v", err)
	}
	// CA bundle.
	withCA := base
	withCA.CAFile = s.CAFile
	c, err := vsphere.Connect(ctx, withCA)
	if err != nil {
		t.Fatalf("CA bundle: %v", err)
	}
	c.Logout(ctx)
	// Thumbprint pinning (with and without colons, lower case).
	withTP := base
	withTP.Thumbprint = strings.ToLower(strings.ReplaceAll(s.Thumbprint, ":", ""))
	if c, err = vsphere.Connect(ctx, withTP); err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	c.Logout(ctx)
	// Wrong thumbprint.
	bad := base
	bad.Thumbprint = strings.Repeat("AB:", 19) + "AB"
	if _, err := vsphere.Connect(ctx, bad); err == nil {
		t.Fatal("wrong thumbprint accepted")
	}
	// Explicit insecure test option.
	ins := base
	ins.InsecureSkipVerifyForTesting = true
	if c, err = vsphere.Connect(ctx, ins); err != nil {
		t.Fatalf("insecure: %v", err)
	}
	c.Logout(ctx)
	// Wrong password; the password must not appear in the error.
	wrong := withCA
	wrong.Password = "definitely-wrong-password"
	_, err = vsphere.Connect(ctx, wrong)
	if err == nil || strings.Contains(err.Error(), "definitely-wrong-password") {
		t.Fatalf("bad login: %v", err)
	}
}

func TestServerURLValidation(t *testing.T) {
	for _, bad := range []string{"", "http://vc.example.test", "https://user:pw@vc.example.test", "ftp://x"} {
		c := vsphere.ConnectionConfig{Server: bad}
		if _, err := c.ServerURL(); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	c := vsphere.ConnectionConfig{Server: "vc.example.test:8443"}
	u, err := c.ServerURL()
	if err != nil || u.String() != "https://vc.example.test:8443/sdk" {
		t.Fatalf("got %v %v", u, err)
	}
}

func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "username"), []byte("u@vsphere.local\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("p4ss\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("x"), 0o600)
	var c vsphere.ConnectionConfig
	if err := c.LoadCredentials(dir); err != nil {
		t.Fatal(err)
	}
	if c.Username != "u@vsphere.local" || c.Password != "p4ss" || c.CAFile != filepath.Join(dir, "ca.crt") {
		t.Fatalf("got %+v", c)
	}
	t.Setenv(vsphere.EnvUsername, "envuser")
	t.Setenv(vsphere.EnvPassword, "envpass")
	var e vsphere.ConnectionConfig
	if err := e.LoadCredentials(t.TempDir()); err != nil || e.Username != "envuser" || e.Password != "envpass" {
		t.Fatalf("env fallback: %+v %v", e, err)
	}
	t.Setenv(vsphere.EnvPassword, "")
	var m vsphere.ConnectionConfig
	if err := m.LoadCredentials(t.TempDir()); err == nil {
		t.Fatal("missing password accepted")
	}
}

func TestResolveVM(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	a, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "app-01", Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	// Two VMs with the same name in different folders.
	if _, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "dup", Folder: "team-a", Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "dup", Folder: "team-b", Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}}); err != nil {
		t.Fatal(err)
	}
	// A VM whose name is a glob match for "app-0?" but not equal.
	if _, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "app-02", Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}}); err != nil {
		t.Fatal(err)
	}
	c := connect(t, s)

	for _, sel := range []vsphere.VMSelector{
		{MoRef: a.Ref.Value}, {InstanceUUID: a.InstanceUUID}, {BIOSUUID: a.BIOSUUID}, {Name: "app-01"},
	} {
		ref, err := c.ResolveVM(ctx, sel)
		if err != nil || ref != a.Ref {
			t.Fatalf("%s: %v %v", sel, ref, err)
		}
	}
	if _, err := c.ResolveVM(ctx, vsphere.VMSelector{Name: "dup"}); !errors.Is(err, vsphere.ErrAmbiguous) {
		t.Fatalf("ambiguous name: %v", err)
	}
	if _, err := c.ResolveVM(ctx, vsphere.VMSelector{Name: "app-0?"}); !errors.Is(err, vsphere.ErrNotFound) {
		t.Fatalf("glob name must not match: %v", err)
	}
	if _, err := c.ResolveVM(ctx, vsphere.VMSelector{InstanceUUID: "00000000-0000-0000-0000-000000000000"}); !errors.Is(err, vsphere.ErrNotFound) {
		t.Fatalf("unknown uuid: %v", err)
	}
	if _, err := c.ResolveVM(ctx, vsphere.VMSelector{MoRef: "vm-99999"}); !errors.Is(err, vsphere.ErrNotFound) {
		t.Fatalf("unknown moref: %v", err)
	}
	if _, err := c.ResolveVM(ctx, vsphere.VMSelector{Name: "a", MoRef: "vm-1"}); err == nil {
		t.Fatal("two identities accepted")
	}
	if _, err := c.ResolveVM(ctx, vsphere.VMSelector{}); err == nil {
		t.Fatal("no identity accepted")
	}
}

func TestInspectAndSelectDisks(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	vm, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "multi", Disks: []fakevsphere.Disk{
		disk("lsilogic", 0, 0, 1<<20), disk("lsilogic", 0, 1, 2<<20), disk("pvscsi", 1, 0, 3<<20),
		disk("nvme", 0, 0, 1<<20), disk("sata", 0, 2, 1<<20),
	}})
	if err != nil {
		t.Fatal(err)
	}
	c := connect(t, s)
	info, err := c.InspectVM(ctx, vm.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := info.CheckColdSource(); err != nil {
		t.Fatal(err)
	}
	if len(info.Disks) != 5 {
		t.Fatalf("got %d disks", len(info.Disks))
	}
	byLoc := map[string]vsphere.DiskInfo{}
	for _, d := range info.Disks {
		byLoc[d.BusLocation] = d
	}
	// pvscsi is the first ParaVirtualSCSIController but has bus number 1;
	// the lease key convention for that layout is unverified, so it is rejected.
	if d := byLoc["scsi:1:0"]; d.Supported || d.ExpectedLeaseKey != "" || !strings.Contains(d.UnsupportedReason, "not been verified") {
		t.Fatalf("pvscsi on bus 1: %+v", d)
	}
	want := map[string]string{
		"scsi:0:0": "/" + vm.Ref.Value + "/VirtualLsiLogicController0:0",
		"scsi:0:1": "/" + vm.Ref.Value + "/VirtualLsiLogicController0:1",
		"nvme:0:0": "/" + vm.Ref.Value + "/VirtualNVMEController0:0",
		"sata:0:2": "/" + vm.Ref.Value + "/VirtualAHCIController0:2",
	}
	for loc, key := range want {
		if byLoc[loc].ExpectedLeaseKey != key || !byLoc[loc].Supported {
			t.Errorf("%s: key %q supported=%v reason=%s", loc, byLoc[loc].ExpectedLeaseKey, byLoc[loc].Supported, byLoc[loc].UnsupportedReason)
		}
	}
	d, err := info.SelectDisk(vsphere.DiskSelector{BusLocation: "SCSI0:1"})
	if err != nil || d.CapacityBytes != 2<<20 {
		t.Fatalf("select by bus: %+v %v", d, err)
	}
	if d2, err := info.SelectDisk(vsphere.DiskSelector{DeviceKey: d.Key}); err != nil || d2.BusLocation != "scsi:0:1" {
		t.Fatalf("select by key: %v", err)
	}
	if d3, err := info.SelectDisk(vsphere.DiskSelector{FileName: d.FileName}); err != nil || d3.Key != d.Key {
		t.Fatalf("select by file: %v", err)
	}
	if _, err := info.SelectDisk(vsphere.DiskSelector{BusLocation: "scsi:3:3"}); err == nil {
		t.Fatal("missing disk selected")
	}
	if _, err := info.SelectDisk(vsphere.DiskSelector{BusLocation: "scsi:1:0"}); !errors.Is(err, vsphere.ErrUnsupported) {
		t.Fatalf("unsupported disk selected: %v", err)
	}
	if _, err := info.SelectDisk(vsphere.DiskSelector{DeviceKey: 2000, BusLocation: "scsi:0:0"}); err == nil {
		t.Fatal("two selectors accepted")
	}
}

func TestSourceStateChecks(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	on, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "running", PowerOn: true, Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "snapped", Snapshot: true, Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	c := connect(t, s)
	info, err := c.InspectVM(ctx, on.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := info.CheckColdSource(); !errors.Is(err, vsphere.ErrSourceState) || !strings.Contains(err.Error(), "poweredOn") {
		t.Fatalf("powered-on VM: %v", err)
	}
	// The fake, like vSphere, refuses to export a powered-on VM.
	if _, err := c.ExportVM(ctx, on.Ref, 5*time.Second); err == nil {
		t.Fatal("export of powered-on VM succeeded")
	}
	info, err = c.InspectVM(ctx, snap.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := info.CheckColdSource(); !errors.Is(err, vsphere.ErrSourceState) || !strings.Contains(err.Error(), "snapshots") {
		t.Fatalf("snapshot VM: %v", err)
	}
}

func TestLeaseMapping(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	vm, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "map", Disks: []fakevsphere.Disk{
		disk("lsilogic", 0, 0, 1<<20), disk("lsilogic", 0, 1, 2<<20), disk("sata", 0, 0, 1<<20),
	}})
	if err != nil {
		t.Fatal(err)
	}
	c := connect(t, s)
	info, _ := c.InspectVM(ctx, vm.Ref)

	lease, err := c.ExportVM(ctx, vm.Ref, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	urls := lease.Info().DeviceUrl
	for _, loc := range []string{"scsi:0:0", "scsi:0:1", "sata:0:0"} {
		d, err := info.SelectDisk(vsphere.DiskSelector{BusLocation: loc})
		if err != nil {
			t.Fatal(err)
		}
		u, err := vsphere.MatchDeviceURL(info, d, urls)
		if err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		if u.Key != d.ExpectedLeaseKey {
			t.Fatalf("%s mapped to %s", loc, u.Key)
		}
	}
	// Reordering URLs must not change the mapping.
	rev := []types.HttpNfcLeaseDeviceUrl{urls[2], urls[0], urls[1]}
	d1, _ := info.SelectDisk(vsphere.DiskSelector{BusLocation: "scsi:0:1"})
	if u, err := vsphere.MatchDeviceURL(info, d1, rev); err != nil || u.Key != d1.ExpectedLeaseKey {
		t.Fatalf("reordered: %v", err)
	}
	if err := lease.Abort(ctx, errors.New("test done")); err != nil {
		t.Fatal(err)
	}

	for name, lb := range map[string]fakevsphere.LeaseBehavior{
		"vcsim key format": {VcsimKeys: true},
		"extra disk url":   {ExtraDiskURL: true},
		"missing disk url": {DropDiskURLs: []int32{d1.Key}},
	} {
		t.Run(name, func(t *testing.T) {
			s.SetLeaseBehavior(lb)
			defer s.SetLeaseBehavior(fakevsphere.LeaseBehavior{})
			l, err := c.ExportVM(ctx, vm.Ref, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Abort(ctx, nil)
			if _, err := vsphere.MatchDeviceURL(info, d1, l.Info().DeviceUrl); !errors.Is(err, vsphere.ErrSourceState) {
				t.Fatalf("expected mapping failure, got %v", err)
			}
		})
	}
}

func TestLeaseLifecycleFaults(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	vm, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "life", Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	c := connect(t, s)

	t.Run("init error", func(t *testing.T) {
		s.SetLeaseBehavior(fakevsphere.LeaseBehavior{InitError: "datastore unavailable"})
		defer s.SetLeaseBehavior(fakevsphere.LeaseBehavior{})
		_, err := c.ExportVM(ctx, vm.Ref, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "datastore unavailable") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("ready timeout aborts lease", func(t *testing.T) {
		s.SetLeaseBehavior(fakevsphere.LeaseBehavior{ReadyDelay: 5 * time.Second})
		defer s.SetLeaseBehavior(fakevsphere.LeaseBehavior{})
		_, err := c.ExportVM(ctx, vm.Ref, 300*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Fatalf("got %v", err)
		}
		aborted := false
		for _, l := range s.Leases() {
			aborted = aborted || l.Aborted
		}
		if !aborted {
			t.Fatal("lease not aborted after ready timeout")
		}
	})
	t.Run("keepalive failure propagates", func(t *testing.T) {
		s.SetLeaseBehavior(fakevsphere.LeaseBehavior{ProgressFailAfter: 2})
		defer s.SetLeaseBehavior(fakevsphere.LeaseBehavior{})
		l, err := c.ExportVM(ctx, vm.Ref, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Abort(ctx, nil)
		failed := make(chan error, 1)
		k := l.StartKeepalive(50*time.Millisecond, 3, func() int32 { return 10 }, func(err error) { failed <- err })
		defer k.Stop()
		select {
		case err := <-failed:
			if !strings.Contains(err.Error(), "keepalive failed") {
				t.Fatalf("got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("keepalive failure not reported")
		}
		if k.Err() == nil {
			t.Fatal("Err() not set")
		}
	})
	t.Run("keepalive renews", func(t *testing.T) {
		l, err := c.ExportVM(ctx, vm.Ref, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var pct atomic.Int32
		pct.Store(42)
		k := l.StartKeepalive(30*time.Millisecond, 3, pct.Load, func(error) { t.Error("unexpected failure") })
		time.Sleep(200 * time.Millisecond)
		k.Stop()
		if k.Calls() < 3 {
			t.Fatalf("only %d renewals", k.Calls())
		}
		if err := l.Complete(ctx); err != nil {
			t.Fatal(err)
		}
		if err := l.Complete(ctx); err == nil {
			t.Fatal("double complete accepted")
		}
		if err := l.Abort(ctx, errors.New("x")); err != nil {
			t.Fatal("abort after complete must be a no-op")
		}
		var rec *fakevsphere.LeaseRecord
		for _, r := range s.Leases() {
			if r.Ref == l.Reference() {
				rec = &r
			}
		}
		if rec == nil || !rec.Completed || rec.Aborted || len(rec.Progress) < 3 || rec.Progress[0] != 42 {
			t.Fatalf("lease record %+v", rec)
		}
	})
	t.Run("abort carries redacted reason", func(t *testing.T) {
		l, err := c.ExportVM(ctx, vm.Ref, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := c.PrepareNFCURL(l.Info().DeviceUrl[0])
		if err := l.Abort(ctx, errors.New("download of "+u.String()+" failed")); err != nil {
			t.Fatal(err)
		}
		for _, r := range s.Leases() {
			if r.Ref == l.Reference() {
				if !r.Aborted || strings.Contains(r.AbortMessage, "/nfc/") && !strings.Contains(r.AbortMessage, "REDACTED") {
					t.Fatalf("abort message not redacted: %q", r.AbortMessage)
				}
			}
		}
	})
}

func TestManifestModes(t *testing.T) {
	s := startFake(t, fakevsphere.Options{})
	ctx := context.Background()
	vm, err := s.AddVM(ctx, fakevsphere.VMSpec{Name: "mf", Disks: []fakevsphere.Disk{disk("lsilogic", 0, 0, 1<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	c := connect(t, s)
	s.SetLeaseBehavior(fakevsphere.LeaseBehavior{Manifest: fakevsphere.ManifestUnsupported})
	l, err := c.ExportVM(ctx, vm.Ref, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Abort(ctx, nil)
	if _, err := l.Manifest(ctx); !errors.Is(err, vsphere.ErrManifestUnavailable) {
		t.Fatalf("got %v", err)
	}
	s.SetLeaseBehavior(fakevsphere.LeaseBehavior{Manifest: fakevsphere.ManifestSHA256})
	l2, err := c.ExportVM(ctx, vm.Ref, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Abort(ctx, nil)
	m, err := l2.Manifest(ctx)
	if err != nil || len(m) != 1 || m[0].ChecksumType != "SHA256" || len(m[0].Checksum) != 64 {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if !bytes.Equal([]byte(m[0].Key), []byte(l2.Info().DeviceUrl[0].Key)) {
		t.Fatal("manifest key mismatch")
	}
}
