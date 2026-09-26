# kubevirt-vsphere-populator

> **Experimental.** Not validated against any real vCenter or ESXi release,
> and no KubeVirt VM has been booted from a PVC populated by it. Do not use it
> for production migrations. See [what is and is not proven](#status).

A Kubernetes volume populator that copies one virtual disk of a **powered-off**
VMware VM from vCenter or a standalone ESXi host into a
PersistentVolumeClaim, in a form KubeVirt can use, **without VDDK**. It uses
the vSphere export API (`ExportVm` / `HttpNfcLease`) through
[govmomi](https://github.com/vmware/govmomi), converts the exported
streamOptimized VMDK to raw bytes with a pinned `qemu-img`, and hands the
volume to the PVC only after it has verified the destination against an
independent decode of the export stream.

Cold migration only. No warm migration, changed block tracking or snapshot
deltas, and no guest conversion (drivers are not changed).

## How it works

```
PVC (dataSourceRef → VSphereVolumeSource)
  └─ controller: prime PVC + worker Job in the PVC's namespace
       └─ worker: ExportVm → lease ready → map disk to lease URL by device identity
                  → one GET → validate stream while staging it (bounded) → manifest check
                  → qemu-img convert → flush → read back and compare digests
                  → re-check source VM → complete lease   (any failure: abort lease)
  └─ controller: only with a validated result, rebind the prime PV to the PVC
```

* Filesystem PVCs receive `/disk.img` (raw, sparse, owned by UID 107);
  Block PVCs receive the raw disk at offset 0 with the rest of the device zeroed.
* A completed Job is never taken as proof: the controller rebinds only when
  the worker's termination message carries matching source and destination
  digests.

Details: [architecture](docs/architecture.md) ·
[source format and design](docs/design.md) ·
[security](docs/security.md) · [compatibility](docs/compatibility.md) ·
[testing](docs/testing.md) · [benchmarks](docs/benchmarks.md) ·
[troubleshooting](docs/troubleshooting.md) ·
[upstream path](docs/upstream.md) · [release checklist](docs/release-checklist.md)

## Status

| Capability | Implemented | Tested | Measured |
|---|---|---|---|
| VM lookup by moRef / instance UUID / BIOS UUID / exact name, ambiguity rejected | yes | against the test double | – |
| Disk selection by device key / bus location / backing file; unsupported backings rejected | yes | unit + test double | – |
| Lease lifecycle: ready wait, keepalive with failure propagation, complete, abort | yes | test double (incl. expiry, init errors, renewal faults, cancellation) | – |
| Lease URL mapping by documented device key, no fallback | yes | test double; **real key format unverified** | – |
| streamOptimized VMDK validation and decoding | yes | synthetic streams, fuzzing, qemu-img interop, one VMware-generated sample; **no real ESXi export** | – |
| Conversion with qemu-img to file and block targets, prefilled targets cleared | yes | real qemu-img, loop devices | – |
| Manifest checksum verification (SHA-1/SHA-256) | yes | test double; **real manifest behavior unverified** | – |
| Read-back verification of the destination | yes | loop devices, files | – |
| Read-only `diagnose` command (format, headers, Range, mapping; redacted) | yes | test double | – |
| Volume populator controller (prime PVC, rebind, WFFC, Conditions, Events, retries, cancellation, restart, concurrency) | yes | envtest (kube-apiserver 1.37) | – |
| End-to-end in Kubernetes (kind 1.37, CSI hostpath, Filesystem + Block) | yes | against the test double, see [testing](docs/testing.md) | local timings only |
| Migrated disk boots | – | QEMU boot of the migrated Ubuntu disk from the kind run; **KubeVirt boot not tested** | – |
| Throughput against vSphere | – | – | **not measured** (local pipeline baseline only) |
| Forklift / CDI integration | – | – | – (see [compatibility](docs/compatibility.md)) |
| Warm migration, CBT, parallel transfers | **no** (planned only as separate designs) | – | – |

Unpassed production gates are listed in the
[release checklist](docs/release-checklist.md).

## Requirements

* Kubernetes with the `AnyVolumeDataSource` behavior (GA since 1.33) and a
  **CSI** StorageClass (the CSI external-provisioner leaves PVCs with unknown
  data sources to populators; other provisioners may not).
* Network access from worker pods to vCenter/ESXi on 443 (API and NFC).
* A vSphere account allowed to read the VM and export it.

## Install

Images are **not published**; build them and push them to your registry:

```sh
make images REGISTRY=registry.example.test/vsphere-populator VERSION=v0.1.0-alpha.0
docker push registry.example.test/vsphere-populator/kubevirt-vsphere-populator-controller:v0.1.0-alpha.0
docker push registry.example.test/vsphere-populator/kubevirt-vsphere-populator-worker:v0.1.0-alpha.0
```

Edit the two image references in `deploy/manager/deployment.yaml` (controller
`image:` and `--worker-image=`), then:

```sh
kubectl apply -k deploy/
# optional, if the volume-data-source-validator CRDs are installed:
kubectl apply -f deploy/optional/volumepopulator.yaml
```

Recommended controller flags: `--allowed-server-pattern` (restricts
`spec.server`), `--scratch-storage-class` (staging on a PVC instead of node
ephemeral storage). See `vsphere-populator-controller --help`.

### Uninstall

Delete PVCs that are still populating first (their finalizers are removed by
the controller), then:

```sh
kubectl delete -k deploy/
kubectl delete -f deploy/optional/volumepopulator.yaml --ignore-not-found
```

Deleting the CRD deletes all `VSphereVolumeSource` objects. Populated PVCs and
their PersistentVolumes are not affected.

## Use

```sh
kubectl create namespace vm-migration
kubectl apply -f examples/secret.yaml      # edit: username, password, ca.crt or thumbprint
kubectl apply -f examples/source.yaml      # edit: server, VM identity, disk
kubectl apply -f examples/pvc-filesystem.yaml   # or examples/pvc-block.yaml
kubectl -n vm-migration get vspherevolumesource app-01-disk0 -o yaml
```

`status.populations[]` shows the phase (`Pending`, `WaitingForFirstConsumer`,
`Running`, `Rebinding`, `Succeeded`, `Failed`, `Cancelled`), progress polled
from the worker, and, after success, the digests and export details. The PVC
receives Events. `examples/kubevirt-vm.yaml` shows how the PVC would be
attached to a VM (untested).

### Standalone worker and diagnostics

```sh
export VSPHERE_USERNAME=... VSPHERE_PASSWORD=...     # or --credentials-dir
vsphere-populator-worker diagnose --server vcenter.example.test --ca-file ca.pem \
  --vm-instance-uuid <uuid> --disk-bus scsi:0:0 --full-read
vsphere-populator-worker populate --server vcenter.example.test --ca-file ca.pem \
  --vm-moref vm-1234 --disk-key 2000 --target-mode file --target-path /mnt/vol --staging-dir /var/tmp
vsphere-populator-worker digest --size <bytes> /dev/sdX   # chunked digest of a device prefix
```

`diagnose` is read-only: it creates an export lease, probes the device URLs
(HEAD, a 64 KiB GET, a `Range` GET and a repeat GET; optionally a full
download and decode), and always aborts the lease. The report contains no
credentials, cookies, tickets or ticket-bearing URLs.

## Development

```sh
make fixtures test test-race lint vet   # see docs/testing.md
make e2e-kind                           # kind + fake vSphere + QEMU boot check
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0 ([LICENSE](LICENSE), [NOTICE](NOTICE)). The worker image
contains qemu-img (GPL-2.0-or-later), run as a separate program; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

This project is not affiliated with or endorsed by VMware, Broadcom, the
KubeVirt project or Red Hat. It does not contain or require VDDK.
