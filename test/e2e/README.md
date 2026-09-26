# End-to-end test (kind + fake vSphere)

`run.sh` creates a disposable kind cluster and checks the whole population
path from an export lease to a bound PVC. The vSphere side is the **test
double** in `test/fakevsphere` (govmomi's vcsim plus an ExportVm
implementation), not a real vCenter or ESXi host.

What it does:

1. Downloads the Ubuntu 24.04 minimal cloud image (pinned SHA-256), converts
   it to raw (`ubuntu-source.raw`, 3.5 GiB) and generates a deterministic
   odd-sized synthetic disk (`synthetic-block.raw`, 300 MiB + 7 sectors).
2. Builds csi-driver-host-path v1.18.0, external-provisioner v6.3.0 and
   node-driver-registrar v2.18.0 from their release tags (registry images may
   be unreachable), the fake vSphere server, and the controller and worker
   images from this checkout.
3. Creates a kind cluster (`kindest/node:v1.37.0`), installs the CSI driver,
   the CRD, RBAC and the controller, and runs the fake vSphere as a Deployment
   serving both disks as a VM with two SCSI disks.
4. Creates a Filesystem PVC (Immediate binding) from disk 0 with
   `requireManifest: true`, and a Block PVC (WaitForFirstConsumer) from disk 1
   with a consumer pod.
5. Waits for both populations to succeed, checks that no prime PVCs remain,
   and compares each destination with its source raw file using `sha256sum`
   (independent of the populator's digests). For the Block PVC it also checks
   that the bytes beyond the disk size are zero.
6. Copies the migrated `disk.img` out of the cluster, verifies its SHA-256
   again, and boots it with QEMU (TCG) using `boot-check.sh`, which waits for a
   cloud-init marker printed from inside the guest. This is a QEMU boot, not a
   KubeVirt boot.

Evidence (report, controller/worker/fake logs, events, final
`VSphereVolumeSource` objects) is written to `$E2E_WORK` (default
`hack/e2e/work`).

Environment knobs: `BOOT_CHECK=0` skips the boot, `KEEP=1` keeps the
cluster, `EXTRA_DOCKER_BUILD_ARGS` passes extra `docker build` arguments
(for example `--build-context` to substitute a base image in a sandbox that
intercepts TLS). On hosts with cgroup v1 the script sets kubelet
`failCgroupV1: false`, and on hosts that cannot lower OOM scores it sets
containerd `restrict_oom_score_adj = true`; both apply only to the disposable
test cluster.
