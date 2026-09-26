// Command vsphere-populator-worker exports a disk of a powered-off vSphere VM
// into a raw file or block device, and provides read-only diagnostics.
//
// Credentials are read from files in --credentials-dir (a mounted Secret with
// "username" and "password" keys) or from the VSPHERE_USERNAME and
// VSPHERE_PASSWORD environment variables. There is deliberately no flag for
// the password.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/diagnose"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/digest"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/progress"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/version"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vmdk"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/worker"
)

const usage = `usage: vsphere-populator-worker <command> [flags]

commands:
  populate      export one VM disk into a raw file or block device
  diagnose      read-only inspection of a VM export (lease, format, headers, Range support)
  inspect-vmdk  decode a local streamOptimized VMDK and print its structure and digest
  digest        compute the chunked SHA-256 digest of a file or device prefix
  version       print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "populate":
		err = populate(ctx, os.Args[2:])
	case "diagnose":
		err = diag(ctx, os.Args[2:])
	case "inspect-vmdk":
		err = inspect(os.Args[2:])
	case "digest":
		err = digestCmd(os.Args[2:])
	case "version":
		fmt.Println(version.String())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type connFlags struct {
	server, credsDir, caFile, thumbprint string
	insecure, trustLease                 bool
	vmMoRef, vmInstance, vmBIOS, vmName  string
	datacenter                           string
	diskKey                              int
	diskBus, diskFile                    string
}

func (c *connFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.server, "server", "", "vCenter or ESXi host name, host:port or https URL")
	fs.StringVar(&c.credsDir, "credentials-dir", "", "directory with username, password and optional ca.crt/thumbprint files (a mounted Secret)")
	fs.StringVar(&c.caFile, "ca-file", "", "PEM bundle used to verify the server and NFC hosts")
	fs.StringVar(&c.thumbprint, "thumbprint", "", "verified SHA-1 or SHA-256 fingerprint of the server certificate")
	fs.BoolVar(&c.insecure, "insecure-skip-tls-verify-for-testing-only", false, "disable TLS verification (test environments only)")
	fs.BoolVar(&c.trustLease, "trust-lease-thumbprints", true, "trust NFC host certificates by the thumbprint returned in the lease over the verified API session")
	fs.StringVar(&c.vmMoRef, "vm-moref", "", "VM managed object reference (e.g. vm-42)")
	fs.StringVar(&c.vmInstance, "vm-instance-uuid", "", "VM instance UUID (vCenter)")
	fs.StringVar(&c.vmBIOS, "vm-bios-uuid", "", "VM BIOS UUID (rejected if not unique)")
	fs.StringVar(&c.vmName, "vm-name", "", "VM name (rejected if not unique)")
	fs.StringVar(&c.datacenter, "datacenter", "", "restrict UUID and name searches to this datacenter")
	fs.IntVar(&c.diskKey, "disk-key", 0, "virtual disk device key (e.g. 2000)")
	fs.StringVar(&c.diskBus, "disk-bus", "", "disk bus location, e.g. scsi:0:1")
	fs.StringVar(&c.diskFile, "disk-file", "", `disk backing file, e.g. "[datastore1] vm/vm_1.vmdk"`)
}

func (c *connFlags) connection() vsphere.ConnectionConfig {
	return vsphere.ConnectionConfig{Server: c.server, CAFile: c.caFile, Thumbprint: c.thumbprint,
		InsecureSkipVerifyForTesting: c.insecure, TrustLeaseThumbprints: c.trustLease,
		UserAgent: "kubevirt-vsphere-populator/" + version.Version}
}

func (c *connFlags) vm() vsphere.VMSelector {
	return vsphere.VMSelector{MoRef: c.vmMoRef, InstanceUUID: c.vmInstance, BIOSUUID: c.vmBIOS, Name: c.vmName, Datacenter: c.datacenter}
}

func (c *connFlags) disk() vsphere.DiskSelector {
	return vsphere.DiskSelector{DeviceKey: int32(c.diskKey), BusLocation: c.diskBus, FileName: c.diskFile}
}

func newLogger(format string) *slog.Logger {
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

func populate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("populate", flag.ExitOnError)
	var cf connFlags
	cf.register(fs)
	var cfg worker.Config
	var progressAddr, resultFile, logFormat string
	fs.Int64Var(&cfg.ExpectedCapacityBytes, "expected-capacity-bytes", 0, "fail unless the disk capacity equals this value")
	fs.StringVar(&cfg.TargetMode, "target-mode", "", "file or block")
	fs.StringVar(&cfg.TargetPath, "target-path", "", "mount directory (file) or device path (block)")
	fs.StringVar(&cfg.ImageName, "image-name", "disk.img", "image file name in file mode")
	fs.BoolVar(&cfg.ZeroTail, "zero-tail", true, "block mode: clear device bytes beyond the virtual disk size")
	fs.StringVar(&cfg.StagingDir, "staging-dir", os.TempDir(), "directory for the staged export stream")
	fs.Int64Var(&cfg.StagingLimitBytes, "staging-limit-bytes", 0, "maximum staged bytes (0: available space minus a reserve)")
	fs.StringVar(&cfg.QemuImgPath, "qemu-img", "", "qemu-img binary (default: from PATH)")
	fs.IntVar(&cfg.Coroutines, "qemu-img-coroutines", 0, "qemu-img -m parallelism (default: qemu-img default)")
	fs.DurationVar(&cfg.LeaseReadyTimeout, "lease-ready-timeout", 10*time.Minute, "maximum wait for the export lease")
	fs.DurationVar(&cfg.Timeout, "timeout", 0, "overall timeout (0: none)")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 5*time.Minute, "abort when no export bytes arrive for this long")
	fs.IntVar(&cfg.KeepaliveMaxFailures, "keepalive-max-failures", 3, "consecutive lease renewal failures tolerated")
	fs.BoolVar(&cfg.RequireManifest, "require-manifest", false, "fail when vSphere provides no manifest checksum for the disk")
	fs.StringVar(&progressAddr, "progress-addr", "", "serve progress JSON at this address (e.g. :8080)")
	fs.StringVar(&resultFile, "result-file", defaultResultFile(), "write the compact result here (termination message)")
	fs.StringVar(&logFormat, "log-format", "json", "json or text")
	_ = fs.Parse(args)
	cfg.Connection, cfg.CredentialsDir, cfg.VM, cfg.Disk = cf.connection(), cf.credsDir, cf.vm(), cf.disk()

	log := newLogger(logFormat).With("version", version.Version)
	tr := progress.New()
	if progressAddr != "" {
		go func() {
			if err := tr.Serve(ctx, progressAddr); err != nil {
				log.Warn("progress server failed", "error", err)
			}
		}()
	}
	res, err := worker.Run(ctx, cfg, log, tr)
	if werr := worker.WriteTermination(resultFile, res); werr != nil {
		log.Warn("could not write result file", "error", werr)
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	if err != nil {
		return err
	}
	if !res.Validated {
		return errors.New("population was not validated")
	}
	return nil
}

func defaultResultFile() string {
	if _, err := os.Stat("/dev/termination-log"); err == nil {
		return "/dev/termination-log"
	}
	return ""
}

func diag(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	var cf connFlags
	cf.register(fs)
	var cfg diagnose.Config
	var output string
	fs.BoolVar(&cfg.FullRead, "full-read", false, "download and decode every probed stream completely (measures single-stream throughput)")
	fs.Int64Var(&cfg.ProbeBytes, "probe-bytes", 64<<10, "bytes read by each probe request")
	fs.BoolVar(&cfg.SkipRange, "skip-range", false, "do not send a Range request")
	fs.BoolVar(&cfg.AllowPoweredOn, "allow-powered-on", false, "inspect even if the VM is not powered off")
	fs.DurationVar(&cfg.LeaseReadyTimeout, "lease-ready-timeout", 5*time.Minute, "maximum wait for the export lease")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 2*time.Minute, "per-request idle timeout")
	fs.StringVar(&output, "output", "", "write the JSON report to this file instead of stdout")
	_ = fs.Parse(args)
	cfg.Connection, cfg.CredentialsDir, cfg.VM, cfg.Disk = cf.connection(), cf.credsDir, cf.vm(), cf.disk()
	rep, err := diagnose.Run(ctx, cfg)
	b, merr := json.MarshalIndent(rep, "", "  ")
	if merr != nil {
		return merr
	}
	var w io.Writer = os.Stdout
	if output != "" {
		f, ferr := os.Create(output)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		w = f
	}
	fmt.Fprintln(w, string(b))
	return err
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect-vmdk", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: inspect-vmdk <file.vmdk>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	res, err := vmdk.Decode(f, vmdk.DecodeOptions{})
	out := map[string]any{"result": res}
	if err == nil && res.Digest == nil && res.Footer != nil {
		ra, rerr := vmdk.DecodeAt(f, st.Size(), *res.Footer, vmdk.DecodeOptions{})
		if rerr != nil {
			err = rerr
		} else {
			res.Digest = ra.Digest
		}
	}
	if res != nil && res.Digest != nil {
		out["guestDigest"] = fmt.Sprintf("kvsp-chunked-sha256-v1:%x", res.Digest)
		out["virtualSize"] = res.VirtualSize()
	}
	if err != nil {
		out["error"] = err.Error()
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return err
}

func digestCmd(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	size := fs.Int64("size", -1, "number of bytes to digest (default: whole file)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: digest [--size N] <file-or-device>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	n := *size
	if n < 0 {
		if n, err = f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	d, err := digest.Reader(f, n, 0)
	if err != nil {
		return err
	}
	fmt.Printf("kvsp-chunked-sha256-v1:%x  %d  %s\n", d, n, strings.TrimSpace(fs.Arg(0)))
	return nil
}
