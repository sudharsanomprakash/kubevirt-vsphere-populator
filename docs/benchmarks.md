# Benchmarks

**No benchmark against vSphere exists.** Throughput of ESXi NFC exports,
datastore behavior, network paths and the load placed on ESXi hosts have not
been measured, and no claim is made about link saturation or about parallel
transfers (which are not implemented). Gate A6 in the release checklist stays
open until those numbers exist.

What was measured is the single-stream pipeline of this software on the
development host, with the vSphere side replaced by the test double.

## Environment

* 4 vCPU Intel Xeon @ 2.80 GHz virtual machine, 15 GiB RAM, one virtio disk
  (ext4), Linux 6.18, cgroup v1. No KVM.
* Go 1.27.1, qemu-img 8.2.2 (Ubuntu 1:8.2.2+ds-0ubuntu1.18).
* Commit 50148f3 plus the benchmark tool in `test/bench`.

## 1. Decoder only (`go test ./internal/vmdk -bench BenchmarkDecode`)

In-memory 128 MiB streams; validation, inflate and the guest digest.

| Input | Guest MB/s (one core) |
|---|---|
| incompressible data | 264 |
| sparse (4 KiB of data per MiB) | 318 |

The chunked SHA-256 of non-zero chunks is the main cost; this CPU does not
advertise the SHA extensions (`sha_ni`), so SHA-256 runs in software.

## 2. Worker pipeline (`test/bench`, fake vSphere in the same process)

`bench` starts the test double, serves the disk over loopback TLS and runs
the full worker (lease, download, decode and staging, manifest check,
qemu-img conversion, flush, read-back verification, lease completion).
Because the fake server runs in the same process (TLS encryption and
serving), `processCPUSeconds` over-states the worker's own CPU use.

| Disk | Target | Virtual size | Allocated | Stream | Download+decode | Convert | Verify (O_DIRECT) | Total | Guest MB/s end to end | CPU s (process / qemu-img) |
|---|---|---|---|---|---|---|---|---|---|---|
| Ubuntu 24.04 minimal (sparse) | file | 3758 MB | 626 MB | 263 MB | 11.0 s | 7.6 s | 3.3 s | 22.0 s | 171 | 14.5 / 9.4 |
| same, second run | file | 3758 MB | 626 MB | 263 MB | 10.9 s | 6.6 s | 3.0 s | 20.6 s | 182 | 14.0 / 8.6 |
| 2 GiB random (incompressible) | file | 2147 MB | 2147 MB | 2164 MB | 49.0 s | 26.3 s | 8.5 s | 84.0 s | 26 | 59.1 / 27.4 |
| 2 GiB random (incompressible) | loop block device | 2147 MB | 2147 MB | 2164 MB | 49.1 s | 20.3 s | 7.6 s | 77.2 s | 28 | 58.9 / 19.0 |

Raw results: `bench` prints JSON; the runs above were recorded on 2026-09-27.

## 3. kind end-to-end run (fake vSphere in its own pod)

From the recorded run (docs/evidence/2026-09-26-kind-e2e.md):

| Disk | Stream bytes | Download+decode | Worker total |
|---|---|---|---|
| Ubuntu 24.04 minimal, 3.5 GiB virtual, Filesystem PVC | 263 MB | 5.2 s | 10.7 s |
| synthetic 300 MiB, Block PVC | 86 MB | 0.7 s | 2.2 s |

## Reading these numbers

* Staging and verification add local I/O: the stream is written once and read
  once by qemu-img, and the destination is read back in full after
  conversion. On the sparse disk, conversion and verification took about
  half of the total.
* A single stream is limited by one core for inflate and hashing
  (roughly 250–300 MB/s of guest data on this CPU) before any network or
  ESXi limit applies.
* These figures describe this host only. Measure against your vSphere
  environment with `vsphere-populator-worker diagnose --full-read` (download
  and decode without writing) and a real population, and record vSphere and
  datastore versions, disk sizes, the network path, elapsed time, throughput,
  worker CPU and ESXi host CPU/network during the export.
