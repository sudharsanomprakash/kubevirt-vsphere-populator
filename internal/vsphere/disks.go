package vsphere

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// ErrUnsupported is wrapped by errors caused by unsupported source configurations.
var ErrUnsupported = errors.New("unsupported source configuration")

// ErrSourceState is wrapped by errors caused by a source in the wrong state.
var ErrSourceState = errors.New("source VM is not in a supported state")

// DiskInfo describes one virtual disk and whether it can be exported.
type DiskInfo struct {
	Key               int32  `json:"key"`
	Label             string `json:"label,omitempty"`
	CapacityBytes     int64  `json:"capacityBytes"`
	ControllerKey     int32  `json:"controllerKey"`
	ControllerType    string `json:"controllerType"`
	ControllerBus     int32  `json:"controllerBusNumber"`
	ControllerOrdinal int    `json:"controllerOrdinal"`
	UnitNumber        int32  `json:"unitNumber"`
	BusLocation       string `json:"busLocation"`
	BackingType       string `json:"backingType"`
	FileName          string `json:"fileName,omitempty"`
	DiskMode          string `json:"diskMode,omitempty"`
	ThinProvisioned   *bool  `json:"thinProvisioned,omitempty"`
	HasParent         bool   `json:"hasParent,omitempty"`
	Sharing           string `json:"sharing,omitempty"`
	Encrypted         bool   `json:"encrypted,omitempty"`
	BackingUUID       string `json:"backingUUID,omitempty"`
	Supported         bool   `json:"supported"`
	UnsupportedReason string `json:"unsupportedReason,omitempty"`
	// ExpectedLeaseKey is the HttpNfcLeaseDeviceUrl key this disk is
	// expected to have on export, or empty when the key convention cannot
	// be determined unambiguously (see docs/design.md).
	ExpectedLeaseKey string `json:"expectedLeaseKey,omitempty"`
}

// VMInfo is the state of the source VM relevant to a cold export.
type VMInfo struct {
	Ref             types.ManagedObjectReference        `json:"ref"`
	Name            string                              `json:"name"`
	InstanceUUID    string                              `json:"instanceUUID,omitempty"`
	BIOSUUID        string                              `json:"biosUUID,omitempty"`
	PowerState      types.VirtualMachinePowerState      `json:"powerState"`
	ConnectionState types.VirtualMachineConnectionState `json:"connectionState"`
	ChangeVersion   string                              `json:"changeVersion,omitempty"`
	Template        bool                                `json:"template,omitempty"`
	HasSnapshots    bool                                `json:"hasSnapshots,omitempty"`
	Encrypted       bool                                `json:"encrypted,omitempty"`
	Disks           []DiskInfo                          `json:"disks"`
}

var vmProps = []string{
	"name", "config.instanceUuid", "config.uuid", "config.template", "config.changeVersion",
	"config.hardware.device", "config.keyId", "runtime.powerState", "runtime.connectionState", "snapshot",
}

// InspectVM reads the VM's configuration and runtime state.
func (c *Client) InspectVM(ctx context.Context, ref types.ManagedObjectReference) (*VMInfo, error) {
	var vm mo.VirtualMachine
	if err := property.DefaultCollector(c.Client).RetrieveOne(ctx, ref, vmProps, &vm); err != nil {
		return nil, fmt.Errorf("read VM %s: %w", ref.Value, err)
	}
	return vmInfo(vm)
}

func vmInfo(vm mo.VirtualMachine) (*VMInfo, error) {
	if vm.Config == nil {
		return nil, fmt.Errorf("%w: VM %s has no configuration (inaccessible?)", ErrSourceState, vm.Self.Value)
	}
	info := &VMInfo{
		Ref:             vm.Self,
		Name:            vm.Name,
		InstanceUUID:    vm.Config.InstanceUuid,
		BIOSUUID:        vm.Config.Uuid,
		PowerState:      vm.Runtime.PowerState,
		ConnectionState: vm.Runtime.ConnectionState,
		ChangeVersion:   vm.Config.ChangeVersion,
		Template:        vm.Config.Template,
		HasSnapshots:    vm.Snapshot != nil && len(vm.Snapshot.RootSnapshotList) > 0,
		Encrypted:       vm.Config.KeyId != nil,
	}
	info.Disks = describeDisks(vm.Self, vm.Config.Hardware.Device)
	return info, nil
}

func controllerFamily(dev types.BaseVirtualDevice) string {
	switch dev.(type) {
	case types.BaseVirtualSCSIController:
		return "scsi"
	case *types.VirtualAHCIController, types.BaseVirtualSATAController:
		return "sata"
	case *types.VirtualNVMEController:
		return "nvme"
	case *types.VirtualIDEController:
		return "ide"
	}
	return ""
}

func typeName(v any) string { return reflect.TypeOf(v).Elem().Name() }

func describeDisks(vmRef types.ManagedObjectReference, devices []types.BaseVirtualDevice) []DiskInfo {
	type ctrl struct {
		dev     types.BaseVirtualDevice
		bus     int32
		family  string
		ordinal int
		// ambiguous is set when the ordinal among controllers of the same
		// type differs from the bus number (see ExpectedLeaseKey).
		ambiguous bool
	}
	ctrls := map[int32]*ctrl{}
	byType := map[string][]*ctrl{}
	for _, d := range devices {
		fam := controllerFamily(d)
		if fam == "" {
			continue
		}
		bc, ok := d.(types.BaseVirtualController)
		if !ok {
			continue
		}
		c := &ctrl{dev: d, bus: bc.GetVirtualController().BusNumber, family: fam}
		ctrls[d.GetVirtualDevice().Key] = c
		byType[typeName(d)] = append(byType[typeName(d)], c)
	}
	for _, cs := range byType {
		sort.Slice(cs, func(i, j int) bool { return cs[i].bus < cs[j].bus })
		for i, c := range cs {
			c.ordinal = i
			c.ambiguous = int32(i) != c.bus
		}
	}

	var disks []DiskInfo
	for _, d := range devices {
		disk, ok := d.(*types.VirtualDisk)
		if !ok {
			continue
		}
		vd := disk.GetVirtualDevice()
		di := DiskInfo{Key: vd.Key, CapacityBytes: disk.CapacityInBytes, ControllerKey: vd.ControllerKey}
		if di.CapacityBytes == 0 {
			di.CapacityBytes = disk.CapacityInKB * 1024
		}
		if vd.DeviceInfo != nil {
			di.Label = vd.DeviceInfo.GetDescription().Label
		}
		if vd.UnitNumber != nil {
			di.UnitNumber = *vd.UnitNumber
		}
		if vd.Backing != nil {
			di.BackingType = typeName(vd.Backing)
		}
		reasons := analyzeBacking(disk, &di)

		c := ctrls[vd.ControllerKey]
		if c == nil {
			reasons = append(reasons, "disk is not attached to a known SCSI, SATA, NVMe or IDE controller")
		} else {
			di.ControllerType = typeName(c.dev)
			di.ControllerBus = c.bus
			di.ControllerOrdinal = c.ordinal
			di.BusLocation = fmt.Sprintf("%s:%d:%d", c.family, c.bus, di.UnitNumber)
			if !c.ambiguous {
				di.ExpectedLeaseKey = fmt.Sprintf("/%s/%s%d:%d", vmRef.Value, di.ControllerType, c.bus, di.UnitNumber)
			} else {
				reasons = append(reasons, fmt.Sprintf(
					"controller %s has bus number %d but is number %d of its type; the lease device key convention for this layout has not been verified",
					di.ControllerType, c.bus, c.ordinal))
			}
		}
		di.Supported = len(reasons) == 0
		di.UnsupportedReason = strings.Join(reasons, "; ")
		disks = append(disks, di)
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Key < disks[j].Key })
	return disks
}

func analyzeBacking(disk *types.VirtualDisk, di *DiskInfo) []string {
	var reasons []string
	checkMode := func(mode string) {
		di.DiskMode = mode
		if mode != string(types.VirtualDiskModePersistent) {
			reasons = append(reasons, fmt.Sprintf("disk mode %q is not supported (only persistent disks are exported by this release)", mode))
		}
	}
	switch b := disk.Backing.(type) {
	case *types.VirtualDiskFlatVer2BackingInfo:
		di.FileName, di.ThinProvisioned, di.BackingUUID, di.Sharing = b.FileName, b.ThinProvisioned, b.Uuid, b.Sharing
		di.HasParent = b.Parent != nil
		di.Encrypted = b.KeyId != nil
		checkMode(b.DiskMode)
	case *types.VirtualDiskSeSparseBackingInfo:
		di.FileName, di.BackingUUID = b.FileName, b.Uuid
		di.HasParent = b.Parent != nil
		di.Encrypted = b.KeyId != nil
		checkMode(b.DiskMode)
	case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
		di.FileName = b.FileName
		reasons = append(reasons, "raw device mappings (RDM) are not supported")
	case nil:
		reasons = append(reasons, "disk has no backing")
	default:
		reasons = append(reasons, fmt.Sprintf("backing type %s is not supported", di.BackingType))
	}
	if di.HasParent {
		reasons = append(reasons, "disk has a parent (snapshot or linked-clone delta chain); consolidate or remove snapshots first")
	}
	if di.Sharing == string(types.VirtualDiskSharingSharingMultiWriter) {
		reasons = append(reasons, "multi-writer shared disks are not supported")
	}
	if di.Encrypted {
		reasons = append(reasons, "encrypted disks are not supported")
	}
	if di.CapacityBytes <= 0 || di.CapacityBytes%512 != 0 {
		reasons = append(reasons, fmt.Sprintf("capacity %d bytes is not a positive multiple of 512", di.CapacityBytes))
	}
	return reasons
}

// CheckColdSource returns an error unless the VM can be exported cold.
func (v *VMInfo) CheckColdSource() error {
	var problems []string
	if v.ConnectionState != types.VirtualMachineConnectionStateConnected {
		problems = append(problems, fmt.Sprintf("connection state is %q", v.ConnectionState))
	}
	if v.PowerState != types.VirtualMachinePowerStatePoweredOff {
		problems = append(problems, fmt.Sprintf("power state is %q (cold migration requires poweredOff)", v.PowerState))
	}
	if v.HasSnapshots {
		problems = append(problems, "the VM has snapshots (not supported by this release)")
	}
	if v.Encrypted {
		problems = append(problems, "the VM is encrypted")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: VM %s (%s): %s", ErrSourceState, v.Name, v.Ref.Value, strings.Join(problems, "; "))
	}
	return nil
}

// DiskSelector identifies the disk to export. Exactly one of DeviceKey,
// BusLocation or FileName must be set.
type DiskSelector struct {
	// DeviceKey is the VirtualDevice key (for example 2000).
	DeviceKey int32
	// BusLocation is "<scsi|sata|nvme|ide>:<bus>:<unit>", for example "scsi:0:1".
	BusLocation string
	// FileName is the backing file, for example "[datastore1] vm/vm_1.vmdk".
	FileName string
}

// Validate checks that exactly one selector is set.
func (s DiskSelector) Validate() error {
	n := 0
	if s.DeviceKey != 0 {
		n++
	}
	if s.BusLocation != "" {
		n++
		if _, err := ParseBusLocation(s.BusLocation); err != nil {
			return err
		}
	}
	if s.FileName != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("exactly one of deviceKey, busLocation or fileName must be set (got %d)", n)
	}
	return nil
}

func (s DiskSelector) String() string {
	switch {
	case s.DeviceKey != 0:
		return "device key " + strconv.Itoa(int(s.DeviceKey))
	case s.BusLocation != "":
		return "bus location " + s.BusLocation
	default:
		return "backing file " + s.FileName
	}
}

// ParseBusLocation normalizes a bus location such as "SCSI0:1" or "scsi:0:1".
func ParseBusLocation(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, fam := range []string{"scsi", "sata", "nvme", "ide"} {
		if strings.HasPrefix(t, fam) {
			rest := strings.TrimPrefix(strings.TrimPrefix(t, fam), ":")
			parts := strings.Split(rest, ":")
			if len(parts) == 2 {
				bus, err1 := strconv.ParseUint(parts[0], 10, 8)
				unit, err2 := strconv.ParseUint(parts[1], 10, 8)
				if err1 == nil && err2 == nil {
					return fmt.Sprintf("%s:%d:%d", fam, bus, unit), nil
				}
			}
		}
	}
	return "", fmt.Errorf("invalid bus location %q (want e.g. scsi:0:1)", s)
}

// SelectDisk returns the single disk matching sel. It fails when no disk or
// more than one disk matches, or when the matched disk is unsupported.
func (v *VMInfo) SelectDisk(sel DiskSelector) (*DiskInfo, error) {
	if err := sel.Validate(); err != nil {
		return nil, err
	}
	var loc string
	if sel.BusLocation != "" {
		loc, _ = ParseBusLocation(sel.BusLocation)
	}
	var matches []DiskInfo
	for _, d := range v.Disks {
		switch {
		case sel.DeviceKey != 0 && d.Key == sel.DeviceKey,
			loc != "" && d.BusLocation == loc,
			sel.FileName != "" && d.FileName == sel.FileName:
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		have := make([]string, 0, len(v.Disks))
		for _, d := range v.Disks {
			have = append(have, fmt.Sprintf("key=%d bus=%s file=%q", d.Key, d.BusLocation, d.FileName))
		}
		return nil, fmt.Errorf("no disk of VM %s matches %s (disks: %s)", v.Name, sel, strings.Join(have, "; "))
	case 1:
	default:
		return nil, fmt.Errorf("%s matches %d disks of VM %s; select by device key", sel, len(matches), v.Name)
	}
	d := matches[0]
	if !d.Supported {
		return nil, fmt.Errorf("%w: disk %d (%s): %s", ErrUnsupported, d.Key, d.BusLocation, d.UnsupportedReason)
	}
	return &d, nil
}
