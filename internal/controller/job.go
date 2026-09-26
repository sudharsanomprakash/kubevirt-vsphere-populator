package controller

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/sudharsanomprakash/kubevirt-vsphere-populator/api/v1alpha1"
)

// Names, labels and annotations used by the controller.
const (
	Finalizer          = v1alpha1.Group + "/populate"
	LabelManagedBy     = "app.kubernetes.io/managed-by"
	ManagedByValue     = "vsphere-populator"
	LabelComponent     = "app.kubernetes.io/component"
	LabelPVCUID        = v1alpha1.Group + "/pvc-uid"
	AnnPVCName         = v1alpha1.Group + "/pvc-name"
	AnnPopulatedFrom   = v1alpha1.Group + "/populated-from"
	AnnGuestDigest     = v1alpha1.Group + "/guest-digest"
	AnnSelectedNode    = "volume.kubernetes.io/selected-node"
	primePrefix        = "vsphere-populator-prime-"
	jobPrefix          = "vsphere-populator-"
	credentialsMount   = "/etc/vsphere-populator/credentials"
	scratchMount       = "/scratch"
	fileTargetMount    = "/target"
	blockTargetDevice  = "/dev/vsphere-populator-target"
	progressPort       = 8080
	workerContainer    = "worker"
	defaultBackoff     = int32(2)
	scratchOverheadRaw = 256 << 20
)

// PrimeName is the name of the prime PVC for a target PVC UID.
func PrimeName(uid string) string { return primePrefix + uid }

// JobName is the name of the worker Job for a target PVC UID.
func JobName(uid string) string { return jobPrefix + uid }

// JobOptions carries controller-level settings for worker Jobs.
type JobOptions struct {
	Image           string
	ImagePullPolicy corev1.PullPolicy
	// FileRunAsUser is the UID used for filesystem-mode workers (default 107,
	// the qemu user KubeVirt runs as, so that disk.img is owned accordingly).
	FileRunAsUser int64
	// BlockRunAsUser is the UID used for block-mode workers. Block device
	// nodes are usually root-owned inside the container, so the default is 0
	// with all capabilities dropped.
	BlockRunAsUser int64
	// DefaultScratchStorageClass is used when the source does not set one;
	// empty means emptyDir.
	DefaultScratchStorageClass string
	ImagePullSecrets           []corev1.LocalObjectReference
}

func isBlock(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock
}

// workerArgs builds the worker command line. It contains no secrets.
func workerArgs(src *v1alpha1.VSphereVolumeSource, block bool) []string {
	s := src.Spec
	args := []string{"populate",
		"--server=" + s.Server,
		"--credentials-dir=" + credentialsMount,
		"--staging-dir=" + scratchMount,
		"--progress-addr=:" + strconv.Itoa(progressPort),
		"--log-format=json",
	}
	vm := s.VirtualMachine
	switch {
	case vm.MoRef != "":
		args = append(args, "--vm-moref="+vm.MoRef)
	case vm.InstanceUUID != "":
		args = append(args, "--vm-instance-uuid="+vm.InstanceUUID)
	case vm.BIOSUUID != "":
		args = append(args, "--vm-bios-uuid="+vm.BIOSUUID)
	case vm.Name != "":
		args = append(args, "--vm-name="+vm.Name)
	}
	if vm.Datacenter != "" {
		args = append(args, "--datacenter="+vm.Datacenter)
	}
	d := s.Disk
	switch {
	case d.DeviceKey != nil:
		args = append(args, "--disk-key="+strconv.Itoa(int(*d.DeviceKey)))
	case d.BusLocation != "":
		args = append(args, "--disk-bus="+d.BusLocation)
	case d.FileName != "":
		args = append(args, "--disk-file="+d.FileName)
	}
	if d.ExpectedCapacityBytes != nil {
		args = append(args, "--expected-capacity-bytes="+strconv.FormatInt(*d.ExpectedCapacityBytes, 10))
	}
	if block {
		args = append(args, "--target-mode=block", "--target-path="+blockTargetDevice, "--zero-tail=true")
	} else {
		args = append(args, "--target-mode=file", "--target-path="+fileTargetMount)
	}
	if t := s.TLS; t != nil {
		if t.Thumbprint != "" {
			args = append(args, "--thumbprint="+t.Thumbprint)
		}
		if t.TrustLeaseThumbprints != nil && !*t.TrustLeaseThumbprints {
			args = append(args, "--trust-lease-thumbprints=false")
		}
		if t.InsecureSkipVerifyForTestingOnly {
			args = append(args, "--insecure-skip-tls-verify-for-testing-only")
		}
	}
	if t := s.Transfer; t != nil {
		if t.Timeout != nil {
			args = append(args, "--timeout="+t.Timeout.Duration.String())
		}
		if t.IdleTimeout != nil {
			args = append(args, "--idle-timeout="+t.IdleTimeout.Duration.String())
		}
		if t.LeaseReadyTimeout != nil {
			args = append(args, "--lease-ready-timeout="+t.LeaseReadyTimeout.Duration.String())
		}
		if t.RequireManifest {
			args = append(args, "--require-manifest")
		}
	}
	return args
}

// scratchSize returns the scratch volume size: an explicit size, or the PVC
// request plus 2% and 256Mi. A streamOptimized export of a disk is not
// larger than the disk plus small per-grain overhead, and the PVC must be at
// least as large as the disk.
func scratchSize(src *v1alpha1.VSphereVolumeSource, pvc *corev1.PersistentVolumeClaim) resource.Quantity {
	if w := src.Spec.Worker; w != nil && w.Scratch != nil && w.Scratch.Size != nil {
		return *w.Scratch.Size
	}
	req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	v := req.Value()
	return *resource.NewQuantity(v+v/50+scratchOverheadRaw, resource.BinarySI)
}

// BuildJob returns the worker Job that populates prime for pvc.
func BuildJob(pvc *corev1.PersistentVolumeClaim, prime *corev1.PersistentVolumeClaim, src *v1alpha1.VSphereVolumeSource, nodeName string, opts JobOptions) *batchv1.Job {
	uid := string(pvc.UID)
	block := isBlock(pvc)
	labels := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelComponent: "worker",
		LabelPVCUID:    uid,
	}
	annotations := map[string]string{AnnPVCName: pvc.Name}

	backoff := defaultBackoff
	var deadline *int64
	var tolerations []corev1.Toleration
	var resources corev1.ResourceRequirements
	var scratchClass string = opts.DefaultScratchStorageClass
	if w := src.Spec.Worker; w != nil {
		if w.BackoffLimit != nil {
			backoff = *w.BackoffLimit
		}
		deadline = w.ActiveDeadlineSeconds
		tolerations = w.Tolerations
		if w.Resources != nil {
			resources = *w.Resources
		}
		if w.Scratch != nil && w.Scratch.StorageClassName != nil {
			scratchClass = *w.Scratch.StorageClassName
		}
	}
	if resources.Requests == nil && resources.Limits == nil {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}
	}

	uidToRun := opts.FileRunAsUser
	if block {
		uidToRun = opts.BlockRunAsUser
	}
	podSC := &corev1.PodSecurityContext{
		RunAsUser:      ptr.To(uidToRun),
		RunAsGroup:     ptr.To(uidToRun),
		RunAsNonRoot:   ptr.To(uidToRun != 0),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if !block {
		podSC.FSGroup = ptr.To(uidToRun)
		podSC.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch)
	}

	size := scratchSize(src, pvc)
	scratch := corev1.Volume{Name: "scratch"}
	if scratchClass != "" {
		scratch.Ephemeral = &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelPVCUID: uid}},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: ptr.To(scratchClass),
				VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			},
		}}
	} else {
		scratch.EmptyDir = &corev1.EmptyDirVolumeSource{SizeLimit: &size}
	}

	container := corev1.Container{
		Name:            workerContainer,
		Image:           opts.Image,
		ImagePullPolicy: opts.ImagePullPolicy,
		Args:            workerArgs(src, block),
		Env:             []corev1.EnvVar{{Name: "TMPDIR", Value: scratchMount}},
		Ports:           []corev1.ContainerPort{{Name: "progress", ContainerPort: progressPort, Protocol: corev1.ProtocolTCP}},
		Resources:       resources,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "credentials", MountPath: credentialsMount, ReadOnly: true},
			{Name: "scratch", MountPath: scratchMount},
		},
	}
	if block {
		container.VolumeDevices = []corev1.VolumeDevice{{Name: "target", DevicePath: blockTargetDevice}}
	} else {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "target", MountPath: fileTargetMount})
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		SecurityContext:               podSC,
		Tolerations:                   tolerations,
		ImagePullSecrets:              opts.ImagePullSecrets,
		TerminationGracePeriodSeconds: ptr.To(int64(60)),
		Containers:                    []corev1.Container{container},
		Volumes: []corev1.Volume{
			{Name: "target", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: prime.Name}}},
			{Name: "credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: src.Spec.CredentialsSecretRef.Name, DefaultMode: ptr.To(int32(0o440))}}},
			scratch,
		},
	}
	if nodeName != "" {
		// WaitForFirstConsumer: run where the consumer was scheduled so that
		// node-local storage is provisioned on the right node.
		podSpec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{nodeName}}},
			}}},
		}}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: JobName(uid), Namespace: pvc.Namespace, Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{ownerRef(pvc)},
		},
		Spec: batchv1.JobSpec{
			Parallelism:           ptr.To(int32(1)),
			Completions:           ptr.To(int32(1)),
			BackoffLimit:          ptr.To(backoff),
			ActiveDeadlineSeconds: deadline,
			// A replacement pod is created only after the previous pod has
			// fully terminated, so two workers never write the volume at once.
			PodReplacementPolicy: ptr.To(batchv1.Failed),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec:       podSpec,
			},
		},
	}
}

func ownerRef(pvc *corev1.PersistentVolumeClaim) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: pvc.Name, UID: pvc.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false),
	}
}

// BuildPrime returns the prime PVC for pvc.
func BuildPrime(pvc *corev1.PersistentVolumeClaim, nodeName string) *corev1.PersistentVolumeClaim {
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: PrimeName(string(pvc.UID)), Namespace: pvc.Namespace,
			Labels:          map[string]string{LabelManagedBy: ManagedByValue, LabelComponent: "prime", LabelPVCUID: string(pvc.UID)},
			Annotations:     map[string]string{AnnPVCName: pvc.Name},
			OwnerReferences: []metav1.OwnerReference{ownerRef(pvc)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:               pvc.Spec.AccessModes,
			Resources:                 pvc.Spec.Resources,
			StorageClassName:          pvc.Spec.StorageClassName,
			VolumeMode:                pvc.Spec.VolumeMode,
			VolumeAttributesClassName: pvc.Spec.VolumeAttributesClassName,
		},
	}
	if nodeName != "" {
		p.Annotations[AnnSelectedNode] = nodeName
	}
	return p
}

// ValidateSource applies controller-level checks beyond the CRD schema.
func ValidateSource(src *v1alpha1.VSphereVolumeSource, allowInsecure bool, allowedServer func(string) bool) error {
	s := src.Spec
	if t := s.TLS; t != nil && t.InsecureSkipVerifyForTestingOnly && !allowInsecure {
		return fmt.Errorf("tls.insecureSkipVerifyForTestingOnly is set but the controller does not allow insecure TLS (start it with --allow-insecure-tls-for-testing in test environments only)")
	}
	if allowedServer != nil && !allowedServer(s.Server) {
		return fmt.Errorf("server %q is not permitted by the controller's --allowed-server-pattern", s.Server)
	}
	if strings.HasPrefix(strings.ToLower(s.Server), "http://") {
		return fmt.Errorf("server must use https")
	}
	if s.CredentialsSecretRef.Name == "" {
		return fmt.Errorf("credentialsSecretRef.name is required")
	}
	return nil
}
