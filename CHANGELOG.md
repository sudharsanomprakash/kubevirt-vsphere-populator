# Changelog

## Unreleased (experimental)

First public development snapshot. Not validated against real vSphere; see
docs/release-checklist.md.

* Cold export of one VM disk over `ExportVm`/`HttpNfcLease` with a keepalive
  that propagates renewal failures, complete-after-validation and abort on
  every failure path.
* VM selection by moRef, instance UUID, BIOS UUID or exact name; disk
  selection by device key, bus location or backing file; lease URL mapping by
  documented device key with no fallback.
* streamOptimized VMDK validation and decoding (streaming and random access)
  used as an independent verifier; conversion with a pinned qemu-img to a raw
  file or block device; read-back verification with a chunked SHA-256.
* Read-only `diagnose` command with redacted output.
* `VSphereVolumeSource` CRD and a volume populator controller (prime PVC,
  worker Job, PV rebind after validation, WaitForFirstConsumer, Conditions,
  Events, progress, retries, cancellation).
* Test double for vSphere exports, envtest controller tests, and a kind
  end-to-end test with a QEMU boot check.
