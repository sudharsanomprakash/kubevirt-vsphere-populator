package diagnose_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/diagnose"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/test/fakevsphere"
)

func setup(t *testing.T) (*fakevsphere.Server, *fakevsphere.VM, diagnose.Config) {
	t.Helper()
	s, err := fakevsphere.Start(fakevsphere.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	raw := make([]byte, 3<<20)
	for i := range raw {
		if i%7000 < 300 {
			raw[i] = byte(i)
		}
	}
	vm, err := s.AddVM(context.Background(), fakevsphere.VMSpec{Name: "diag", Disks: []fakevsphere.Disk{
		{Controller: "lsilogic", Raw: raw}, {Controller: "lsilogic", Unit: 1, Raw: raw[:1<<20]},
	}})
	if err != nil {
		t.Fatal(err)
	}
	creds := t.TempDir()
	_ = os.WriteFile(filepath.Join(creds, "username"), []byte(fakevsphere.Username), 0o600)
	_ = os.WriteFile(filepath.Join(creds, "password"), []byte(fakevsphere.Password), 0o600)
	cfg := diagnose.Config{
		Connection:     vsphere.ConnectionConfig{Server: s.URL.Host, CAFile: s.CAFile, TrustLeaseThumbprints: true},
		CredentialsDir: creds,
		VM:             vsphere.VMSelector{MoRef: vm.Ref.Value},
	}
	return s, vm, cfg
}

func TestDiagnoseReport(t *testing.T) {
	s, _, cfg := setup(t)
	cfg.FullRead = true
	rep, err := diagnose.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Probes) != 2 || len(rep.Lease.DeviceURLs) != 2 {
		t.Fatalf("probes %d urls %d", len(rep.Probes), len(rep.Lease.DeviceURLs))
	}
	for _, p := range rep.Probes {
		if p.Format == nil || !p.Format.StreamOptimized || p.Format.Descriptor.CreateType != "streamOptimized" {
			t.Fatalf("format %+v", p.Format)
		}
		if p.RangeHonored == nil || *p.RangeHonored || p.Range.Status != 200 {
			t.Fatalf("range should be reported as ignored: %+v", p.Range)
		}
		if p.StableAcrossRequests == nil || !*p.StableAcrossRequests {
			t.Fatal("stable stream reported unstable")
		}
		if p.Full == nil || p.Full.Error != "" || p.Full.Bytes == 0 || p.Full.GuestDigest == "" || !p.Full.FooterPresent {
			t.Fatalf("full read %+v", p.Full)
		}
	}
	for _, d := range rep.Lease.DeviceURLs {
		if !d.KeyParses || d.MatchedDisk == 0 || !d.HasSSLThumbprint {
			t.Fatalf("device url %+v", d)
		}
	}
	if rep.ManifestStatus != "ok" || len(rep.Manifest) != 2 {
		t.Fatalf("manifest %s %v", rep.ManifestStatus, rep.Manifest)
	}
	if rep.Lease.Disposition != "aborted" {
		t.Fatalf("disposition %q", rep.Lease.Disposition)
	}
	for _, l := range s.Leases() {
		if l.Completed || !l.Aborted {
			t.Fatalf("diagnostic lease not aborted: %+v", l)
		}
	}
	// No ticket, password or full NFC URL may appear anywhere in the report.
	b, _ := json.Marshal(rep)
	out := string(b)
	if strings.Contains(out, fakevsphere.Password) || strings.Contains(out, "/nfc/") && !strings.Contains(out, "/REDACTED/") {
		t.Fatalf("report leaks secrets: %s", out)
	}
	if strings.Contains(out, "vmware_soap_session") {
		t.Fatal("cookie leaked")
	}
}

func TestDiagnoseRangeHonoredAndUnstable(t *testing.T) {
	s, _, cfg := setup(t)
	s.SetNFCBehavior(fakevsphere.NFCBehavior{HonorRange: true, ChangeOnRepeat: true, ContentLength: true})
	cfg.Disk = vsphere.DiskSelector{BusLocation: "scsi:0:0"}
	rep, err := diagnose.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Probes) != 1 {
		t.Fatalf("probes %d", len(rep.Probes))
	}
	p := rep.Probes[0]
	if p.Range.Status != 206 || p.Range.Headers["Content-Range"] == "" {
		t.Fatalf("range %+v", p.Range)
	}
	// With ChangeOnRepeat the range response comes from a changed stream.
	// Byte 512-1023 is inside the descriptor/overhead, which is unchanged,
	// so Range is still honored; the repeat request differs in grain data.
	if p.RangeHonored == nil || !*p.RangeHonored {
		t.Fatal("range not reported as honored")
	}
	if p.StableAcrossRequests == nil || *p.StableAcrossRequests {
		t.Fatal("changing content not detected")
	}
	found := false
	for _, n := range rep.Notes {
		found = found || strings.Contains(n, "not stable")
	}
	if !found {
		t.Fatalf("notes %v", rep.Notes)
	}
}

func TestDiagnoseNonStreamFormat(t *testing.T) {
	s, err := fakevsphere.Start(fakevsphere.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	flat := make([]byte, 1<<20) // a raw "flat" disk served instead of a VMDK stream
	_, err = s.AddVM(context.Background(), fakevsphere.VMSpec{Name: "flat", Disks: []fakevsphere.Disk{{Controller: "lsilogic", Raw: flat, Stream: flat}}})
	if err != nil {
		t.Fatal(err)
	}
	creds := t.TempDir()
	_ = os.WriteFile(filepath.Join(creds, "username"), []byte(fakevsphere.Username), 0o600)
	_ = os.WriteFile(filepath.Join(creds, "password"), []byte(fakevsphere.Password), 0o600)
	rep, err := diagnose.Run(context.Background(), diagnose.Config{
		Connection:     vsphere.ConnectionConfig{Server: s.URL.Host, CAFile: s.CAFile, TrustLeaseThumbprints: true},
		CredentialsDir: creds, VM: vsphere.VMSelector{Name: "flat"}, FullRead: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := rep.Probes[0]
	if p.Format.SparseExtent || p.Format.StreamOptimized || p.Full.Error == "" {
		t.Fatalf("raw bytes misidentified: %+v %+v", p.Format, p.Full)
	}
}
