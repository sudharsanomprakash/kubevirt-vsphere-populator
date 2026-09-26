# Compatibility

"Tested" below means covered by the automated tests or the end-to-end run
described in [testing.md](testing.md). Nothing in this project has been run
against a real vCenter or ESXi host, and no KubeVirt VM has been booted from a
populated PVC.

## Source (vSphere)

| Item | Status |
|---|---|
| vCenter / ESXi versions | **Untested.** Implemented against the vSphere API as exposed by govmomi v0.56.0; exercised only against the test double |
| vCenter vs. standalone ESXi | Both code paths exist (lease URLs via ESXi host with thumbprint; `*` host substitution). Tested against the test double in both modes |
| Export format | streamOptimized VMDK with grain directory at the end, footer and end-of-stream marker. A VMware-generated sample decodes correctly. Real exports: **unverified** |
| Disk backings | FlatVer2 (thin/thick) and SeSparse without parents, `persistent` mode. Rejected: RDM, delta/linked clones, snapshots, independent/non-persistent modes, multi-writer, encrypted |
| Controllers | LSI Logic, LSI Logic SAS, ParaVirtual, BusLogic, AHCI (SATA), NVMe, IDE, when a controller's bus number equals its index among controllers of the same type; otherwise rejected (key convention unverified) |
| Power state | Powered off only (cold migration) |
| Warm migration / CBT / snapshot deltas | **Not supported.** An HttpNfcLease export has no changed-block tracking and does not replace VDDK's `QueryChangedDiskAreas`-based incremental copies |
| Guest conversion | **Not performed.** Disks are copied bit-for-bit; drivers, bootloader and network configuration are unchanged (no virt-v2v) |

## Destination (Kubernetes)

| Item | Status |
|---|---|
| Kubernetes | Controller tested with envtest (kube-apiserver 1.37.0). End-to-end: see testing.md for the kind (v1.37.0) run |
| Provisioner | CSI provisioners (external-provisioner skips PVCs with unknown data source kinds). End-to-end with csi-driver-host-path v1.18.0 / external-provisioner v6.3.0. Non-CSI provisioners are not supported |
| Volume modes | Filesystem (`/disk.img`, raw, owned by UID 107, mode 0660) and Block |
| Binding modes | Immediate and WaitForFirstConsumer |
| Access modes | Copied from the PVC; RWX filesystems rely on the Job's single-pod guarantee plus `flock` |
| Cross-namespace dataSourceRef | Not supported |
| Pod Security | Filesystem workers: `restricted`. Block workers: `baseline` (UID 0) |

## KubeVirt, CDI and Forklift

### Plain PVC (this project's interface)

* A populated **Filesystem** PVC contains `disk.img`, the raw file name and
  location KubeVirt uses for filesystem-mode PVC disks. The file size equals
  the source disk size. KubeVirt checks that the PVC can hold the image; the
  PVC must be requested with room for filesystem overhead.
* A populated **Block** PVC contains the raw disk starting at byte 0; bytes
  beyond the disk size are zero.
* `examples/kubevirt-vm.yaml` shows how the PVC would be attached. **This has
  not been tested**: KubeVirt images could not be pulled in the environment
  used for development (quay.io blocked), so no KubeVirt VM was started. The
  migrated Ubuntu disk from the end-to-end run was booted with plain QEMU
  instead (see testing.md). A KubeVirt boot remains an unpassed release gate.
* Because no guest conversion happens, pick a disk bus the guest already
  supports (for example `sata` or `scsi` for a VMware guest without virtio
  drivers).

### CDI DataVolume

Checked against CDI v1.66.1 source (`pkg/controller/datavolume/external-population-controller.go`):

* A DataVolume whose `spec.storage.dataSourceRef` (or `spec.pvc.dataSourceRef`)
  names an external populator is handled by CDI's external-population
  controller. CDI creates the PVC with the `dataSourceRef` and an
  external-population annotation, requires a CSI StorageClass, and marks the
  DataVolume **Succeeded once the PVC has been created**, not when the volume
  has been populated. Consumers therefore wait on PVC binding, which this
  populator only allows after validation.
* **Not tested** in this project. Expected to work without changes to either
  project, but the DataVolume `Succeeded` phase alone must not be read as
  "populated".
* Using a DataVolume `spec.source` (CDI's importer) with this project is not
  possible; there is no CDI source type for it.

### Forklift (kubev2v/forklift)

Checked against the Forklift main branch (module version
`v0.0.0-20260926025348-12c6344f7f07`):

* Forklift's vSphere provider copies disks either with virt-v2v (cold,
  "remote" conversion) or with CDI DataVolumes using a VDDK source, and uses
  volume populators for vSphere only in the storage copy-offload path
  (`VSphereXcopyVolumePopulator`, served by Forklift's own fork of
  lib-volume-populator in `cmd/populator-controller`). Populated copy-offload
  PVCs are converted in place afterwards (`InPlace` conversion with
  virt-v2v-in-place).
* This project is **not** a drop-in replacement for any of those paths and
  there is no Forklift adapter. Writing a PVC that Forklift would accept as a
  migrated disk requires changes in Forklift (see [upstream.md](upstream.md)).

## Toolchain and dependencies

| Component | Version |
|---|---|
| Go | 1.27.1 (toolchain), language 1.26 |
| govmomi | v0.56.0 |
| controller-runtime | v0.25.1 |
| Kubernetes libraries | v0.37.1 |
| qemu-img (worker image) | Ubuntu `qemu-utils` 1:8.2.2+ds-0ubuntu1.18 |
| Worker base image | ubuntu:24.04 (pinned digest) |
| Controller base image | gcr.io/distroless/static-debian12:nonroot (pinned digest) |
