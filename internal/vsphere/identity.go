package vsphere

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// ErrNotFound and ErrAmbiguous classify VM lookup failures.
var (
	ErrNotFound  = errors.New("virtual machine not found")
	ErrAmbiguous = errors.New("virtual machine identity is ambiguous")
)

// VMSelector identifies the source VM. Exactly one field other than
// Datacenter must be set. InstanceUUID (vCenter) or MoRef are the stable
// identities; BIOSUUID and Name are accepted only when they match exactly
// one VM.
type VMSelector struct {
	MoRef        string
	InstanceUUID string
	BIOSUUID     string
	Name         string
	// Datacenter optionally restricts UUID and name searches (inventory path or name).
	Datacenter string
}

// Validate checks that exactly one identity is set.
func (s VMSelector) Validate() error {
	n := 0
	for _, v := range []string{s.MoRef, s.InstanceUUID, s.BIOSUUID, s.Name} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of moRef, instanceUUID, biosUUID or name must be set (got %d)", n)
	}
	if s.MoRef != "" && !strings.HasPrefix(s.MoRef, "vm-") && !strings.HasPrefix(s.MoRef, "VirtualMachine:") {
		// ESXi uses numeric morefs ("42"); vCenter uses "vm-42".
		if strings.ContainsAny(s.MoRef, " /") {
			return fmt.Errorf("invalid moRef %q", s.MoRef)
		}
	}
	return nil
}

// String describes the selector without secrets.
func (s VMSelector) String() string {
	switch {
	case s.MoRef != "":
		return "moRef " + s.MoRef
	case s.InstanceUUID != "":
		return "instanceUUID " + s.InstanceUUID
	case s.BIOSUUID != "":
		return "biosUUID " + s.BIOSUUID
	default:
		return fmt.Sprintf("name %q", s.Name)
	}
}

// ResolveVM returns the unique VM matching sel.
func (c *Client) ResolveVM(ctx context.Context, sel VMSelector) (types.ManagedObjectReference, error) {
	var zero types.ManagedObjectReference
	if err := sel.Validate(); err != nil {
		return zero, err
	}
	var dc *object.Datacenter
	if sel.Datacenter != "" {
		f := find.NewFinder(c.Client, false)
		d, err := f.Datacenter(ctx, sel.Datacenter)
		if err != nil {
			return zero, fmt.Errorf("datacenter %q: %w", sel.Datacenter, err)
		}
		dc = d
	}

	switch {
	case sel.MoRef != "":
		ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: strings.TrimPrefix(sel.MoRef, "VirtualMachine:")}
		var vm mo.VirtualMachine
		if err := property.DefaultCollector(c.Client).RetrieveOne(ctx, ref, []string{"name"}, &vm); err != nil {
			return zero, fmt.Errorf("%w: %s: %w", ErrNotFound, sel, err)
		}
		return ref, nil
	case sel.InstanceUUID != "" || sel.BIOSUUID != "":
		uuid, instance := sel.BIOSUUID, false
		if sel.InstanceUUID != "" {
			uuid, instance = sel.InstanceUUID, true
		}
		refs, err := object.NewSearchIndex(c.Client).FindAllByUuid(ctx, dc, uuid, true, types.NewBool(instance))
		if err != nil {
			return zero, fmt.Errorf("search %s: %w", sel, err)
		}
		return unique(refs, sel)
	default:
		root := c.ServiceContent.RootFolder
		if dc != nil {
			folders, err := dc.Folders(ctx)
			if err != nil {
				return zero, err
			}
			root = folders.VmFolder.Reference()
		}
		v, err := view.NewManager(c.Client).CreateContainerView(ctx, root, []string{"VirtualMachine"}, true)
		if err != nil {
			return zero, err
		}
		defer func() { _ = v.Destroy(context.WithoutCancel(ctx)) }()
		refs, err := v.Find(ctx, []string{"VirtualMachine"}, property.Match{"name": sel.Name})
		if err != nil {
			return zero, fmt.Errorf("search %s: %w", sel, err)
		}
		// property.Match uses glob patterns; require an exact name match.
		var exact []object.Reference
		if len(refs) > 0 {
			var vms []mo.VirtualMachine
			if err := property.DefaultCollector(c.Client).Retrieve(ctx, refs, []string{"name"}, &vms); err != nil {
				return zero, err
			}
			for _, vm := range vms {
				if vm.Name == sel.Name {
					exact = append(exact, object.NewVirtualMachine(c.Client, vm.Self))
				}
			}
		}
		return unique(exact, sel)
	}
}

func unique(refs []object.Reference, sel VMSelector) (types.ManagedObjectReference, error) {
	seen := map[string]types.ManagedObjectReference{}
	for _, r := range refs {
		seen[r.Reference().Value] = r.Reference()
	}
	switch len(seen) {
	case 0:
		return types.ManagedObjectReference{}, fmt.Errorf("%w: %s", ErrNotFound, sel)
	case 1:
		for _, r := range seen {
			return r, nil
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return types.ManagedObjectReference{}, fmt.Errorf("%w: %s matches %d VMs (%s); use instanceUUID or moRef",
		ErrAmbiguous, sel, len(seen), strings.Join(ids, ", "))
}
