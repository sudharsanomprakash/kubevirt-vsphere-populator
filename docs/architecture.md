# Architecture

```
            user namespace (e.g. vm-migration)                         vSphere
 ┌──────────────────────────────────────────────────────────┐
 │ VSphereVolumeSource ◄── PVC.spec.dataSourceRef            │
 │        │                    │  (finalizer while populating)│
 │        │        ┌───────────┴───────────┐                  │
 │        │        │ controller creates     │                  │
 │        ▼        ▼                        ▼                  │
 │  Secret   prime PVC  ◄── mounted ──  worker Job (1 pod)  ───┼──► vCenter/ESXi SDK (session, ExportVm,
 │ (creds)   (same spec,                 - vsphere-populator-   │     HttpNfcLease, progress, complete)
 │           no dataSource)                worker populate      │
 │              │                        - qemu-img             ├──► ESXi NFC URL (one GET per disk)
 │              ▼                                                │
 │        PersistentVolume ── claimRef rebound to the PVC ──────►│ (after validation)
 └──────────────────────────────────────────────────────────┘
```

## Components

| Component | Binary / image | Runs as |
|---|---|---|
| Controller | `vsphere-populator-controller` (distroless, static) | Deployment in `vsphere-populator-system`, leader-elected, non-root |
| Worker | `vsphere-populator-worker` + `qemu-img` (Ubuntu 24.04) | One Job per target PVC, in the PVC's namespace |
| API | `VSphereVolumeSource` (`vsphere-populator.sudharsanomprakash.github.io/v1alpha1`), namespaced | |

Go packages:

| Package | Responsibility |
|---|---|
| `internal/vmdk` | streamOptimized VMDK parser (streaming and random access), test encoder |
| `internal/digest` | chunked SHA-256 used to compare decoded source and destination |
| `internal/transfer` | GET with strict status/length/encoding checks, idle timeout, bounded staging file |
| `internal/convert` | qemu-img discovery, `info` cross-check, `convert` with progress and cancellation |
| `internal/target` | file and block destinations: capacity, exclusivity, flush, tail clearing, read-back |
| `internal/vsphere` | TLS/credentials, VM and disk selection, lease lifecycle, keepalive, URL mapping |
| `internal/worker` | orchestration of one population and its result |
| `internal/diagnose` | read-only export inspection |
| `internal/controller` | the volume populator controller |
| `internal/redact` | removal of credentials, cookies, tickets and ticket-bearing URLs |
| `test/fakevsphere` | vcsim-based test double with an ExportVm implementation and fault injection |

## Population sequence

1. A user creates a `VSphereVolumeSource` and a PVC whose `dataSourceRef`
   names it. The CSI external-provisioner ignores PVCs whose data source kind
   it does not handle ("assuming an external populator will provision it"), so
   the PVC stays unbound. Non-CSI provisioners may not behave this way and are
   not supported.
2. The controller validates the source (CRD schema with CEL rules, plus
   controller policy: insecure TLS permission and an optional server allow
   pattern) and the StorageClass. With `WaitForFirstConsumer` it waits for the
   scheduler's `volume.kubernetes.io/selected-node` annotation.
3. It adds a finalizer to the PVC and creates, in the same namespace:
   * the prime PVC `vsphere-populator-prime-<pvc-uid>`: same access modes,
     size, StorageClass, volume mode and volume attributes class; no data
     source; owned by the PVC; with the selected-node annotation for WFFC;
   * the Job `vsphere-populator-<pvc-uid>`: parallelism 1, completions 1,
     `backoffLimit` from the source (default 2), `podReplacementPolicy:
     Failed`, the prime PVC mounted at `/target` (Filesystem) or mapped at
     `/dev/vsphere-populator-target` (Block), the credentials Secret mounted
     read-only, a scratch volume, node affinity to the selected node for WFFC,
     no service account token, all capabilities dropped, read-only root
     filesystem, seccomp `RuntimeDefault`.
4. While the Job runs, the controller polls the worker's `/progress`
   endpoint and records phase, bytes, percentages and throughput in
   `status.populations[]` of the source.
5. When the Job is complete, the controller reads the worker's termination
   message. It rebinds the volume only if the message says `validated: true`,
   the source and destination digests are present and equal, and the virtual
   size fits the PVC request. A Job that completed without that evidence is
   reported as `Failed` (`NoValidationEvidence`) and the volume is not handed
   over.
6. Rebind: the prime PVC's PersistentVolume gets its `claimRef` patched to
   the user's PVC (name, namespace, UID, resourceVersion) and is annotated
   with `populated-from` and `guest-digest`. The PV controller binds the PVC to
   it; the prime PVC becomes `Lost`.
7. The controller then deletes the Job and the prime PVC, removes the
   finalizer and records `Succeeded`.
8. Deleting the PVC before completion deletes the Job (the worker receives
   SIGTERM and aborts the lease) and the prime PVC, and records `Cancelled`.

## Idempotency and concurrency

* All objects have deterministic names derived from the PVC UID, carry a
  `pvc-uid` label that is checked before use, and are created with
  `AlreadyExists` treated as success. State is recomputed from the cluster on
  every reconcile, so a controller restart resumes where it left off (tested
  in `TestControllerRestart`).
* The controller uses leader election; running two controllers without it
  still yields a single prime PVC and Job (`TestConcurrentReconcilers`).
* Two workers never write the same destination concurrently: the Job allows
  one pod at a time and, with `podReplacementPolicy: Failed`, creates a
  replacement only after the previous pod has fully terminated. As defense in
  depth the worker takes an `flock` on filesystem volumes and opens block
  devices with `O_EXCL` (both are node-local guarantees).
* Status updates to the shared `VSphereVolumeSource` are retried on conflict
  and only touch the entry of the PVC being reconciled.

## Retries

* Worker attempts: the Job's `backoffLimit`. Each attempt starts from
  scratch with a new lease; stale staging and partial files are removed.
* After the Job has failed, the population stays `Failed` and the prime PVC
  and Job are kept for inspection. Delete and recreate the PVC to retry.

## Why not kubernetes-csi/lib-volume-populator

The library's API was checked at `v1.2.0` (latest tag, 2022) and at master
(2025-07, `v1.2.1-0.20250717175425-4441d3fff37b`). It implements the same
prime-PVC-and-rebind pattern, but:

* its populator pod runs in the populator's own namespace, so it cannot mount a
  credentials Secret from the user's namespace without copying Secrets across
  namespaces (which would require the controller to read all Secrets);
* the pod spec is fixed apart from image and arguments (no security context,
  no Secret volumes, no scratch volume, no resources);
* failed pods are deleted and recreated without a retry limit;
* the newer provider-function and mutator hooks exist only on an untagged
  master commit.

Forklift maintains a fork (`pkg/lib-volume-populator`) for similar reasons and
runs its populator pods in the CR's namespace. This project implements the
same pattern directly with controller-runtime so that it can use a Job, keep
the controller free of Secret access, and report status on the source object.

## Security boundaries

See [security.md](security.md).
