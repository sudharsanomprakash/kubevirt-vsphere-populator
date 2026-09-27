# Testing

## Reproducible commands

```sh
make fixtures        # downloads the pinned VMware-generated sample (SHA-256 checked)
make test            # all suites; requires qemu-img, fixtures and envtest (fails instead of skipping)
make test-race       # the same with -race
make lint vet        # golangci-lint v2 (config in .golangci.yml) and go vet
make verify-generate # deepcopy/CRD are up to date
make images          # controller and worker images
make e2e-kind        # kind end-to-end (fake vSphere) + QEMU boot check
```

Loop-device tests (real block devices) run only as root; CI runs the suites
with `sudo`. `KVSP_REQUIRE_QEMU_IMG=1`, `KVSP_REQUIRE_FIXTURES=1` and
`KVSP_REQUIRE_ENVTEST=1` (set by the Makefile) turn skips into failures.

## What the suites cover

| Area | Tests |
|---|---|
| VMDK stream format | round trips for 512 B, one grain, odd sizes, multi-table disks; all-zero disks with and without explicit zero grains; out-of-order grains (streaming decoder reports no digest, random-access decoder recovers it); truncation at many offsets; trailing zero padding accepted, trailing data rejected; missing footer; wrong createType; duplicate, misaligned, out-of-range, short, oversized, corrupt-checksum, non-zlib and oversized-claim grains; last partial grain; header validation; early abort from `OnHeader`; fuzzing (`FuzzDecode`) |
| Interoperability | qemu-img converts streams from the test encoder back to the original bytes (ordered and shuffled); qemu-img's own streamOptimized files are rejected (no end-of-stream marker); the VMware-generated sample decodes, matches the data pattern asserted by QEMU's iotest 059, and matches qemu-img's conversion |
| Digest | agreement with its definition for irregular writes and zero runs; length sensitivity; short input |
| Download | 200 only (206, 302, 404, 500, 503 rejected); truncated bodies with Content-Length and chunked; clean early end; idle timeout before headers and mid-body; cancellation; content encoding rejected; bounded staging and stale-file cleanup |
| Destinations | file target replaces a prefilled image, is sparse, is locked against a second writer, rejects insufficient capacity (tmpfs) and wrong sizes, removes partial output on abort; block target on a prefilled regular file and a real loop device (zero regions cleared, tail beyond the disk zeroed, `O_DIRECT` read-back, `BLKZEROOUT`), undersized device rejected, regular files rejected by default, second exclusive open rejected |
| qemu-img wrapper | argument safety (never `--target-is-zero`, input format forced), errors, cancellation of a hung subprocess, progress parsing |
| vSphere (test double) | TLS: untrusted server rejected, CA bundle, thumbprint pinning, wrong thumbprint, explicit insecure mode, password never in errors; server URL validation; credential files and environment; VM lookup by moRef/instance UUID/BIOS UUID/name, duplicate names across folders rejected, glob-like names not matched; multi-controller disk inventory and expected keys; ambiguous controller numbering rejected; selectors; powered-on and snapshot VMs rejected, export of a powered-on VM refused; lease URL mapping independent of URL order; vcsim-style keys, extra disk URLs and missing disk URLs rejected; lease init error; ready timeout aborts the lease; keepalive renews and propagates failures; complete/abort semantics; abort reason redacted; manifest SHA-1/SHA-256/unsupported; unsupported backings (RDM, delta, independent, non-persistent, multi-writer, encrypted, sparse v2, odd capacity) |
| Worker (test double + real qemu-img) | file target with multi-disk mapping and a prefilled previous image; block target on a prefilled loop device; ESXi mode with `*` hosts; untrusted NFC host; HTTP 503/404, truncation, clean early end, stalls, corruption, wrong manifest, required-but-missing manifest, lease init error, keepalive failure, vcsim keys, staging limit and capacity mismatch all fail, abort the lease and leave no image or staging file; keepalive outlives a short lease timeout; lease expiry without keepalive fails; out-of-order streams; stream/vSphere capacity mismatch; insufficient destination and powered-on source rejected before any lease; interrupted conversion aborts the lease and cleans up; a retry after a failed attempt succeeds |
| Diagnostics | report contents, Range ignored vs honored, unstable content detected, raw (non-VMDK) content identified, lease always aborted, no secrets in the report |
| Controller (envtest, kube-apiserver 1.37.0) | Filesystem lifecycle (prime PVC, finalizer, locked-down Job, progress status, validated rebind with annotations, cleanup); Block Job with device mapping and ephemeral scratch; WaitForFirstConsumer; completed Job without validation or with mismatched digests is not trusted; failed Job reports the worker error; insecure TLS needs controller permission; server allow pattern; source created later; PVC deletion cancels and cleans up; controller restart resumes; two controllers plus 16 concurrent reconciles create one prime PVC and one Job |

## Recorded runs

### 2026-09-26/27, development host (no vSphere, no KVM)

Host: 4 vCPU Xeon VM, Ubuntu 24.04 userland, Linux 6.18, cgroup v1, Docker
29.3.1, Go 1.27.1, qemu-img 8.2.2.

| Check | Result |
|---|---|
| `go test -race` of every package (qemu-img, fixtures and envtest required, loop devices as root) | pass |
| golangci-lint v2.14.0 (`.golangci.yml`), `go vet`, gofmt | 0 issues |
| Controller and worker images build (base images pinned; a sandbox-only `--build-context` substituted the Go build image with one trusting the sandbox's TLS proxy) | pass |
| kind v0.33.0, Kubernetes v1.37.0, csi-driver-host-path v1.18.0, external-provisioner v6.3.0: Filesystem PVC (Immediate) and Block PVC (WaitForFirstConsumer) populated from the fake vSphere with `requireManifest` | pass; both `Succeeded` 41 s after the PVCs were created; no prime PVCs left; PVs annotated |
| Independent comparison (`sha256sum`) of source raw files with the destinations | pass: `disk.img` = `23ff2cd7…2f2a` (3,758,096,384 bytes) and block device prefix = `b71a852f…5997` (314,576,384 bytes); remaining 222,294,528 bytes (212 MiB) of the block device are zero |
| Boot of the migrated `disk.img` (copied out of the PVC, SHA-256 re-checked) with QEMU 8.2 TCG, cloud-init marker from inside the guest | pass after 105 s (kernel 6.8.0-139-generic, root on `/dev/vda1`) |
| Negative control: boot check on a zeroed disk | fails as expected ("No bootable device") |
| Incidental: Filesystem population on a node with too little free space | worker refused before requesting a lease, Job retried, source reported `Failed` with the capacity message (docs/evidence/2026-09-26-kind-capacity-failure.md) |

Full report and logs: [evidence/2026-09-26-kind-e2e.md](evidence/2026-09-26-kind-e2e.md).

Two harness defects were found and fixed during these runs (CSI binaries
built without a version stamp; the checksum wait matched the directory
listing instead of the checksum line). Earlier runs that failed because of them
are not counted.

Not performed: any run against real vCenter/ESXi, any KubeVirt boot, any
throughput measurement against vSphere.
