// Package fakevsphere runs the govmomi vCenter simulator (vcsim) extended
// with an ExportVm implementation and an NFC download server with injectable
// faults. vcsim itself implements HttpNfcLease only for imports and snapshot
// exports, and uses device keys ("/vm-42/disk:0") that differ from the
// documented vSphere format; this package models the documented behavior
// ("/vm-42/VirtualLsiLogicController0:0", streamOptimized disk streams, lease
// states, progress renewal, manifests).
//
// It is a test double. Passing tests against it do not prove compatibility
// with any real vCenter or ESXi release.
package fakevsphere

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vmdk"
)

// Credentials accepted by the fake.
const (
	Username = "migrator@vsphere.example"
	Password = "not-a-real-password-1234"
)

// NFCBehavior controls how disk downloads are served.
type NFCBehavior struct {
	// HonorRange answers Range requests with 206 Partial Content. When
	// false, Range headers are ignored and the full stream is returned with 200.
	HonorRange bool
	// Status, when non-zero, is returned instead of 200.
	Status int
	// TruncateAfter closes the connection after this many body bytes.
	TruncateAfter int64
	// StallAfter stops sending (without closing) after this many bytes.
	StallAfter int64
	// BytesPerSecond throttles the response (0 = unlimited).
	BytesPerSecond int64
	// ContentLength sends a Content-Length header (otherwise chunked).
	ContentLength bool
	// ChangeOnRepeat serves a different stream on every GET after the first
	// for the same device (simulating content that changes between requests).
	ChangeOnRepeat bool
	// CorruptAt flips a byte at this stream offset (-1 or 0 disables; use
	// an offset beyond the header).
	CorruptAt int64
	// ContentType is sent as Content-Type (default application/octet-stream).
	ContentType string
}

// ManifestMode controls HttpNfcLeaseGetManifest.
type ManifestMode string

const (
	ManifestSHA1        ManifestMode = "sha1"
	ManifestSHA256      ManifestMode = "sha256"
	ManifestUnsupported ManifestMode = "unsupported"
	ManifestWrong       ManifestMode = "wrong"
)

// LeaseBehavior controls lease lifecycle faults.
type LeaseBehavior struct {
	// InitError makes the lease enter the error state instead of ready.
	InitError string
	// ReadyDelay delays the transition to ready.
	ReadyDelay time.Duration
	// TimeoutSeconds is the leaseTimeout reported in the lease info (default 300).
	TimeoutSeconds int32
	// ExpireWithoutProgress expires the lease when no HttpNfcLeaseProgress
	// arrives for this long (0 disables).
	ExpireWithoutProgress time.Duration
	// ProgressFailAfter makes HttpNfcLeaseProgress fail after this many
	// successful calls (0 disables).
	ProgressFailAfter int
	// ProgressFault is the fault returned once ProgressFailAfter is reached
	// (default InvalidState).
	ProgressFault types.BaseMethodFault
	// Manifest selects the manifest behavior (default sha1).
	Manifest ManifestMode
	// VcsimKeys uses vcsim's "/vm-42/disk:0" key format instead of the documented one.
	VcsimKeys bool
	// ExtraDiskURL adds a disk URL that belongs to no disk.
	ExtraDiskURL bool
	// DropDiskURLs omits the disk URLs with these device keys.
	DropDiskURLs []int32
	// FileSize is reported in each device URL.
	FileSize int64
}

// Disk describes a virtual disk of a fake VM.
type Disk struct {
	// Controller is one of lsilogic, lsilogic-sas, pvscsi, buslogic, sata, nvme.
	Controller string
	Bus        int32
	Unit       int32
	// Raw is the guest disk content; its length is the capacity unless
	// CapacityBytes is set.
	Raw []byte
	// Stream, if set, is served verbatim instead of an encoding of Raw.
	Stream        []byte
	CapacityBytes int64
	DiskMode      string
	// Key is filled in by AddVM.
	Key int32
}

// VMSpec describes a fake VM.
type VMSpec struct {
	Name     string
	Folder   string // optional sub-folder of the datacenter VM folder
	Disks    []Disk
	PowerOn  bool
	Snapshot bool
}

// VM is a created fake VM.
type VM struct {
	Ref          types.ManagedObjectReference
	Name         string
	InstanceUUID string
	BIOSUUID     string
	Disks        []Disk
}

// Request records one NFC request.
type Request struct {
	Method string
	Target string
	Range  string
	Status int
	Bytes  int64
}

// LeaseRecord records what happened to a lease.
type LeaseRecord struct {
	Ref          types.ManagedObjectReference
	VM           types.ManagedObjectReference
	Progress     []int32
	Completed    bool
	Aborted      bool
	AbortMessage string
	Expired      bool
	Requests     []Request
	ticket       string
	streams      map[string][]byte // targetId -> stream
	served       map[string]int
	sums         map[string][2][]byte
	lastRenew    time.Time
}

// Server is a running fake.
type Server struct {
	ESX bool

	model  *simulator.Model
	sdk    *simulator.Server
	nfc    *httptest.Server
	client *vim25.Client

	// URL is the SDK URL (without credentials).
	URL *url.URL
	// CAFile trusts the SDK server certificate.
	CAFile string
	// Thumbprint is the SHA-1 thumbprint of the SDK server certificate.
	Thumbprint string
	// NFCThumbprint is the SHA-1 thumbprint of the separate NFC server
	// certificate, which is not signed by CAFile (like an ESXi host
	// certificate that is not trusted by the client).
	NFCThumbprint string
	// NFCCAFile trusts the NFC server certificate.
	NFCCAFile string

	mu     sync.Mutex
	nfcB   NFCBehavior
	leaseB LeaseBehavior
	vms    map[string]*VM
	leases map[string]*LeaseRecord
	byTkt  map[string]*LeaseRecord
	dir    string
	stop   chan struct{}
}

// Options configures Start.
type Options struct {
	// ESX models a standalone ESXi host: lease URLs use "*" as host.
	ESX bool
}

// Start launches the simulator and the NFC server.
func Start(opts Options) (*Server, error) {
	m := simulator.VPX()
	if opts.ESX {
		m = simulator.ESX()
	}
	m.Machine = 0
	if !opts.ESX {
		m.Host = 0 // cluster hosts only, so there is a single resource pool
	}
	m.Autostart = false
	m.App = 0
	m.Pod = 0
	m.Pool = 0
	m.Portgroup = 0
	m.PortgroupNSX = 0
	m.OpaqueNetwork = 0
	if err := m.Create(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "fakevsphere-")
	if err != nil {
		return nil, err
	}
	s := &Server{ESX: opts.ESX, model: m, vms: map[string]*VM{}, leases: map[string]*LeaseRecord{}, byTkt: map[string]*LeaseRecord{},
		dir: dir, stop: make(chan struct{}), nfcB: NFCBehavior{ContentLength: false}}

	m.Service.TLS = new(tls.Config)
	m.Service.Listen = &url.URL{User: url.UserPassword(Username, Password)}
	m.Service.ServeMux = http.NewServeMux()
	m.Service.ServeMux.Handle("/fake-nfc/", http.HandlerFunc(s.serveNFC))
	m.Map().Handler = s.handler
	s.sdk = m.Service.NewServer()

	u := *s.sdk.URL
	u.User = nil
	s.URL = &u
	caFile, err := s.sdk.CertificateFile()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.CAFile = caFile
	s.Thumbprint = soap.ThumbprintSHA1(s.sdk.Certificate())

	cert, certPEM, err := selfSigned()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.NFCCAFile = filepath.Join(dir, "nfc-ca.pem")
	if err := os.WriteFile(s.NFCCAFile, certPEM, 0o600); err != nil {
		s.Close()
		return nil, err
	}
	s.nfc = httptest.NewUnstartedServer(http.HandlerFunc(s.serveNFC))
	s.nfc.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.nfc.StartTLS()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	s.NFCThumbprint = soap.ThumbprintSHA1(leaf)

	// An administrative client used to build the inventory.
	sc := soap.NewClient(s.sdk.URL, true)
	vc, err := vim25.NewClient(context.Background(), sc)
	if err != nil {
		s.Close()
		return nil, err
	}
	if err := session.NewManager(vc).Login(context.Background(), url.UserPassword(Username, Password)); err != nil {
		s.Close()
		return nil, err
	}
	s.client = vc
	go s.expiryLoop()
	return s, nil
}

// Close stops all servers and removes temporary files.
func (s *Server) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	if s.nfc != nil {
		s.nfc.CloseClientConnections()
		s.nfc.Close()
	}
	if s.sdk != nil {
		s.sdk.Close()
	}
	s.model.Remove()
	_ = os.RemoveAll(s.dir)
}

// SetNFCBehavior replaces the NFC behavior.
func (s *Server) SetNFCBehavior(b NFCBehavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nfcB = b
}

// SetLeaseBehavior replaces the lease behavior for leases created afterwards.
func (s *Server) SetLeaseBehavior(b LeaseBehavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseB = b
}

// Leases returns copies of all lease records.
func (s *Server) Leases() []LeaseRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LeaseRecord
	for _, l := range s.leases {
		c := *l
		c.Progress = append([]int32(nil), l.Progress...)
		c.Requests = append([]Request(nil), l.Requests...)
		out = append(out, c)
	}
	return out
}

// Client returns the administrative client (for test assertions).
func (s *Server) Client() *vim25.Client { return s.client }

var controllerTypes = map[string]func(bus int32, key int32) types.BaseVirtualDevice{
	"lsilogic": func(bus, key int32) types.BaseVirtualDevice {
		return &types.VirtualLsiLogicController{VirtualSCSIController: scsi(bus, key)}
	},
	"lsilogic-sas": func(bus, key int32) types.BaseVirtualDevice {
		return &types.VirtualLsiLogicSASController{VirtualSCSIController: scsi(bus, key)}
	},
	"pvscsi": func(bus, key int32) types.BaseVirtualDevice {
		return &types.ParaVirtualSCSIController{VirtualSCSIController: scsi(bus, key)}
	},
	"buslogic": func(bus, key int32) types.BaseVirtualDevice {
		return &types.VirtualBusLogicController{VirtualSCSIController: scsi(bus, key)}
	},
	"sata": func(bus, key int32) types.BaseVirtualDevice {
		return &types.VirtualAHCIController{VirtualSATAController: types.VirtualSATAController{
			VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: key}, BusNumber: bus}}}
	},
	"nvme": func(bus, key int32) types.BaseVirtualDevice {
		return &types.VirtualNVMEController{VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: key}, BusNumber: bus}}
	},
}

func scsi(bus, key int32) types.VirtualSCSIController {
	return types.VirtualSCSIController{
		VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: key}, BusNumber: bus},
		SharedBus:         types.VirtualSCSISharingNoSharing,
	}
}

// AddVM creates a powered-off VM with the given disks.
func (s *Server) AddVM(ctx context.Context, spec VMSpec) (*VM, error) {
	finder := find.NewFinder(s.client, true)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, err
	}
	finder.SetDatacenter(dc)
	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, err
	}
	folder := folders.VmFolder
	if spec.Folder != "" {
		if f, err := finder.Folder(ctx, path.Join(folder.InventoryPath, spec.Folder)); err == nil {
			folder = f
		} else if folder, err = folders.VmFolder.CreateFolder(ctx, spec.Folder); err != nil {
			return nil, err
		}
	}
	pool, err := finder.DefaultResourcePool(ctx)
	if err != nil {
		return nil, err
	}
	ds, err := finder.DefaultDatastore(ctx)
	if err != nil {
		return nil, err
	}

	var changes []types.BaseVirtualDeviceConfigSpec
	ctrlKeys := map[string]int32{}
	nextKey := int32(-100)
	for i, d := range spec.Disks {
		id := fmt.Sprintf("%s/%d", d.Controller, d.Bus)
		if _, ok := ctrlKeys[id]; !ok {
			mk, ok := controllerTypes[d.Controller]
			if !ok {
				return nil, fmt.Errorf("unknown controller %q", d.Controller)
			}
			ctrlKeys[id] = nextKey
			changes = append(changes, &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: mk(d.Bus, nextKey)})
			nextKey--
		}
		capacity := d.CapacityBytes
		if capacity == 0 {
			capacity = int64(len(d.Raw))
		}
		mode := d.DiskMode
		if mode == "" {
			mode = string(types.VirtualDiskModePersistent)
		}
		unit := d.Unit
		disk := &types.VirtualDisk{
			VirtualDevice: types.VirtualDevice{
				Key:           nextKey,
				ControllerKey: ctrlKeys[id],
				UnitNumber:    &unit,
				Backing: &types.VirtualDiskFlatVer2BackingInfo{
					VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
						FileName: fmt.Sprintf("[%s] %s%s/%s_%d.vmdk", ds.Name(), strings.ReplaceAll(spec.Folder, "/", "-"), spec.Name, spec.Name, i),
					},
					DiskMode:        mode,
					ThinProvisioned: types.NewBool(true),
				},
			},
			CapacityInBytes: capacity,
			CapacityInKB:    capacity / 1024,
		}
		nextKey--
		changes = append(changes, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationAdd, FileOperation: types.VirtualDeviceConfigSpecFileOperationCreate, Device: disk})
	}
	cs := types.VirtualMachineConfigSpec{
		Name: spec.Name, GuestId: "otherGuest64", NumCPUs: 1, MemoryMB: 64,
		Files:        &types.VirtualMachineFileInfo{VmPathName: fmt.Sprintf("[%s] %s%s", ds.Name(), strings.ReplaceAll(spec.Folder, "/", "-"), spec.Name)},
		DeviceChange: changes,
	}
	task, err := folder.CreateVM(ctx, cs, pool, nil)
	if err != nil {
		return nil, err
	}
	info, err := task.WaitForResult(ctx)
	if err != nil {
		return nil, fmt.Errorf("create VM: %w", err)
	}
	ref := info.Result.(types.ManagedObjectReference)
	vmObj := object.NewVirtualMachine(s.client, ref)
	var mvm mo.VirtualMachine
	if err := vmObj.Properties(ctx, ref, []string{"config"}, &mvm); err != nil {
		return nil, err
	}
	out := &VM{Ref: ref, Name: spec.Name, InstanceUUID: mvm.Config.InstanceUuid, BIOSUUID: mvm.Config.Uuid}
	devs := object.VirtualDeviceList(mvm.Config.Hardware.Device)
	for _, d := range spec.Disks {
		found := false
		for _, dev := range devs.SelectByType((*types.VirtualDisk)(nil)) {
			vd := dev.GetVirtualDevice()
			ctrl := devs.FindByKey(vd.ControllerKey)
			bc, ok := ctrl.(types.BaseVirtualController)
			if !ok || vd.UnitNumber == nil {
				continue
			}
			if *vd.UnitNumber == d.Unit && bc.GetVirtualController().BusNumber == d.Bus && controllerName(ctrl) == d.Controller {
				d.Key = vd.Key
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("created VM lacks disk %s:%d:%d", d.Controller, d.Bus, d.Unit)
		}
		if d.Stream == nil {
			var buf bytes.Buffer
			if _, err := vmdk.WriteStreamOptimized(&buf, bytes.NewReader(d.Raw), int64(len(d.Raw)), vmdk.WriterOptions{}); err != nil {
				return nil, err
			}
			d.Stream = buf.Bytes()
		}
		out.Disks = append(out.Disks, d)
	}
	if spec.Snapshot {
		t, err := vmObj.CreateSnapshot(ctx, "snap", "", false, false)
		if err != nil {
			return nil, err
		}
		if err := t.Wait(ctx); err != nil {
			return nil, err
		}
	}
	if spec.PowerOn {
		t, err := vmObj.PowerOn(ctx)
		if err != nil {
			return nil, err
		}
		if err := t.Wait(ctx); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.vms[ref.Value] = out
	s.mu.Unlock()
	return out, nil
}

func controllerName(d types.BaseVirtualDevice) string {
	switch d.(type) {
	case *types.VirtualLsiLogicController:
		return "lsilogic"
	case *types.VirtualLsiLogicSASController:
		return "lsilogic-sas"
	case *types.ParaVirtualSCSIController:
		return "pvscsi"
	case *types.VirtualBusLogicController:
		return "buslogic"
	case *types.VirtualAHCIController:
		return "sata"
	case *types.VirtualNVMEController:
		return "nvme"
	case *types.VirtualIDEController:
		return "ide"
	}
	return ""
}

// handler intercepts ExportVm calls on virtual machines. vcsim resolves the
// handler of an authenticated call through the session registry, which falls
// back to the global registry, so the exporter is placed in the session
// registry for the duration of the call and removed by ExportVm itself.
func (s *Server) handler(ctx *simulator.Context, m *simulator.Method) (mo.Reference, types.BaseMethodFault) {
	if m.Name == "ExportVm" && m.This.Type == "VirtualMachine" {
		e := &exporter{s: s, ref: m.This}
		if ctx.Session != nil {
			ctx.Session.Registry.Put(e)
		}
		return e, nil
	}
	return nil, nil
}

type exporter struct {
	s   *Server
	ref types.ManagedObjectReference
}

func (e *exporter) Reference() types.ManagedObjectReference { return e.ref }

// ExportVm creates a lease for the VM's disks.
func (e *exporter) ExportVm(ctx *simulator.Context, req *types.ExportVm) soap.HasFault {
	if ctx.Session != nil {
		ctx.Session.Registry.Remove(ctx, e.ref)
	}
	body := &methods.ExportVmBody{}
	vm, ok := ctx.Map.Get(e.ref).(*simulator.VirtualMachine)
	if !ok {
		body.Fault_ = simulator.Fault("", &types.ManagedObjectNotFound{Obj: e.ref})
		return body
	}
	if vm.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		body.Fault_ = simulator.Fault("The operation is not allowed in the current state.",
			&types.InvalidPowerState{RequestedState: types.VirtualMachinePowerStatePoweredOff, ExistingState: vm.Runtime.PowerState})
		return body
	}
	e.s.mu.Lock()
	spec := e.s.vms[e.ref.Value]
	lb := e.s.leaseB
	e.s.mu.Unlock()
	if spec == nil {
		body.Fault_ = simulator.Fault("VM not created by fakevsphere", &types.NotSupported{})
		return body
	}

	lease := &HttpNfcLease{s: e.s}
	lease.HttpNfcLease = mo.HttpNfcLease{State: types.HttpNfcLeaseStateInitializing, InitializeProgress: 0,
		Mode: string(types.HttpNfcLeaseModePull), Capabilities: types.HttpNfcLeaseCapabilities{PullModeSupported: true}}
	ctx.Map.Put(lease)
	ticket := randomHex(16)
	rec := &LeaseRecord{Ref: lease.Self, VM: e.ref, ticket: ticket, streams: map[string][]byte{}, served: map[string]int{},
		sums: map[string][2][]byte{}, lastRenew: time.Now()}
	lease.rec = rec

	devs := object.VirtualDeviceList(vm.Config.Hardware.Device)
	var urls []types.HttpNfcLeaseDeviceUrl
	var totalKB int64
	ordinal := map[string]int{}
	for i, dev := range devs.SelectByType((*types.VirtualDisk)(nil)) {
		disk := dev.(*types.VirtualDisk)
		vd := disk.GetVirtualDevice()
		totalKB += disk.CapacityInKB
		if contains(lb.DropDiskURLs, vd.Key) {
			continue
		}
		ctrl := devs.FindByKey(vd.ControllerKey)
		var key string
		if lb.VcsimKeys {
			key = fmt.Sprintf("/%s/disk:%d", e.ref.Value, ordinal["disk"])
			ordinal["disk"]++
		} else {
			key = fmt.Sprintf("/%s/%s%d:%d", e.ref.Value, reflect.TypeOf(ctrl).Elem().Name(),
				ctrl.(types.BaseVirtualController).GetVirtualController().BusNumber, *vd.UnitNumber)
		}
		target := fmt.Sprintf("disk-%d.vmdk", i)
		var stream []byte
		for _, d := range spec.Disks {
			if d.Key == vd.Key {
				stream = d.Stream
			}
		}
		rec.streams[target] = stream
		urls = append(urls, types.HttpNfcLeaseDeviceUrl{
			Key: key, ImportKey: key, Url: e.s.deviceURL(ticket, target), SslThumbprint: e.s.nfcThumbprintFor(),
			Disk: types.NewBool(true), TargetId: target, FileSize: lb.FileSize,
		})
	}
	if lb.ExtraDiskURL {
		urls = append(urls, types.HttpNfcLeaseDeviceUrl{Key: fmt.Sprintf("/%s/VirtualLsiLogicController3:15", e.ref.Value),
			Url: e.s.deviceURL(ticket, "disk-99.vmdk"), Disk: types.NewBool(true), TargetId: "disk-99.vmdk"})
		rec.streams["disk-99.vmdk"] = []byte("bogus")
	}
	timeout := lb.TimeoutSeconds
	if timeout == 0 {
		timeout = 300
	}
	info := types.HttpNfcLeaseInfo{Lease: lease.Self, Entity: e.ref, DeviceUrl: urls, TotalDiskCapacityInKB: totalKB, LeaseTimeout: timeout}

	e.s.mu.Lock()
	e.s.leases[lease.Self.Value] = rec
	e.s.byTkt[ticket] = rec
	e.s.mu.Unlock()

	finish := func() {
		if lb.InitError != "" {
			ctx.WithLock(lease, func() {
				ctx.Update(lease, []types.PropertyChange{
					{Name: "state", Val: types.HttpNfcLeaseStateError},
					{Name: "error", Val: &types.LocalizedMethodFault{Fault: &types.SystemError{Reason: lb.InitError}, LocalizedMessage: lb.InitError}},
				})
			})
			return
		}
		e.s.mu.Lock()
		rec.lastRenew = time.Now()
		e.s.mu.Unlock()
		ctx.WithLock(lease, func() {
			ctx.Update(lease, []types.PropertyChange{
				{Name: "initializeProgress", Val: int32(100)},
				{Name: "info", Val: &info},
				{Name: "state", Val: types.HttpNfcLeaseStateReady},
			})
		})
	}
	if lb.ReadyDelay > 0 {
		time.AfterFunc(lb.ReadyDelay, finish)
	} else {
		finish()
	}
	body.Res = &types.ExportVmResponse{Returnval: lease.Self}
	return body
}

func contains(keys []int32, k int32) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func (s *Server) deviceURL(ticket, target string) string {
	if s.ESX {
		return "https://*/fake-nfc/" + ticket + "/" + target
	}
	return s.nfc.URL + "/nfc/" + ticket + "/" + target
}

func (s *Server) nfcThumbprintFor() string {
	if s.ESX {
		return ""
	}
	return s.NFCThumbprint
}

// HttpNfcLease is named after the vSphere type because vcsim resolves
// managed object types by Go type name.
type HttpNfcLease struct {
	mo.HttpNfcLease
	s   *Server
	rec *LeaseRecord
}

func (l *HttpNfcLease) fault(msg string, f types.BaseMethodFault) *soap.Fault {
	return simulator.Fault(msg, f)
}

func (l *HttpNfcLease) usable() (types.BaseMethodFault, string) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	switch {
	case l.rec.Expired:
		return &types.Timedout{}, "lease timed out"
	case l.rec.Completed || l.rec.Aborted:
		return &types.ManagedObjectNotFound{Obj: l.Self}, "lease no longer exists"
	}
	return nil, ""
}

func (l *HttpNfcLease) HttpNfcLeaseProgress(ctx *simulator.Context, req *types.HttpNfcLeaseProgress) soap.HasFault {
	body := &methods.HttpNfcLeaseProgressBody{}
	if f, msg := l.usable(); f != nil {
		body.Fault_ = l.fault(msg, f)
		return body
	}
	l.s.mu.Lock()
	lb := l.s.leaseB
	n := len(l.rec.Progress)
	if lb.ProgressFailAfter > 0 && n >= lb.ProgressFailAfter {
		l.s.mu.Unlock()
		f := lb.ProgressFault
		if f == nil {
			f = &types.InvalidState{}
		}
		body.Fault_ = l.fault("injected progress failure", f)
		return body
	}
	l.rec.Progress = append(l.rec.Progress, req.Percent)
	l.rec.lastRenew = time.Now()
	l.s.mu.Unlock()
	l.TransferProgress = req.Percent
	body.Res = &types.HttpNfcLeaseProgressResponse{}
	return body
}

func (l *HttpNfcLease) HttpNfcLeaseComplete(ctx *simulator.Context, req *types.HttpNfcLeaseComplete) soap.HasFault {
	body := &methods.HttpNfcLeaseCompleteBody{}
	if f, msg := l.usable(); f != nil {
		body.Fault_ = l.fault(msg, f)
		return body
	}
	l.s.mu.Lock()
	l.rec.Completed = true
	l.s.mu.Unlock()
	ctx.WithLock(l, func() {
		ctx.Update(l, []types.PropertyChange{{Name: "state", Val: types.HttpNfcLeaseStateDone}})
	})
	body.Res = &types.HttpNfcLeaseCompleteResponse{}
	return body
}

func (l *HttpNfcLease) HttpNfcLeaseAbort(ctx *simulator.Context, req *types.HttpNfcLeaseAbort) soap.HasFault {
	body := &methods.HttpNfcLeaseAbortBody{}
	l.s.mu.Lock()
	if l.rec.Completed || l.rec.Aborted {
		l.s.mu.Unlock()
		body.Fault_ = l.fault("lease no longer exists", &types.ManagedObjectNotFound{Obj: l.Self})
		return body
	}
	l.rec.Aborted = true
	if req.Fault != nil {
		l.rec.AbortMessage = req.Fault.LocalizedMessage
	}
	l.s.mu.Unlock()
	ctx.WithLock(l, func() {
		ctx.Update(l, []types.PropertyChange{{Name: "state", Val: types.HttpNfcLeaseStateError}})
	})
	body.Res = &types.HttpNfcLeaseAbortResponse{}
	return body
}

func (l *HttpNfcLease) HttpNfcLeaseGetManifest(ctx *simulator.Context, req *types.HttpNfcLeaseGetManifest) soap.HasFault {
	body := &methods.HttpNfcLeaseGetManifestBody{}
	l.s.mu.Lock()
	mode := l.s.leaseB.Manifest
	l.s.mu.Unlock()
	if mode == ManifestUnsupported {
		body.Fault_ = l.fault("method not supported", &types.MethodNotFound{Receiver: l.Self, Method: "HttpNfcLeaseGetManifest"})
		return body
	}
	var info types.HttpNfcLeaseInfo
	ctx.WithLock(l, func() {
		if l.Info != nil {
			info = *l.Info
		}
	})
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	var entries []types.HttpNfcLeaseManifestEntry
	for _, d := range info.DeviceUrl {
		sums, ok := l.rec.sums[d.TargetId]
		stream := l.rec.streams[d.TargetId]
		if !ok {
			s1 := sha1.Sum(stream)
			s2 := sha256.Sum256(stream)
			sums = [2][]byte{s1[:], s2[:]}
		}
		e := types.HttpNfcLeaseManifestEntry{Key: d.Key, Size: int64(len(stream)), Capacity: 0, PopulatedSize: 0}
		switch mode {
		case ManifestSHA256:
			e.Checksum, e.ChecksumType = hex.EncodeToString(sums[1]), "SHA256"
		case ManifestWrong:
			e.Sha1 = strings.Repeat("ab", 20)
		default:
			e.Sha1 = hex.EncodeToString(sums[0])
		}
		entries = append(entries, e)
	}
	body.Res = &types.HttpNfcLeaseGetManifestResponse{Returnval: entries}
	return body
}

func (s *Server) expiryLoop() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		limit := s.leaseB.ExpireWithoutProgress
		if limit > 0 {
			for _, l := range s.leases {
				if !l.Completed && !l.Aborted && !l.Expired && time.Since(l.lastRenew) > limit {
					l.Expired = true
				}
			}
		}
		s.mu.Unlock()
	}
}

// serveNFC serves /nfc/<ticket>/<target> (vCenter mode, separate host) or
// /fake-nfc/<ticket>/<target> (ESX mode, same host as the SDK).
func (s *Server) serveNFC(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	ticket, target := parts[1], parts[2]
	s.mu.Lock()
	rec := s.byTkt[ticket]
	b := s.nfcB
	var stream []byte
	var repeat int
	if rec != nil {
		stream = rec.streams[target]
		repeat = rec.served[target]
		rec.served[target]++
	}
	gone := rec == nil || rec.Completed || rec.Aborted || rec.Expired
	s.mu.Unlock()
	record := func(status int, n int64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if rec != nil {
			rec.Requests = append(rec.Requests, Request{Method: r.Method, Target: target, Range: r.Header.Get("Range"), Status: status, Bytes: n})
		}
	}
	if gone || stream == nil {
		record(http.StatusNotFound, 0)
		http.NotFound(w, r)
		return
	}
	if b.Status != 0 {
		record(b.Status, 0)
		w.WriteHeader(b.Status)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		record(http.StatusMethodNotAllowed, 0)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if b.ChangeOnRepeat && repeat > 0 {
		stream = append([]byte(nil), stream...)
		// Change a padding byte of the header sector, which leaves the
		// stream parseable but different.
		stream[300] ^= byte(repeat)
	}
	if b.CorruptAt > 0 && b.CorruptAt < int64(len(stream)) {
		stream = append([]byte(nil), stream...)
		stream[b.CorruptAt] ^= 0xff
	}
	ct := b.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	body := stream
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" && b.HonorRange {
		start, end, ok := parseRange(rng, int64(len(stream)))
		if ok {
			body = stream[start : end+1]
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(stream)))
		}
	}
	if b.ContentLength || status == http.StatusPartialContent {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	if r.Method == http.MethodHead {
		record(status, 0)
		w.WriteHeader(status)
		return
	}
	if status == http.StatusOK && repeat == 0 {
		s1 := sha1.Sum(stream)
		s2 := sha256.Sum256(stream)
		s.mu.Lock()
		rec.sums[target] = [2][]byte{s1[:], s2[:]}
		s.mu.Unlock()
	}
	w.WriteHeader(status)
	n, _ := s.write(w, r, rec, body, b)
	record(status, n)
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, rec *LeaseRecord, body []byte, b NFCBehavior) (int64, error) {
	flusher, _ := w.(http.Flusher)
	var n int64
	chunk := 64 * 1024
	for len(body) > 0 {
		c := min(chunk, len(body))
		if b.TruncateAfter > 0 && n+int64(c) > b.TruncateAfter {
			c = int(b.TruncateAfter - n)
			if c > 0 {
				m, _ := w.Write(body[:c])
				n += int64(m)
			}
			if flusher != nil {
				flusher.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
			return n, errors.New("truncated")
		}
		if b.StallAfter > 0 && n+int64(c) > b.StallAfter {
			c = int(b.StallAfter - n)
			if c > 0 {
				m, _ := w.Write(body[:c])
				n += int64(m)
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-s.stop:
			}
			return n, errors.New("stalled")
		}
		m, err := w.Write(body[:c])
		n += int64(m)
		if err != nil {
			return n, err
		}
		body = body[c:]
		if b.BytesPerSecond > 0 {
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(time.Duration(float64(c) / float64(b.BytesPerSecond) * float64(time.Second)))
		}
		s.mu.Lock()
		expired := rec.Expired || rec.Aborted
		s.mu.Unlock()
		if expired {
			// The lease is gone: the host stops serving the transfer.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
			return n, errors.New("lease no longer valid")
		}
	}
	return n, nil
}

func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, bs, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if bs != "" {
		e, err := strconv.ParseInt(bs, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		end = min(e, size-1)
	}
	return start, end, true
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = io.ReadFull(rand.Reader, b)
	return hex.EncodeToString(b)
}

func selfSigned() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "esxi-nfc.fake.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	return cert, certPEM, err
}
