# Path towards Forklift and CDI

This project is independent and will stay usable on its own. Nothing here
implies that KubeVirt, CDI or Forklift will adopt it. The recommendation is a
design discussion with the Forklift maintainers once the real-vSphere release
gates in [release-checklist.md](release-checklist.md) have results; this
document collects what that discussion needs. No upstream pull request has
been opened.

## What the project would contribute

* A VDDK-free way to copy a powered-off VM's disks: the vSphere export API
  (`ExportVm` / `HttpNfcLease`), which ships with vSphere and needs no
  proprietary client library.
* A validation model that does not trust a completed Job: stream structure
  checks, manifest checksums, and a read-back comparison of the destination
  against an independent decode of the stream, before the lease is completed
  and before the PVC is bound.

## What it would not replace

* **Warm migration.** Forklift's warm migrations rely on VDDK and changed
  block tracking to copy only changed areas between snapshots. Export leases
  always transfer a full disk. A VDDK-free warm path would need a separate
  design (for example CBT queries through the vSphere API plus a non-VDDK
  block reader), which this project does not have.
* **Guest conversion.** Forklift runs virt-v2v to install drivers and adjust
  the guest. This project copies disks only.
* **Performance claims.** There are no measurements against vSphere yet.
  NFC export throughput per host/stream and its effect on ESXi hosts must be
  measured before this can be proposed as a default.

## Likely integration point in Forklift

Based on the Forklift main branch (module version
`v0.0.0-20260926025348-12c6344f7f07`):

1. **Populator controller.** `cmd/populator-controller` runs Forklift's fork
   of lib-volume-populator (`pkg/lib-volume-populator/populator-machinery`) for
   `OvirtVolumePopulator`, `OpenstackVolumePopulator` and
   `VSphereXcopyVolumePopulator`, with pods in the CR's namespace. A new
   populator kind (say `VSphereExportVolumePopulator`) with an image variable
   and an argument builder is the smallest change there. Alternatively the
   existing controller in this repository could be referenced, but two
   populator frameworks in one product would be harder to maintain.
2. **vSphere plan builder.** `pkg/controller/plan/adapter/vsphere/builder.go`
   decides per plan between virt-v2v transfer (`ShouldUseV2vForTransfer`),
   CDI with a VDDK source, and populators (`SupportsVolumePopulators`,
   currently only for copy offload via the storage map's `OffloadPlugin`). A
   new transfer mode would need a plan or storage-map setting, PVC creation
   with a `dataSourceRef` to the new populator (as `PopulatorVolumes` does for
   copy offload), progress reporting (`PopulatorTransferredBytes`), and
   cleanup.
3. **Conversion.** After population, the disks would be converted in place
   like copy-offload PVCs (`ResolveConversionType` returns `InPlace` when
   `IsCopyOffload` is true). That condition would have to cover the new
   populator as well.
4. **Credentials and TLS.** Forklift already stores vSphere credentials,
   CA bundles and thumbprints in the provider Secret; the populator would need
   to consume that Secret format (`user`, `password`, `cacert`, `thumbprint`,
   `insecureSkipVerify`) rather than this project's keys.

## CDI

No CDI change is required for the plain-PVC or DataVolume-with-`dataSourceRef`
paths (see [compatibility.md](compatibility.md)). Two points to raise:

* CDI marks an externally populated DataVolume `Succeeded` when the PVC is
  created. Tools that treat that phase as "data present" would be misled;
  surfacing the populator's status (or waiting for the PVC to be bound)
  would make the DataVolume phase meaningful.
* Filesystem overhead: KubeVirt/CDI expect `disk.img` to fit the PVC with
  CDI's filesystem overhead accounted for. This project requires free space
  for the full virtual size but does not apply CDI's overhead setting; aligning
  with CDI's `filesystemOverhead` configuration should be discussed.

## Questions for Forklift maintainers

1. Is a VDDK-free cold path for vSphere wanted, given the VDDK distribution
   constraints, and would it be acceptable only as an opt-in mode?
2. Should it be a new populator in Forklift's populator-controller, or should
   Forklift invoke an external populator (this project) through
   `dataSourceRef`?
3. Which evidence would be required before enabling it: specific vSphere
   versions, datastore types (VMFS, vSAN, NFS, vVols), disk sizes, throughput
   relative to VDDK, and the load on ESXi hosts during NFC exports?
4. How should export-lease concurrency be bounded per ESXi host and per
   datastore alongside Forklift's existing `MAX_VM_INFLIGHT`-style limits?
5. Is in-place virt-v2v conversion after population the preferred sequence
   for cold migrations that do not use virt-v2v for the copy?
6. Would Forklift accept the stricter validation model (read-back comparison
   before binding), given its extra read of every destination disk?

## Before proposing anything

* Run `vsphere-populator-worker diagnose --full-read` against real vCenter
  and ESXi releases and record the lease metadata, key formats, response
  headers, Range behavior and manifest checksums.
* Run the end-to-end flow against a disposable real VM, compare source and
  destination independently, and boot the result in KubeVirt.
* Measure single-stream throughput and ESXi CPU/network impact, then decide
  whether concurrency is worth adding.
