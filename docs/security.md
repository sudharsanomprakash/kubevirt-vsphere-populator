# Security considerations

## Credentials

* vSphere credentials come from a Secret in the PVC's namespace
  (`username`, `password`, optional `ca.crt` and `thumbprint`), mounted
  read-only (mode 0440) into the worker pod. The worker also accepts the
  `VSPHERE_USERNAME` and `VSPHERE_PASSWORD` environment variables for
  standalone use. There is no command-line flag for the password.
* The controller never reads Secrets and has no RBAC permission to do so. It
  only names the Secret in the worker pod spec.
* Use a vSphere account limited to what an export needs (read access to the
  VM and the privilege to export it, `VApp.Export`), not an administrator.
* The worker pod has no Kubernetes service account token
  (`automountServiceAccountToken: false`).

## TLS

* Verification is on by default. The server certificate must chain to the
  system roots or to `ca.crt`, or match a pinned SHA-1/SHA-256 `thumbprint`
  (from the Secret or `spec.tls.thumbprint`). Thumbprint pinning skips the
  hostname check, as in govmomi.
* NFC hosts: when connected through vCenter, disks are served by ESXi hosts.
  Their certificates are verified against the same CA bundle; failing that,
  the per-device `sslThumbprint` returned in the export lease is accepted,
  because it was received over the already verified vCenter session. Set
  `spec.tls.trustLeaseThumbprints: false` to require CA validation for ESXi
  hosts as well.
* Disabling verification requires both
  `spec.tls.insecureSkipVerifyForTestingOnly: true` and the controller flag
  `--allow-insecure-tls-for-testing`. Without the flag the source is rejected.
  Never enable this outside test environments.

## Redaction

Export URLs contain lease tickets. Everything the worker logs, writes to its
termination message, reports in the `diagnose` output or sends back to
vSphere in an abort fault is passed through `internal/redact`: registered
secrets (password, full URLs, long path elements, query values) are replaced,
NFC URLs are reduced to `https://host/REDACTED/disk-0.vmdk`, URL user
information and `password=`/`token=`/`ticket=`/session-cookie patterns are
masked, and `diagnose` only reports allow-listed response headers. Tests check
that failure messages and diagnostic reports contain neither the password nor
ticket-bearing URLs.

## Who can make the worker send credentials where

Anyone who can create a `VSphereVolumeSource` and a PVC in a namespace can
cause a worker pod in that namespace to mount **any Secret in that namespace
that they name** and to send its `username` and `password` to **the server they
name**. Therefore:

* grant `create` on `vspherevolumesources` only to principals who may already
  read the Secrets in that namespace (the shipped aggregated ClusterRole gives
  it to namespace `admin`; remove it if that is too broad);
* start the controller with `--allowed-server-pattern` (a regular expression
  that `spec.server` must fully match), for example
  `--allowed-server-pattern='vcenter\.corp\.example\.test(:443)?'`;
* keep migration credentials in dedicated namespaces.

## Pod security

* Filesystem-mode workers run as UID/GID 107 (the `qemu` user KubeVirt uses,
  so `disk.img` is owned accordingly) with `fsGroup` 107, non-root, no
  privilege escalation, all capabilities dropped, read-only root filesystem and
  the `RuntimeDefault` seccomp profile; they satisfy the `restricted` Pod
  Security Standard.
* Block-mode workers run as UID 0 (with all capabilities dropped) because
  block device nodes in containers are usually owned by root. They satisfy
  `baseline`, not `restricted`. If your container runtime sets device
  ownership from the security context (containerd
  `device_ownership_from_security_context`), set
  `--worker-block-run-as-user=107`.
* The controller runs non-root with a read-only root filesystem and needs
  cluster-wide access to PVCs, PVs (patch for rebinding), Jobs, pods (read),
  events, StorageClasses and its own CRD.

## Supply chain

* Base images and the `qemu-utils` package version are pinned
  (`build/Dockerfile.*`). qemu-img runs as a subprocess and is never given
  `--target-is-zero`, and the input format is always forced to `vmdk`.
* Images are not published by this repository yet; build them yourself
  (`make images`) and push them to a registry you control.

## Reporting vulnerabilities

See [SECURITY.md](../SECURITY.md).
