// Command bench measures the single-stream worker pipeline against the
// fakevsphere test double on the local machine: stream download over
// loopback TLS, VMDK decode and staging, qemu-img conversion, flush and
// read-back verification. It reports per-phase durations and CPU time.
//
// This measures this software on this host. It says nothing about vSphere,
// ESXi NFC throughput, datastores or network links.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/progress"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/worker"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/test/fakevsphere"
)

func cpu(who int) (user, sys float64) {
	var ru syscall.Rusage
	_ = syscall.Getrusage(who, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6, float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func main() {
	raw := flag.String("raw", "", "raw source disk image")
	mode := flag.String("target-mode", "file", "file or block")
	targetPath := flag.String("target", "", "target directory (file) or device (block)")
	staging := flag.String("staging", os.TempDir(), "staging directory")
	flag.Parse()
	if *raw == "" || *targetPath == "" {
		log.Fatal("usage: bench -raw disk.raw -target DIR|DEVICE [-target-mode file|block] [-staging DIR]")
	}
	s, err := fakevsphere.Start(fakevsphere.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	encStart := time.Now()
	vm, err := s.AddVM(context.Background(), fakevsphere.VMSpec{Name: "bench", Disks: []fakevsphere.Disk{{Controller: "lsilogic", RawFile: *raw}}})
	if err != nil {
		log.Fatal(err)
	}
	encode := time.Since(encStart)
	creds, _ := os.MkdirTemp("", "bench-creds")
	defer os.RemoveAll(creds)
	_ = os.WriteFile(filepath.Join(creds, "username"), []byte(fakevsphere.Username), 0o600)
	_ = os.WriteFile(filepath.Join(creds, "password"), []byte(fakevsphere.Password), 0o600)

	cfg := worker.Config{
		Connection:     vsphere.ConnectionConfig{Server: s.URL.Host, CAFile: s.CAFile, TrustLeaseThumbprints: true},
		CredentialsDir: creds, VM: vsphere.VMSelector{MoRef: vm.Ref.Value}, Disk: vsphere.DiskSelector{BusLocation: "scsi:0:0"},
		TargetMode: *mode, TargetPath: *targetPath, ZeroTail: true, StagingDir: *staging, RequireManifest: true,
	}
	u0, s0 := cpu(syscall.RUSAGE_SELF)
	start := time.Now()
	res, err := worker.Run(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), progress.New())
	wall := time.Since(start)
	u1, s1 := cpu(syscall.RUSAGE_SELF)
	cu, cs := cpu(syscall.RUSAGE_CHILDREN)
	if err != nil {
		log.Fatal(err)
	}
	out := map[string]any{
		"note":                    "local pipeline benchmark against fakevsphere over loopback; not a vSphere measurement",
		"cpus":                    runtime.NumCPU(),
		"virtualSizeBytes":        res.VirtualSize,
		"allocatedBytes":          res.Export.AllocatedBytes,
		"exportStreamBytes":       res.Export.Bytes,
		"fakeEncodeSeconds":       encode.Seconds(),
		"downloadDecodeSeconds":   res.Export.Seconds,
		"downloadStreamMBps":      res.Export.BytesPerSecond / 1e6,
		"downloadGuestMBps":       float64(res.VirtualSize) / res.Export.Seconds / 1e6,
		"convertSeconds":          res.Conversion.Seconds,
		"verifySeconds":           res.Destination.Seconds,
		"verifyMode":              res.Destination.VerifyMode,
		"totalSeconds":            wall.Seconds(),
		"endToEndGuestMBps":       float64(res.VirtualSize) / wall.Seconds() / 1e6,
		"processCPUSeconds":       (u1 - u0) + (s1 - s0),
		"qemuImgCPUSeconds":       cu + cs,
		"note2":                   "processCPUSeconds includes the in-process fake server (TLS serving) as well as the worker",
		"manifest":                res.Manifest.Status,
		"validated":               res.Validated,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
