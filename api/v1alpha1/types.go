// Package v1alpha1 contains the VSphereVolumeSource API.
//
// +kubebuilder:object:generate=true
// +groupName=vsphere-populator.sudharsanomprakash.github.io
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// Group, version and kind of the API.
const (
	Group   = "vsphere-populator.sudharsanomprakash.github.io"
	Version = "v1alpha1"
	Kind    = "VSphereVolumeSource"
)

var (
	// GroupVersion is the API group and version.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}
	// SchemeBuilder registers the types.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&VSphereVolumeSource{}, &VSphereVolumeSourceList{})
}

// VSphereVolumeSource describes one virtual disk of a powered-off vSphere VM
// that can populate PersistentVolumeClaims through spec.dataSourceRef.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vsvs,categories=kubevirt
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.server`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VSphereVolumeSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VSphereVolumeSourceSpec   `json:"spec"`
	Status VSphereVolumeSourceStatus `json:"status,omitempty"`
}

// VSphereVolumeSourceSpec identifies the source disk and how to reach it.
type VSphereVolumeSourceSpec struct {
	// Server is the vCenter or standalone ESXi host name, host:port or https URL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('http://')",message="server must use https"
	// +kubebuilder:validation:XValidation:rule="!self.contains('@')",message="server must not contain credentials"
	Server string `json:"server"`

	// CredentialsSecretRef names a Secret in the same namespace with the keys
	// "username" and "password", and optionally "ca.crt" (PEM bundle) and
	// "thumbprint" (verified certificate fingerprint).
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

	// TLS controls server certificate verification. Verification is always
	// enabled unless insecureSkipVerifyForTestingOnly is set and the
	// controller permits it.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// VirtualMachine identifies the source VM.
	VirtualMachine VMSelector `json:"virtualMachine"`

	// Disk identifies the virtual disk to export.
	Disk DiskSelector `json:"disk"`

	// Transfer tunes timeouts and verification.
	// +optional
	Transfer *TransferSpec `json:"transfer,omitempty"`

	// Worker tunes the worker Job.
	// +optional
	Worker *WorkerSpec `json:"worker,omitempty"`
}

// TLSSpec controls certificate verification.
type TLSSpec struct {
	// Thumbprint pins the server certificate by its SHA-1 or SHA-256
	// fingerprint (hex, with or without colons). When set it is used instead
	// of a "thumbprint" key in the credentials Secret.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:?){19}[0-9A-Fa-f]{2}$|^([0-9A-Fa-f]{2}:?){31}[0-9A-Fa-f]{2}$`
	Thumbprint string `json:"thumbprint,omitempty"`
	// TrustLeaseThumbprints accepts ESXi host certificates by the thumbprint
	// that vCenter returns in the export lease over the verified API
	// connection. Defaults to true.
	// +optional
	TrustLeaseThumbprints *bool `json:"trustLeaseThumbprints,omitempty"`
	// InsecureSkipVerifyForTestingOnly disables certificate verification.
	// It is rejected unless the controller runs with
	// --allow-insecure-tls-for-testing.
	// +optional
	InsecureSkipVerifyForTestingOnly bool `json:"insecureSkipVerifyForTestingOnly,omitempty"`
}

// VMSelector identifies the VM. Exactly one of moRef, instanceUUID, biosUUID
// or name must be set; moRef and instanceUUID are the stable identities.
// biosUUID and name are accepted only when they match exactly one VM.
// +kubebuilder:validation:XValidation:rule="[has(self.moRef), has(self.instanceUUID), has(self.biosUUID), has(self.name)].filter(x, x).size() == 1",message="exactly one of moRef, instanceUUID, biosUUID or name must be set"
type VMSelector struct {
	// +optional
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.:-]{1,80}$`
	MoRef string `json:"moRef,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9A-Fa-f-]{32,36}$`
	InstanceUUID string `json:"instanceUUID,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9A-Fa-f-]{32,36}$`
	BIOSUUID string `json:"biosUUID,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=80
	Name string `json:"name,omitempty"`
	// Datacenter restricts UUID and name searches.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Datacenter string `json:"datacenter,omitempty"`
}

// DiskSelector identifies the disk. Exactly one of deviceKey, busLocation or
// fileName must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.deviceKey), has(self.busLocation), has(self.fileName)].filter(x, x).size() == 1",message="exactly one of deviceKey, busLocation or fileName must be set"
type DiskSelector struct {
	// DeviceKey is the virtual device key, for example 2000.
	// +optional
	// +kubebuilder:validation:Minimum=1
	DeviceKey *int32 `json:"deviceKey,omitempty"`
	// BusLocation is "<scsi|sata|nvme|ide>:<bus>:<unit>", for example "scsi:0:1".
	// +optional
	// +kubebuilder:validation:Pattern=`^(scsi|sata|nvme|ide):[0-9]{1,2}:[0-9]{1,2}$`
	BusLocation string `json:"busLocation,omitempty"`
	// FileName is the backing file, for example "[datastore1] vm/vm_1.vmdk".
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	FileName string `json:"fileName,omitempty"`
	// ExpectedCapacityBytes, when set, must equal the disk capacity.
	// +optional
	// +kubebuilder:validation:Minimum=512
	ExpectedCapacityBytes *int64 `json:"expectedCapacityBytes,omitempty"`
}

// TransferSpec tunes the transfer.
type TransferSpec struct {
	// Timeout bounds one attempt (default: no limit).
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// IdleTimeout aborts an attempt when no export bytes arrive (default 5m).
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`
	// LeaseReadyTimeout bounds the wait for the export lease (default 10m).
	// +optional
	LeaseReadyTimeout *metav1.Duration `json:"leaseReadyTimeout,omitempty"`
	// RequireManifest fails the population if vSphere provides no manifest
	// checksum for the exported stream.
	// +optional
	RequireManifest bool `json:"requireManifest,omitempty"`
}

// WorkerSpec tunes the worker Job.
type WorkerSpec struct {
	// BackoffLimit is the number of retries of the worker Job (default 2).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
	// ActiveDeadlineSeconds bounds the whole Job including retries.
	// +optional
	// +kubebuilder:validation:Minimum=60
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// Resources for the worker container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Scratch configures the volume that stages the export stream.
	// +optional
	Scratch *ScratchSpec `json:"scratch,omitempty"`
	// Tolerations for the worker pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// ScratchSpec configures staging storage. By default an emptyDir with a
// size limit is used; when storageClassName is set a generic ephemeral
// volume of that class is used and deleted with the worker pod.
type ScratchSpec struct {
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// Size of the scratch volume. Defaults to the PVC request plus 2% and 256Mi.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
}

// Condition types.
const (
	// ConditionAccepted reports whether the spec is usable by this controller.
	ConditionAccepted = "Accepted"
	// ConditionPopulating is true while any population from this source runs.
	ConditionPopulating = "Populating"
	// ConditionFailed is true when the most recent population failed permanently.
	ConditionFailed = "Failed"
)

// Population phases.
const (
	PhasePending                 = "Pending"
	PhaseWaitingForFirstConsumer = "WaitingForFirstConsumer"
	PhaseRunning                 = "Running"
	PhaseRebinding               = "Rebinding"
	PhaseSucceeded               = "Succeeded"
	PhaseFailed                  = "Failed"
	PhaseCancelled               = "Cancelled"
)

// VSphereVolumeSourceStatus reports populations from this source.
type VSphereVolumeSourceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Populations lists the PVCs populated (or being populated) from this source.
	// +optional
	// +listType=map
	// +listMapKey=pvcUID
	Populations []PopulationStatus `json:"populations,omitempty"`
}

// PopulationStatus describes the population of one PVC.
type PopulationStatus struct {
	PVCName string    `json:"pvcName"`
	PVCUID  types.UID `json:"pvcUID"`
	// +kubebuilder:validation:Enum=Pending;WaitingForFirstConsumer;Running;Rebinding;Succeeded;Failed;Cancelled
	Phase string `json:"phase"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// FailedAttempts counts failed worker pods.
	// +optional
	FailedAttempts int32 `json:"failedAttempts,omitempty"`
	// +optional
	Progress *PopulationProgress `json:"progress,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// Result is recorded from the worker's validated termination message.
	// +optional
	Result *PopulationResult `json:"result,omitempty"`
}

// PopulationProgress is polled from the running worker.
type PopulationProgress struct {
	WorkerPhase string `json:"workerPhase,omitempty"`
	// StreamBytes counts exported bytes received.
	StreamBytes int64 `json:"streamBytes,omitempty"`
	// GuestBytes is the guest disk offset covered so far.
	GuestBytes int64 `json:"guestBytes,omitempty"`
	// VirtualSize is the guest disk size.
	VirtualSize int64 `json:"virtualSize,omitempty"`
	// TransferPercent is GuestBytes/VirtualSize as an integer percentage.
	TransferPercent int32 `json:"transferPercent,omitempty"`
	// ConvertPercent is qemu-img's progress as an integer percentage.
	ConvertPercent int32 `json:"convertPercent,omitempty"`
	// VerifiedBytes counts destination bytes read back.
	VerifiedBytes int64 `json:"verifiedBytes,omitempty"`
	// BytesPerSecond is the average download rate.
	BytesPerSecond int64        `json:"bytesPerSecond,omitempty"`
	UpdateTime     *metav1.Time `json:"updateTime,omitempty"`
}

// PopulationResult summarizes a validated population.
type PopulationResult struct {
	VirtualSize       int64  `json:"virtualSize"`
	GuestDigest       string `json:"guestDigest"`
	DestinationDigest string `json:"destinationDigest"`
	ExportBytes       int64  `json:"exportBytes,omitempty"`
	ExportSHA256      string `json:"exportSHA256,omitempty"`
	// ManifestStatus is verified, unavailable, no-entry or no-checksum.
	ManifestStatus string `json:"manifestStatus,omitempty"`
	LeaseKey       string `json:"leaseKey,omitempty"`
	VMMoRef        string `json:"vmMoRef,omitempty"`
	DiskKey        int32  `json:"diskKey,omitempty"`
	ServerVersion  string `json:"serverVersion,omitempty"`
	QemuImgVersion string `json:"qemuImgVersion,omitempty"`
	// ExportMillis is the download duration.
	ExportMillis int64 `json:"exportMillis,omitempty"`
	// TotalMillis is the worker's total duration.
	TotalMillis int64 `json:"totalMillis,omitempty"`
}

// VSphereVolumeSourceList is a list of VSphereVolumeSource.
//
// +kubebuilder:object:root=true
type VSphereVolumeSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VSphereVolumeSource `json:"items"`
}
