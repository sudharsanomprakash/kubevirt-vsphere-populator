// Package worker implements a single cold population: it exports one disk
// of a powered-off vSphere VM over an HttpNfcLease, stages and validates the
// streamOptimized VMDK, converts it with qemu-img onto the destination,
// flushes, verifies the destination against an independent decode of the
// stream, and only then completes the lease.
package worker

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/convert"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/progress"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/redact"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/target"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/transfer"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vmdk"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
)

// Config is the worker configuration. It never contains secrets other than
// those loaded into Connection by LoadCredentials.
type Config struct {
	Connection     vsphere.ConnectionConfig
	CredentialsDir string

	VM   vsphere.VMSelector
	Disk vsphere.DiskSelector
	// ExpectedCapacityBytes, when non-zero, must equal the disk capacity.
	ExpectedCapacityBytes int64

	// TargetMode is "file" or "block".
	TargetMode string
	// TargetPath is the mount directory (file) or device path (block).
	TargetPath string
	// ImageName is the image file name in file mode (default disk.img).
	ImageName string
	// ZeroTail clears block device bytes beyond the virtual size.
	ZeroTail bool
	// AllowRegularFileTarget permits a regular file as block target (tests only).
	AllowRegularFileTarget bool

	StagingDir        string
	StagingLimitBytes int64
	QemuImgPath       string
	Coroutines        int

	LeaseReadyTimeout    time.Duration
	Timeout              time.Duration
	IdleTimeout          time.Duration
	KeepaliveInterval    time.Duration
	KeepaliveMaxFailures int
	// RequireManifest fails the population when the server provides no
	// manifest checksum for the disk.
	RequireManifest bool
}

// Defaults fills unset durations and limits.
func (c *Config) Defaults() {
	if c.LeaseReadyTimeout == 0 {
		c.LeaseReadyTimeout = 10 * time.Minute
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.KeepaliveMaxFailures == 0 {
		c.KeepaliveMaxFailures = 3
	}
	if c.ImageName == "" {
		c.ImageName = target.DefaultImageName
	}
	if c.StagingDir == "" {
		c.StagingDir = os.TempDir()
	}
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	if err := c.VM.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Disk.Validate(); err != nil {
		errs = append(errs, err)
	}
	switch c.TargetMode {
	case "file", "block":
	default:
		errs = append(errs, fmt.Errorf("target mode must be file or block, got %q", c.TargetMode))
	}
	if c.TargetPath == "" {
		errs = append(errs, errors.New("target path is required"))
	}
	if strings.ContainsAny(c.ImageName, "/\x00") || c.ImageName == "." || c.ImageName == ".." {
		errs = append(errs, fmt.Errorf("invalid image name %q", c.ImageName))
	}
	if _, err := c.Connection.ServerURL(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Result describes a population. It contains no secrets.
type Result struct {
	Validated bool   `json:"validated"`
	Error     string `json:"error,omitempty"`
	Phase     string `json:"phase,omitempty"`

	Server struct {
		APIType string `json:"apiType,omitempty"`
		Version string `json:"version,omitempty"`
		Build   string `json:"build,omitempty"`
	} `json:"server"`
	VM struct {
		MoRef        string `json:"moRef,omitempty"`
		Name         string `json:"name,omitempty"`
		InstanceUUID string `json:"instanceUUID,omitempty"`
	} `json:"vm"`
	Disk struct {
		Key           int32  `json:"key,omitempty"`
		BusLocation   string `json:"busLocation,omitempty"`
		FileName      string `json:"fileName,omitempty"`
		CapacityBytes int64  `json:"capacityBytes,omitempty"`
		LeaseKey      string `json:"leaseKey,omitempty"`
	} `json:"disk"`
	Export struct {
		Bytes          int64   `json:"bytes"`
		SHA1           string  `json:"sha1,omitempty"`
		SHA256         string  `json:"sha256,omitempty"`
		ContentType    string  `json:"contentType,omitempty"`
		ContentLength  int64   `json:"contentLength"`
		Grains         int64   `json:"grains"`
		AllocatedBytes int64   `json:"allocatedBytes"`
		Ordered        bool    `json:"ordered"`
		Seconds        float64 `json:"seconds"`
		BytesPerSecond float64 `json:"bytesPerSecond"`
	} `json:"export"`
	Manifest struct {
		// Status is "verified", "unavailable" or "no-entry".
		Status    string `json:"status"`
		Algorithm string `json:"algorithm,omitempty"`
	} `json:"manifest"`
	Conversion struct {
		Tool    string  `json:"tool"`
		Version string  `json:"version"`
		Seconds float64 `json:"seconds"`
	} `json:"conversion"`
	Destination struct {
		Kind         string  `json:"kind,omitempty"`
		DeviceBytes  int64   `json:"deviceBytes,omitempty"`
		TailZeroMode string  `json:"tailZeroMode,omitempty"`
		VerifyMode   string  `json:"verifyMode,omitempty"`
		Seconds      float64 `json:"verifySeconds"`
	} `json:"destination"`
	VirtualSize       int64   `json:"virtualSize"`
	GuestDigest       string  `json:"guestDigest,omitempty"`
	DestinationDigest string  `json:"destinationDigest,omitempty"`
	KeepaliveRenewals int64   `json:"keepaliveRenewals"`
	TotalSeconds      float64 `json:"totalSeconds"`
}

const digestPrefix = "kvsp-chunked-sha256-v1:"

// Run performs one population. The returned error has been redacted.
func Run(ctx context.Context, cfg Config, log *slog.Logger, tr *progress.Tracker) (res *Result, err error) {
	start := time.Now()
	res = &Result{}
	res.Conversion.Tool = "qemu-img"
	secrets := redact.NewSecrets()
	defer func() {
		res.TotalSeconds = time.Since(start).Seconds()
		if err != nil {
			err = secrets.Error(err)
			res.Error = err.Error()
			res.Phase = tr.Snapshot().Phase
			if ctx.Err() != nil {
				tr.SetPhase(progress.PhaseCancelled)
			} else {
				tr.SetPhase(progress.PhaseFailed)
			}
		}
	}()

	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return res, fmt.Errorf("invalid configuration: %w", err)
	}
	if err := cfg.Connection.LoadCredentials(cfg.CredentialsDir); err != nil {
		return res, err
	}
	secrets.Add(cfg.Connection.Password)
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	qemu, err := convert.Find(ctx, cfg.QemuImgPath)
	if err != nil {
		return res, err
	}
	res.Conversion.Version = qemu.Version
	if n, err := transfer.CleanStaleStaging(cfg.StagingDir); err != nil {
		return res, fmt.Errorf("clean staging directory: %w", err)
	} else if n > 0 {
		log.Info("removed stale staging files from an interrupted attempt", "count", n)
	}

	var dest target.Destination
	switch cfg.TargetMode {
	case "file":
		dest = target.NewFile(cfg.TargetPath, cfg.ImageName)
	case "block":
		b := target.NewBlock(cfg.TargetPath)
		b.ZeroTail = cfg.ZeroTail
		b.AllowRegularFile = cfg.AllowRegularFileTarget
		dest = b
	}
	res.Destination.Kind = string(dest.Kind())
	committed := false
	defer func() {
		if !committed {
			dest.Abort()
		} else {
			_ = dest.Close()
		}
	}()

	tr.SetPhase(progress.PhaseDiscovering)
	c, err := vsphere.Connect(ctx, cfg.Connection)
	if err != nil {
		return res, err
	}
	secrets = c.Secrets
	defer c.Logout(context.WithoutCancel(ctx))
	about := c.About()
	res.Server.APIType, res.Server.Version, res.Server.Build = about.ApiType, about.Version, about.Build
	log.Info("connected", "apiType", about.ApiType, "version", about.Version, "build", about.Build)

	ref, err := c.ResolveVM(ctx, cfg.VM)
	if err != nil {
		return res, err
	}
	vm, err := c.InspectVM(ctx, ref)
	if err != nil {
		return res, err
	}
	res.VM.MoRef, res.VM.Name, res.VM.InstanceUUID = vm.Ref.Value, vm.Name, vm.InstanceUUID
	if err := vm.CheckColdSource(); err != nil {
		return res, err
	}
	disk, err := vm.SelectDisk(cfg.Disk)
	if err != nil {
		return res, err
	}
	res.Disk.Key, res.Disk.BusLocation, res.Disk.FileName, res.Disk.CapacityBytes = disk.Key, disk.BusLocation, disk.FileName, disk.CapacityBytes
	res.VirtualSize = disk.CapacityBytes
	if cfg.ExpectedCapacityBytes != 0 && cfg.ExpectedCapacityBytes != disk.CapacityBytes {
		return res, fmt.Errorf("disk %d has capacity %d bytes, expected %d", disk.Key, disk.CapacityBytes, cfg.ExpectedCapacityBytes)
	}
	log.Info("selected source disk", "vm", vm.Name, "moRef", vm.Ref.Value, "disk", disk.Key, "bus", disk.BusLocation,
		"capacityBytes", disk.CapacityBytes, "changeVersion", vm.ChangeVersion)

	// Destination capacity and exclusivity are checked before the lease.
	if err := dest.Prepare(disk.CapacityBytes); err != nil {
		return res, err
	}
	staging, err := transfer.NewStaging(cfg.StagingDir, cfg.StagingLimitBytes, 1<<20)
	if err != nil {
		return res, err
	}
	defer func() {
		if rerr := staging.Remove(); rerr != nil {
			log.Warn("could not remove staging file", "error", rerr)
		}
	}()
	log.Info("staging", "path", staging.Path(), "limitBytes", staging.Limit())

	tr.SetPhase(progress.PhaseLeasing)
	lease, err := c.ExportVM(ctx, vm.Ref, cfg.LeaseReadyTimeout)
	if err != nil {
		return res, err
	}
	defer func() {
		if !lease.Finished() {
			cause := err
			if cause == nil {
				cause = errors.New("worker exited before completing the lease")
			}
			if aerr := lease.Abort(context.WithoutCancel(ctx), cause); aerr != nil {
				log.Warn("lease abort failed", "error", aerr)
			} else {
				log.Info("lease aborted")
			}
		}
	}()
	log.Info("lease ready", "lease", lease.Reference().Value, "timeoutSeconds", lease.Info().LeaseTimeout,
		"totalDiskCapacityKB", lease.Info().TotalDiskCapacityInKB, "deviceURLs", len(lease.Info().DeviceUrl))

	xctx, xcancel := context.WithCancelCause(ctx)
	defer xcancel(nil)
	interval := cfg.KeepaliveInterval
	if interval == 0 {
		interval = vsphere.KeepaliveInterval(lease.Timeout())
	}
	ka := lease.StartKeepalive(interval, cfg.KeepaliveMaxFailures, tr.LeasePercent, func(e error) {
		log.Error("lease keepalive failed; cancelling transfer", "error", secrets.String(e.Error()))
		xcancel(e)
	})
	defer ka.Stop()
	keepaliveErr := func(e error) error {
		if cause := context.Cause(xctx); cause != nil && !errors.Is(cause, context.Canceled) && ctx.Err() == nil {
			return fmt.Errorf("%w (transfer interrupted: %v)", cause, e)
		}
		return e
	}

	dev, err := vsphere.MatchDeviceURL(vm, disk, lease.Info().DeviceUrl)
	if err != nil {
		return res, err
	}
	res.Disk.LeaseKey = dev.Key
	u, err := c.PrepareNFCURL(dev)
	if err != nil {
		return res, err
	}
	// The VM must not have changed between inspection and lease creation.
	if err := checkUnchanged(xctx, c, vm, disk); err != nil {
		return res, keepaliveErr(err)
	}

	tr.SetPhase(progress.PhaseDownloading)
	log.Info("downloading", "url", redact.URL(u), "leaseKey", dev.Key, "targetId", dev.TargetId)
	dlStart := time.Now()
	resp, err := transfer.Get(xctx, &c.Client.Client.Client, u, transfer.Options{IdleTimeout: cfg.IdleTimeout})
	if err != nil {
		return res, keepaliveErr(fmt.Errorf("export request for %s: %w", redact.URL(u), err))
	}
	res.Export.ContentType = resp.Header.Get("Content-Type")
	res.Export.ContentLength = resp.ContentLength
	h1, h256 := sha1.New(), sha256.New()
	tee := io.TeeReader(resp.Body, io.MultiWriter(staging, h1, h256))
	dec, err := vmdk.Decode(tee, vmdk.DecodeOptions{
		OnHeader: func(h vmdk.Header, d vmdk.Descriptor) error {
			if h.VirtualSize() != disk.CapacityBytes {
				return fmt.Errorf("%w: exported disk has virtual size %d bytes but vSphere reports capacity %d", vsphere.ErrSourceState, h.VirtualSize(), disk.CapacityBytes)
			}
			log.Info("export format", "createType", d.CreateType, "version", h.Version, "grainBytes", h.GrainBytes(), "virtualSize", h.VirtualSize())
			return nil
		},
		OnProgress: func(p vmdk.Progress) { tr.Download(p.StreamBytes, p.GuestOffset, p.VirtualSize) },
	})
	resp.Body.Close()
	if err != nil {
		return res, keepaliveErr(fmt.Errorf("export stream: %w", err))
	}
	if err := staging.Finish(); err != nil {
		return res, fmt.Errorf("flush staging file: %w", err)
	}
	if staging.Size() != dec.StreamBytes+dec.TrailingBytes {
		return res, fmt.Errorf("staged %d bytes but decoded %d", staging.Size(), dec.StreamBytes+dec.TrailingBytes)
	}
	dl := time.Since(dlStart)
	res.Export.Bytes = staging.Size()
	res.Export.SHA1, res.Export.SHA256 = hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h256.Sum(nil))
	res.Export.Grains, res.Export.AllocatedBytes, res.Export.Ordered = dec.Grains, dec.AllocatedBytes, dec.Ordered
	res.Export.Seconds = dl.Seconds()
	if dl > 0 {
		res.Export.BytesPerSecond = float64(res.Export.Bytes) / dl.Seconds()
	}
	log.Info("export received", "bytes", res.Export.Bytes, "seconds", res.Export.Seconds, "bytesPerSecond", int64(res.Export.BytesPerSecond),
		"grains", dec.Grains, "allocatedBytes", dec.AllocatedBytes, "ordered", dec.Ordered)

	if err := verifyManifest(xctx, lease, dev.Key, res, h1, h256, cfg.RequireManifest); err != nil {
		return res, keepaliveErr(err)
	}
	log.Info("manifest", "status", res.Manifest.Status, "algorithm", res.Manifest.Algorithm)

	guestDigest := dec.Digest
	if guestDigest == nil {
		tr.SetPhase(progress.PhaseDecoding)
		f, err := os.Open(staging.Path())
		if err != nil {
			return res, err
		}
		ra, err := vmdk.DecodeAt(f, staging.Size(), *dec.Footer, vmdk.DecodeOptions{})
		f.Close()
		if err != nil {
			return res, fmt.Errorf("random-access decode of staged stream: %w", err)
		}
		guestDigest = ra.Digest
	}
	res.GuestDigest = digestPrefix + hex.EncodeToString(guestDigest)

	tr.SetPhase(progress.PhaseConverting)
	if _, err := qemu.CheckStaged(xctx, staging.Path(), disk.CapacityBytes); err != nil {
		return res, keepaliveErr(err)
	}
	convStart := time.Now()
	copts := convert.ConvertOptions{Target: convert.NewFile, Progress: tr.Convert, Coroutines: cfg.Coroutines}
	if dest.Existing() {
		copts.Target = convert.ExistingDevice
	}
	if err := qemu.Convert(xctx, staging.Path(), dest.ConvertPath(), copts); err != nil {
		return res, keepaliveErr(err)
	}
	res.Conversion.Seconds = time.Since(convStart).Seconds()

	tr.SetPhase(progress.PhaseFlushing)
	if err := dest.Flush(xctx); err != nil {
		return res, keepaliveErr(err)
	}
	if b, ok := dest.(*target.Block); ok {
		res.Destination.DeviceBytes, res.Destination.TailZeroMode = b.DeviceSize(), b.TailZeroMode
	}

	tr.SetPhase(progress.PhaseVerifying)
	vr, err := dest.Verify(xctx, func(done, _ int64) { tr.Verify(done) })
	if err != nil {
		return res, keepaliveErr(fmt.Errorf("read back destination: %w", err))
	}
	res.Destination.VerifyMode, res.Destination.Seconds = vr.Mode, vr.Elapsed.Seconds()
	res.DestinationDigest = digestPrefix + hex.EncodeToString(vr.Digest)
	if res.DestinationDigest != res.GuestDigest {
		return res, fmt.Errorf("destination content does not match the exported disk (destination %s, source %s)", res.DestinationDigest, res.GuestDigest)
	}
	if err := checkUnchanged(xctx, c, vm, disk); err != nil {
		return res, keepaliveErr(err)
	}
	if err := dest.Commit(); err != nil {
		return res, err
	}
	committed = true

	tr.SetPhase(progress.PhaseCompleting)
	ka.Stop()
	res.KeepaliveRenewals = ka.Calls()
	if kerr := ka.Err(); kerr != nil {
		return res, kerr
	}
	if err := lease.Progress(ctx, 100); err != nil {
		log.Warn("final lease progress update failed", "error", err)
	}
	if err := lease.Complete(ctx); err != nil {
		return res, fmt.Errorf("complete lease: %w", err)
	}
	res.Validated = true
	tr.SetPhase(progress.PhaseSucceeded)
	log.Info("population validated", "virtualSize", res.VirtualSize, "guestDigest", res.GuestDigest)
	return res, nil
}

// checkUnchanged re-reads the VM and fails if it was powered on,
// reconfigured, or the selected disk changed.
func checkUnchanged(ctx context.Context, c *vsphere.Client, before *vsphere.VMInfo, disk *vsphere.DiskInfo) error {
	now, err := c.InspectVM(ctx, before.Ref)
	if err != nil {
		return err
	}
	if now.PowerState != before.PowerState {
		return fmt.Errorf("%w: power state changed from %s to %s during export", vsphere.ErrSourceState, before.PowerState, now.PowerState)
	}
	if now.ChangeVersion != before.ChangeVersion {
		return fmt.Errorf("%w: VM configuration changed during export (changeVersion %q -> %q)", vsphere.ErrSourceState, before.ChangeVersion, now.ChangeVersion)
	}
	if now.HasSnapshots != before.HasSnapshots {
		return fmt.Errorf("%w: snapshot state changed during export", vsphere.ErrSourceState)
	}
	for _, d := range now.Disks {
		if d.Key == disk.Key {
			if d.CapacityBytes != disk.CapacityBytes || d.FileName != disk.FileName || d.BusLocation != disk.BusLocation {
				return fmt.Errorf("%w: disk %d changed during export", vsphere.ErrSourceState, disk.Key)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: disk %d disappeared during export", vsphere.ErrSourceState, disk.Key)
}

func verifyManifest(ctx context.Context, lease *vsphere.Lease, key string, res *Result, h1, h256 hash.Hash, require bool) error {
	entries, err := lease.Manifest(ctx)
	if err != nil {
		if errors.Is(err, vsphere.ErrManifestUnavailable) {
			res.Manifest.Status = "unavailable"
			if require {
				return fmt.Errorf("manifest required but unavailable: %w", err)
			}
			return nil
		}
		return fmt.Errorf("read lease manifest: %w", err)
	}
	for _, e := range entries {
		if e.Key != key {
			continue
		}
		if e.Size != 0 && e.Size != res.Export.Bytes {
			return fmt.Errorf("manifest size %d does not match %d received bytes", e.Size, res.Export.Bytes)
		}
		sha1Hex, sha256Hex := hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h256.Sum(nil))
		switch {
		case e.Checksum != "" && strings.EqualFold(e.ChecksumType, "SHA256"):
			res.Manifest.Algorithm = "SHA256"
			if e.Checksum != sha256Hex {
				return errors.New("export stream SHA-256 does not match the vSphere manifest")
			}
		case e.Checksum != "" && strings.EqualFold(e.ChecksumType, "SHA1"):
			res.Manifest.Algorithm = "SHA1"
			if e.Checksum != sha1Hex {
				return errors.New("export stream SHA-1 does not match the vSphere manifest")
			}
		case e.SHA1 != "":
			res.Manifest.Algorithm = "SHA1"
			if e.SHA1 != sha1Hex {
				return errors.New("export stream SHA-1 does not match the vSphere manifest")
			}
		default:
			res.Manifest.Status = "no-checksum"
			if require {
				return errors.New("manifest entry has no supported checksum")
			}
			return nil
		}
		res.Manifest.Status = "verified"
		return nil
	}
	res.Manifest.Status = "no-entry"
	if require {
		return fmt.Errorf("manifest has no entry for %s", key)
	}
	return nil
}
