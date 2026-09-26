# Troubleshooting

Start with the source's status and the PVC's events:

```sh
kubectl -n <ns> get vspherevolumesource <name> -o yaml    # status.conditions, status.populations[]
kubectl -n <ns> describe pvc <pvc>                        # events from vsphere-populator
kubectl -n <ns> get jobs,pods -l app.kubernetes.io/managed-by=vsphere-populator
kubectl -n <ns> logs job/vsphere-populator-<pvc-uid>      # JSON logs; no secrets or tickets
kubectl -n vsphere-populator-system logs deploy/vsphere-populator-controller
```

The worker's final result is in the pod's termination message
(`kubectl get pod <worker-pod> -o jsonpath='{.status.containerStatuses[0].state.terminated.message}'`).

To inspect an export without writing anything, run the diagnostic from a
machine or pod that can reach vCenter (the lease is always aborted):

```sh
vsphere-populator-worker diagnose --server vcenter.example.test \
  --credentials-dir ./creds --vm-instance-uuid <uuid> --disk-bus scsi:0:0 [--full-read]
```

| Symptom | Likely cause and action |
|---|---|
| PVC stays `Pending`, no events from `vsphere-populator` | The controller is not running, or `dataSourceRef.apiGroup`/`kind` is wrong. |
| Event `SourceNotFound` | The `VSphereVolumeSource` does not exist in the PVC's namespace (cross-namespace references are not supported). |
| Condition `Accepted=False`, "insecure" | `tls.insecureSkipVerifyForTestingOnly` is set but the controller does not allow it. Fix TLS instead (CA bundle or thumbprint). |
| `Accepted=False`, "not permitted by ... --allowed-server-pattern" | The server does not match the controller's allow pattern. |
| Phase `WaitingForFirstConsumer` | The StorageClass uses `WaitForFirstConsumer`: create the pod/VM that uses the PVC. |
| PVC provisioned directly and empty | The StorageClass does not use a CSI provisioner that ignores unknown data sources. Use a CSI driver. |
| Worker pod `CreateContainerConfigError` | The credentials Secret named in the source is missing in the namespace. |
| "not trusted: supply a CA bundle or a verified thumbprint" | Add `ca.crt` or `thumbprint` to the Secret. |
| Certificate error after the lease is ready | The ESXi host serving the disk is not trusted. Include the ESXi (VMCA) CA in `ca.crt`, or keep `trustLeaseThumbprints` enabled. |
| "matches N VMs ... use instanceUUID or moRef" | Name or BIOS UUID is not unique. Use a stable identity. |
| "power state is poweredOn" | Cold migration only: power the VM off. |
| "has snapshots" / "disk has a parent" | Consolidate or delete snapshots first. |
| "key convention for this layout has not been verified" | The disk's controller number differs from its bus number. Run `diagnose` and report the lease keys; re-arranging controllers avoids it. |
| "no lease device URL has key ..." / "unrecognized lease device key" | The server's device-key format differs from the documented one. Run `diagnose` and open an issue with the (redacted) report. |
| "stream truncated", "decompressed to ... bytes", "end-of-stream" | The export was cut off or corrupted in transit; the attempt is retried by the Job. Persistent failures: check proxies/load balancers between the cluster and ESXi. |
| "no data received within the idle timeout" | ESXi stopped sending. Check host load and network; raise `transfer.idleTimeout` if the host is merely slow. |
| "lease keepalive failed" | vCenter/ESXi rejected lease renewal (lease expired or was cancelled in vSphere). |
| "does not match the vSphere manifest" | The received bytes differ from what vSphere reports. Do not use the result; investigate the network path. |
| "manifest required but unavailable" | `transfer.requireManifest` is set and the server returned no checksum. |
| "staging capacity exceeded" / insufficient staging | Increase the scratch volume (`worker.scratch.size` or a scratch StorageClass). |
| "insufficient destination capacity" | The PVC is smaller than the disk (Filesystem PVCs need the full virtual size free). |
| "destination content does not match the exported disk" | The read-back digest differs from the decoded stream. Treat the storage as suspect. |
| `NoValidationEvidence` | The worker exited 0 but its result did not prove a validated copy; the volume was not handed over. |
| Population `Failed` after retries | Delete and recreate the PVC to retry; the prime PVC and Job are kept until then for inspection. |
