// Command fake-vsphere serves the fakevsphere test double for end-to-end
// tests. It is not a vSphere implementation; see test/fakevsphere.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/test/fakevsphere"
)

type diskFlags []string

func (d *diskFlags) String() string     { return strings.Join(*d, ",") }
func (d *diskFlags) Set(v string) error { *d = append(*d, v); return nil }

func main() {
	var disks diskFlags
	sdk := flag.String("sdk-listen", ":8989", "SDK listen address")
	nfc := flag.String("nfc-listen", ":9443", "NFC listen address")
	advertise := flag.String("nfc-advertise-host", "", "host name placed in lease URLs")
	vmName := flag.String("vm-name", "e2e-vm", "name of the VM to create")
	flag.Var(&disks, "disk", "controller:bus:unit=path-to-raw-disk (repeatable), e.g. lsilogic:0:0=/data/disk0.raw")
	flag.Parse()

	s, err := fakevsphere.Start(fakevsphere.Options{SDKListen: *sdk, NFCListen: *nfc, NFCAdvertiseHost: *advertise})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	spec := fakevsphere.VMSpec{Name: *vmName}
	for _, d := range disks {
		loc, path, ok := strings.Cut(d, "=")
		parts := strings.Split(loc, ":")
		if !ok || len(parts) != 3 {
			log.Fatalf("invalid --disk %q", d)
		}
		var bus, unit int32
		fmt.Sscanf(parts[1], "%d", &bus)
		fmt.Sscanf(parts[2], "%d", &unit)
		spec.Disks = append(spec.Disks, fakevsphere.Disk{Controller: parts[0], Bus: bus, Unit: unit, RawFile: path})
	}
	vm, err := s.AddVM(context.Background(), spec)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake vSphere SDK at %s (not a real vSphere)", s.URL)
	log.Printf("SDK certificate thumbprint: %s", s.Thumbprint)
	log.Printf("VM %s moRef=%s instanceUUID=%s", vm.Name, vm.Ref.Value, vm.InstanceUUID)
	for _, d := range vm.Disks {
		log.Printf("disk key=%d %s:%d:%d", d.Key, d.Controller, d.Bus, d.Unit)
	}
	_ = os.WriteFile("/tmp/ready", []byte(vm.InstanceUUID+"\n"+s.Thumbprint+"\n"), 0o644)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	for _, l := range s.Leases() {
		log.Printf("lease %s completed=%v aborted=%v progressCalls=%d requests=%d", l.Ref.Value, l.Completed, l.Aborted, len(l.Progress), len(l.Requests))
	}
}
