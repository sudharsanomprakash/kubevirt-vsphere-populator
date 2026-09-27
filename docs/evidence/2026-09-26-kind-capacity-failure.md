# Incidental failure-path observation, 2026-09-26

During an earlier kind run the development host's disk was nearly full, so the
hostpath Filesystem volume (which reports the node filesystem) had 1.6 GB free
for a 3.5 GiB disk. The worker refused before requesting an export lease, the
Job retried once (backoffLimit 1), and the controller reported the failure. No
```yaml
apiVersion: vsphere-populator.sudharsanomprakash.github.io/v1alpha1
kind: VSphereVolumeSource
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"vsphere-populator.sudharsanomprakash.github.io/v1alpha1","kind":"VSphereVolumeSource","metadata":{"annotations":{},"name":"app-01-disk0","namespace":"vm-migration-e2e"},"spec":{"credentialsSecretRef":{"name":"vsphere-credentials"},"disk":{"busLocation":"scsi:0:0","expectedCapacityBytes":3758096384},"server":"fake-vsphere.fake-vsphere.svc:8989","transfer":{"requireManifest":true},"virtualMachine":{"instanceUUID":"5b12c80b-1013-59dd-8c49-6adc837e0c7d"},"worker":{"backoffLimit":1}}}
  creationTimestamp: "2026-09-26T07:02:20Z"
  generation: 1
  name: app-01-disk0
  namespace: vm-migration-e2e
  resourceVersion: "900"
  uid: ae569240-09a8-4e91-82d3-365fe2842856
spec:
  credentialsSecretRef:
    name: vsphere-credentials
  disk:
    busLocation: scsi:0:0
    expectedCapacityBytes: 3758096384
  server: fake-vsphere.fake-vsphere.svc:8989
  transfer:
    requireManifest: true
  virtualMachine:
    instanceUUID: 5b12c80b-1013-59dd-8c49-6adc837e0c7d
  worker:
    backoffLimit: 1
status:
  conditions:
  - lastTransitionTime: "2026-09-26T07:02:20Z"
    message: source accepted
    observedGeneration: 1
    reason: Valid
    status: "True"
    type: Accepted
  - lastTransitionTime: "2026-09-26T07:02:43Z"
    message: 0 population(s) in progress
    observedGeneration: 1
    reason: Populations
    status: "False"
    type: Populating
  - lastTransitionTime: "2026-09-26T07:02:43Z"
    message: 'app-01-root: insufficient destination capacity: filesystem has 1609265152
      bytes available, the disk needs 3758096384'
    observedGeneration: 1
    reason: PopulationFailed
    status: "True"
    type: Failed
  observedGeneration: 1
  populations:
  - completionTime: "2026-09-26T07:02:43Z"
    failedAttempts: 2
    jobName: vsphere-populator-af9bdf68-e819-4f58-be31-aa457c60dc44
    message: 'insufficient destination capacity: filesystem has 1609265152 bytes available,
      the disk needs 3758096384'
    phase: Failed
    progress:
      updateTime: "2026-09-26T07:02:40Z"
      workerPhase: Discovering
    pvcName: app-01-root
    pvcUID: af9bdf68-e819-4f58-be31-aa457c60dc44
    startTime: "2026-09-26T07:02:20Z"
```
