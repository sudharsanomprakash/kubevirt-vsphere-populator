package vsphere

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func unit(u int32) *int32 { return &u }

func TestAnalyzeBackings(t *testing.T) {
	vmRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-7"}
	ctrl := &types.VirtualLsiLogicController{VirtualSCSIController: types.VirtualSCSIController{
		VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 1000}, BusNumber: 0}}}
	mk := func(key int32, u int32, b types.BaseVirtualDeviceBackingInfo) *types.VirtualDisk {
		return &types.VirtualDisk{VirtualDevice: types.VirtualDevice{Key: key, ControllerKey: 1000, UnitNumber: unit(u), Backing: b},
			CapacityInBytes: 1 << 30}
	}
	flat := func(mode string) *types.VirtualDiskFlatVer2BackingInfo {
		return &types.VirtualDiskFlatVer2BackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[ds] a.vmdk"}, DiskMode: mode}
	}
	parent := flat("persistent")
	parent.Parent = flat("persistent")
	mw := flat("persistent")
	mw.Sharing = string(types.VirtualDiskSharingSharingMultiWriter)
	enc := flat("persistent")
	enc.KeyId = &types.CryptoKeyId{KeyId: "k"}
	odd := mk(2009, 9, flat("persistent"))
	odd.CapacityInBytes = 1000

	devices := []types.BaseVirtualDevice{
		ctrl,
		mk(2000, 0, flat("persistent")),
		mk(2001, 1, flat("independent_persistent")),
		mk(2002, 2, flat("nonpersistent")),
		mk(2003, 3, parent),
		mk(2004, 4, &types.VirtualDiskRawDiskMappingVer1BackingInfo{DeviceName: "naa.1"}),
		mk(2005, 5, mw),
		mk(2006, 6, enc),
		mk(2008, 8, &types.VirtualDiskSparseVer2BackingInfo{DiskMode: "persistent"}),
		&types.VirtualDisk{VirtualDevice: types.VirtualDevice{Key: 2010, ControllerKey: 9999, UnitNumber: unit(0), Backing: flat("persistent")}, CapacityInBytes: 1 << 30},
		odd,
	}
	want := map[int32]string{
		2000: "",
		2001: "disk mode",
		2002: "disk mode",
		2003: "parent",
		2004: "raw device mappings",
		2005: "multi-writer",
		2006: "encrypted",
		2008: "not supported",
		2009: "multiple of 512",
		2010: "controller",
	}
	disks := describeDisks(vmRef, devices)
	if len(disks) != len(want) {
		t.Fatalf("got %d disks", len(disks))
	}
	for _, d := range disks {
		w := want[d.Key]
		if w == "" {
			if !d.Supported || d.ExpectedLeaseKey != "/vm-7/VirtualLsiLogicController0:0" {
				t.Errorf("disk %d: %+v", d.Key, d)
			}
			continue
		}
		if d.Supported || !strings.Contains(d.UnsupportedReason, w) {
			t.Errorf("disk %d: supported=%v reason=%q, want %q", d.Key, d.Supported, d.UnsupportedReason, w)
		}
	}
}

func TestParseDeviceKey(t *testing.T) {
	k, err := ParseDeviceKey("/vm-10649/VirtualLsiLogicController0:1")
	if err != nil || k.VM != "vm-10649" || k.ControllerType != "VirtualLsiLogicController" || k.Controller != 0 || k.Unit != 1 {
		t.Fatalf("%+v %v", k, err)
	}
	for _, bad := range []string{"/vm-1/disk:0", "vm-1/VirtualLsiLogicController0:0", "/vm-1/VirtualLsiLogicController:0", ""} {
		if _, err := ParseDeviceKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNormalizeThumbprint(t *testing.T) {
	sha1 := "ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01"
	if got := NormalizeThumbprint(strings.ReplaceAll(sha1, ":", "")); got != strings.ToUpper(sha1) {
		t.Fatalf("got %q", got)
	}
	if NormalizeThumbprint("xyz") != "" || NormalizeThumbprint(strings.Repeat("a", 41)) != "" {
		t.Fatal("invalid thumbprint accepted")
	}
}

func TestKeepaliveInterval(t *testing.T) {
	if KeepaliveInterval(300e9) != 10e9 || KeepaliveInterval(6e9) != 1e9 || KeepaliveInterval(0) != 10e9 {
		t.Fatal("unexpected interval")
	}
}

func TestMethodFaultFromValueDetail(t *testing.T) {
	err := soap.WrapSoapFault(&soap.Fault{Detail: struct {
		Fault types.AnyType `xml:",any,typeattr"` //nolint:staticcheck // must match soap.Fault.Detail exactly
	}{Fault: types.InvalidState{}}})
	if _, ok := MethodFault(fmt.Errorf("wrapped: %w", err)).(*types.InvalidState); !ok {
		t.Fatalf("value fault detail not recognized: %T", MethodFault(err))
	}
	if !isPermanentLeaseFault(err) {
		t.Fatal("InvalidState must be permanent")
	}
	if isPermanentLeaseFault(errors.New("connection reset")) {
		t.Fatal("network error must be transient")
	}
}
