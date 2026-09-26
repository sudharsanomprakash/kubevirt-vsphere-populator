package vsphere

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Lease wraps an export HttpNfcLease.
type Lease struct {
	c     *Client
	lease *nfc.Lease
	info  types.HttpNfcLeaseInfo
	done  atomic.Bool
}

// ExportVM requests an export lease for the VM and waits until it is ready.
// If the lease does not become ready within readyTimeout it is aborted.
func (c *Client) ExportVM(ctx context.Context, vm types.ManagedObjectReference, readyTimeout time.Duration) (*Lease, error) {
	l, err := object.NewVirtualMachine(c.Client, vm).Export(ctx)
	if err != nil {
		return nil, c.Secrets.Error(fmt.Errorf("ExportVm: %w", err))
	}
	lease := &Lease{c: c, lease: l}
	wctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	info, err := lease.waitReady(wctx)
	if err != nil {
		if wctx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("lease not ready within %s: %w", readyTimeout, err)
		}
		_ = lease.Abort(context.WithoutCancel(ctx), err)
		return nil, c.Secrets.Error(err)
	}
	lease.info = *info
	for _, d := range info.DeviceUrl {
		if u, err := c.ParseURL(d.Url); err == nil {
			c.Secrets.AddURL(u)
		}
	}
	return lease, nil
}

// waitReady waits for the lease to leave the initializing state. It is
// equivalent to nfc.Lease.Wait but keeps the raw lease info.
func (l *Lease) waitReady(ctx context.Context) (*types.HttpNfcLeaseInfo, error) {
	var state types.HttpNfcLeaseState
	var info *types.HttpNfcLeaseInfo
	var fault *types.LocalizedMethodFault
	pc := property.DefaultCollector(l.c.Client)
	err := property.Wait(ctx, pc, l.lease.Reference(), []string{"state", "info", "error"}, func(pcs []types.PropertyChange) bool {
		for _, ch := range pcs {
			if ch.Val == nil {
				continue
			}
			switch ch.Name {
			case "state":
				state = ch.Val.(types.HttpNfcLeaseState)
			case "info":
				v := ch.Val.(types.HttpNfcLeaseInfo)
				info = &v
			case "error":
				v := ch.Val.(types.LocalizedMethodFault)
				fault = &v
			}
		}
		return state != "" && state != types.HttpNfcLeaseStateInitializing
	})
	if err != nil {
		return nil, err
	}
	switch state {
	case types.HttpNfcLeaseStateReady:
		if info == nil {
			return nil, errors.New("lease is ready but has no info")
		}
		return info, nil
	case types.HttpNfcLeaseStateError:
		if fault != nil {
			return nil, fmt.Errorf("lease entered error state: %s", faultMessage(fault))
		}
		return nil, errors.New("lease entered error state")
	default:
		return nil, fmt.Errorf("unexpected lease state %q", state)
	}
}

func faultMessage(f *types.LocalizedMethodFault) string {
	if f == nil {
		return ""
	}
	if f.LocalizedMessage != "" {
		return f.LocalizedMessage
	}
	if f.Fault != nil {
		return fmt.Sprintf("%T", f.Fault)
	}
	return "unknown fault"
}

// Info returns the lease info (device URLs are sensitive; redact before output).
func (l *Lease) Info() types.HttpNfcLeaseInfo { return l.info }

// Reference returns the lease's managed object reference.
func (l *Lease) Reference() types.ManagedObjectReference { return l.lease.Reference() }

// Timeout returns the lease timeout reported by the server.
func (l *Lease) Timeout() time.Duration { return time.Duration(l.info.LeaseTimeout) * time.Second }

// Progress reports transfer progress, which also renews the lease.
func (l *Lease) Progress(ctx context.Context, percent int32) error {
	percent = max(0, min(100, percent))
	return l.c.Secrets.Error(l.lease.Progress(ctx, percent))
}

// Complete marks the export as successful. It may only be called once.
func (l *Lease) Complete(ctx context.Context) error {
	if !l.done.CompareAndSwap(false, true) {
		return errors.New("lease already completed or aborted")
	}
	return l.c.Secrets.Error(l.lease.Complete(ctx))
}

// Abort releases the lease with a failure. Calling it after Complete or
// Abort is a no-op.
func (l *Lease) Abort(ctx context.Context, cause error) error {
	if !l.done.CompareAndSwap(false, true) {
		return nil
	}
	var fault *types.LocalizedMethodFault
	if cause != nil {
		msg := l.c.Secrets.String(cause.Error())
		fault = &types.LocalizedMethodFault{
			Fault:            &types.SystemError{Reason: truncate(msg, 512)},
			LocalizedMessage: truncate(msg, 512),
		}
	}
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return l.c.Secrets.Error(l.lease.Abort(actx, fault))
}

// Finished reports whether Complete or Abort has been called.
func (l *Lease) Finished() bool { return l.done.Load() }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// State reads the current lease state and error.
func (l *Lease) State(ctx context.Context) (types.HttpNfcLeaseState, error) {
	var m mo.HttpNfcLease
	if err := property.DefaultCollector(l.c.Client).RetrieveOne(ctx, l.lease.Reference(), []string{"state", "error"}, &m); err != nil {
		return "", l.c.Secrets.Error(err)
	}
	if m.State == types.HttpNfcLeaseStateError && m.Error != nil {
		return m.State, fmt.Errorf("lease error: %s", l.c.Secrets.String(faultMessage(m.Error)))
	}
	return m.State, nil
}

// ManifestEntry is a manifest checksum for one device.
type ManifestEntry struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	SHA1         string `json:"sha1,omitempty"`
	Checksum     string `json:"checksum,omitempty"`
	ChecksumType string `json:"checksumType,omitempty"`
}

// ErrManifestUnavailable is returned when the server does not provide a manifest.
var ErrManifestUnavailable = errors.New("lease manifest unavailable")

// Manifest returns the server's manifest. Servers that do not implement
// HttpNfcLeaseGetManifest yield ErrManifestUnavailable.
func (l *Lease) Manifest(ctx context.Context) ([]ManifestEntry, error) {
	entries, err := l.lease.GetManifest(ctx)
	if err != nil {
		if MethodFault(err) != nil {
			return nil, fmt.Errorf("%w: %w", ErrManifestUnavailable, l.c.Secrets.Error(err))
		}
		return nil, l.c.Secrets.Error(err)
	}
	out := make([]ManifestEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, ManifestEntry{Key: e.Key, Size: e.Size, SHA1: strings.ToLower(e.Sha1),
			Checksum: strings.ToLower(e.Checksum), ChecksumType: e.ChecksumType})
	}
	return out, nil
}

// Keepalive periodically reports progress to keep the lease alive. Unlike
// govmomi's nfc.LeaseUpdater, it propagates failures: when renewals keep
// failing, or fail with a non-transient fault, OnFailure is called once.
type Keepalive struct {
	lease     *Lease
	interval  time.Duration
	maxFails  int
	percent   func() int32
	onFailure func(error)
	stop      chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	err       error
	calls     atomic.Int64
}

// KeepaliveInterval returns the renewal interval for a lease timeout: a
// sixth of the timeout, capped at 10 seconds and at least one second.
func KeepaliveInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 10 * time.Second
	}
	return max(time.Second, min(10*time.Second, timeout/6))
}

// StartKeepalive starts renewing the lease. percent supplies the current
// progress; onFailure is called (at most once, from the keepalive
// goroutine) when renewal fails permanently.
func (l *Lease) StartKeepalive(interval time.Duration, maxConsecutiveFailures int, percent func() int32, onFailure func(error)) *Keepalive {
	if maxConsecutiveFailures < 1 {
		maxConsecutiveFailures = 1
	}
	k := &Keepalive{lease: l, interval: interval, maxFails: maxConsecutiveFailures, percent: percent, onFailure: onFailure, stop: make(chan struct{})}
	k.wg.Add(1)
	go k.run()
	return k
}

func (k *Keepalive) run() {
	defer k.wg.Done()
	t := time.NewTicker(k.interval)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-k.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), max(k.interval, 5*time.Second))
		err := k.lease.Progress(ctx, k.percent())
		cancel()
		k.calls.Add(1)
		if err == nil {
			fails = 0
			continue
		}
		fails++
		permanent := isPermanentLeaseFault(err)
		if permanent || fails >= k.maxFails {
			ferr := fmt.Errorf("lease keepalive failed (%d consecutive failures): %w", fails, err)
			k.mu.Lock()
			k.err = ferr
			k.mu.Unlock()
			if k.onFailure != nil {
				k.onFailure(ferr)
			}
			return
		}
	}
}

// Stop ends renewals and waits for the goroutine to exit.
func (k *Keepalive) Stop() {
	select {
	case <-k.stop:
	default:
		close(k.stop)
	}
	k.wg.Wait()
}

// Err returns the failure that ended the keepalive, if any.
func (k *Keepalive) Err() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Calls returns the number of renewal attempts made.
func (k *Keepalive) Calls() int64 { return k.calls.Load() }

// MethodFault extracts the vSphere fault from err (searching wrapped
// errors), or returns nil.
func MethodFault(err error) types.BaseMethodFault {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if soap.IsSoapFault(e) {
			// SOAP fault details decode as values (types.InvalidState), while
			// BaseMethodFault is implemented by pointers.
			v := soap.ToSoapFault(e).VimFault()
			if f, ok := v.(types.BaseMethodFault); ok {
				return f
			}
			rv := reflect.ValueOf(v)
			if rv.IsValid() && rv.Kind() != reflect.Pointer {
				p := reflect.New(rv.Type())
				p.Elem().Set(rv)
				if f, ok := p.Interface().(types.BaseMethodFault); ok {
					return f
				}
			}
			return nil
		}
		if soap.IsVimFault(e) {
			return soap.ToVimFault(e)
		}
	}
	return nil
}

// isPermanentLeaseFault reports faults that mean the lease is gone or
// unusable (as opposed to a transient network error).
func isPermanentLeaseFault(err error) bool {
	switch MethodFault(err).(type) {
	case *types.ManagedObjectNotFound, *types.InvalidState, *types.Timedout, *types.NotAuthenticated, *types.NoPermission:
		return true
	}
	return false
}

// DeviceKey is a parsed HttpNfcLeaseDeviceUrl key of the form
// "/<vm-moref>/<ControllerType><N>:<unit>".
type DeviceKey struct {
	VM             string
	ControllerType string
	Controller     int
	Unit           int
}

var deviceKeyRE = regexp.MustCompile(`^/([^/]+)/([A-Za-z]+)([0-9]+):([0-9]+)$`)

// ParseDeviceKey parses a lease device key.
func ParseDeviceKey(key string) (DeviceKey, error) {
	m := deviceKeyRE.FindStringSubmatch(key)
	if m == nil {
		return DeviceKey{}, fmt.Errorf("unrecognized lease device key %q", key)
	}
	ctrl, _ := strconv.Atoi(m[3])
	unit, _ := strconv.Atoi(m[4])
	return DeviceKey{VM: m[1], ControllerType: m[2], Controller: ctrl, Unit: unit}, nil
}

// MatchDeviceURL returns the lease device URL for the selected disk. The key
// must parse, belong to this VM, name the disk's controller type, controller
// number and unit, and be flagged as a disk. Exactly one URL must match, and
// the number of disk URLs must equal the number of VM disks. There is no
// fallback to URL order.
func MatchDeviceURL(vm *VMInfo, disk *DiskInfo, urls []types.HttpNfcLeaseDeviceUrl) (types.HttpNfcLeaseDeviceUrl, error) {
	var zero types.HttpNfcLeaseDeviceUrl
	if disk.ExpectedLeaseKey == "" {
		return zero, fmt.Errorf("%w: cannot determine the lease key for disk %d: %s", ErrUnsupported, disk.Key, disk.UnsupportedReason)
	}
	var matches []types.HttpNfcLeaseDeviceUrl
	diskURLs := 0
	var keys []string
	for _, u := range urls {
		keys = append(keys, u.Key)
		isDisk := u.Disk != nil && *u.Disk
		if !isDisk {
			continue
		}
		diskURLs++
		k, err := ParseDeviceKey(u.Key)
		if err != nil {
			return zero, fmt.Errorf("%w: %w", ErrSourceState, err)
		}
		if k.VM != vm.Ref.Value {
			return zero, fmt.Errorf("%w: lease device key %q refers to %q, not VM %s", ErrSourceState, u.Key, k.VM, vm.Ref.Value)
		}
		if u.Key == disk.ExpectedLeaseKey {
			matches = append(matches, u)
		}
	}
	if diskURLs != len(vm.Disks) {
		return zero, fmt.Errorf("%w: lease has %d disk URLs but the VM has %d disks (keys: %s)", ErrSourceState, diskURLs, len(vm.Disks), strings.Join(keys, ", "))
	}
	switch len(matches) {
	case 0:
		return zero, fmt.Errorf("%w: no lease device URL has key %q (keys: %s)", ErrSourceState, disk.ExpectedLeaseKey, strings.Join(keys, ", "))
	case 1:
		return matches[0], nil
	default:
		return zero, fmt.Errorf("%w: %d lease device URLs have key %q", ErrSourceState, len(matches), disk.ExpectedLeaseKey)
	}
}
