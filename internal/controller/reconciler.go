// Package controller implements the Kubernetes volume populator for
// VSphereVolumeSource: it watches PVCs whose spec.dataSourceRef names a
// VSphereVolumeSource, populates a temporary "prime" PVC with a worker Job,
// and after the worker reports a validated result, rebinds the prime PVC's
// PersistentVolume to the user's PVC.
//
// This follows the pattern of kubernetes-csi/lib-volume-populator, but keeps
// the prime PVC and worker in the PVC's own namespace so that the worker can
// mount the credentials Secret there, uses a Job with bounded retries, and
// records Conditions and per-PVC status on the VSphereVolumeSource. See
// docs/architecture.md for why the library itself is not used.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/sudharsanomprakash/kubevirt-vsphere-populator/api/v1alpha1"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/progress"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/worker"
)

// Event reasons.
const (
	ReasonSourceNotFound   = "SourceNotFound"
	ReasonInvalidSource    = "InvalidSource"
	ReasonUnsupported      = "Unsupported"
	ReasonWaitingConsumer  = "WaitingForFirstConsumer"
	ReasonStarted          = "PopulatorStarted"
	ReasonWorkerFailed     = "WorkerAttemptFailed"
	ReasonFailed           = "PopulationFailed"
	ReasonValidated        = "PopulationValidated"
	ReasonRebound          = "VolumeRebound"
	ReasonFinished         = "PopulationFinished"
	ReasonCancelled        = "PopulationCancelled"
	ReasonNoValidationData = "NoValidationEvidence"
)

// Options configures the reconciler.
type Options struct {
	Job                  JobOptions
	AllowInsecureTLS     bool
	AllowedServer        func(string) bool
	ProgressPollInterval time.Duration
	// HTTPClient polls worker progress; nil disables polling.
	HTTPClient *http.Client
}

// Reconciler reconciles PVCs that reference a VSphereVolumeSource.
type Reconciler struct {
	client.Client
	Recorder events.EventRecorder
	Opts     Options
}

// IsOurs reports whether pvc's dataSourceRef names a VSphereVolumeSource.
func IsOurs(pvc *corev1.PersistentVolumeClaim) bool {
	ref := pvc.Spec.DataSourceRef
	return ref != nil && ref.APIGroup != nil && *ref.APIGroup == v1alpha1.Group && ref.Kind == v1alpha1.Kind && ref.Name != ""
}

// Reconcile drives one PVC towards a populated, bound state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !IsOurs(&pvc) {
		return ctrl.Result{}, nil
	}
	uid := string(pvc.UID)
	srcName := pvc.Spec.DataSourceRef.Name

	if pvc.DeletionTimestamp != nil {
		return r.finish(ctx, &pvc, srcName, v1alpha1.PhaseCancelled, "PVC deleted before population completed", nil)
	}

	if pvc.Spec.VolumeName != "" {
		if !controllerutil.ContainsFinalizer(&pvc, Finalizer) {
			return ctrl.Result{}, nil // already finished, or bound by someone else
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			return ctrl.Result{}, nil
		}
		var pv corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if _, ok := pv.Annotations[AnnPopulatedFrom]; !ok {
			// Bound to a volume we did not populate (for example a
			// pre-bound volumeName). Do not claim success.
			r.Recorder.Eventf(&pvc, nil, corev1.EventTypeWarning, ReasonUnsupported, "Populate",
				"PVC was bound to PersistentVolume %s that was not populated by this controller", pv.Name)
			return r.finish(ctx, &pvc, srcName, v1alpha1.PhaseFailed, "bound to a volume that was not populated from the source", nil)
		}
		logger.Info("population finished", "pv", pv.Name)
		r.Recorder.Eventf(&pvc, nil, corev1.EventTypeNormal, ReasonFinished, "Populate", "PVC bound to populated volume %s", pv.Name)
		return r.finish(ctx, &pvc, srcName, v1alpha1.PhaseSucceeded, "populated and bound to "+pv.Name, nil)
	}

	if ns := pvc.Spec.DataSourceRef.Namespace; ns != nil && *ns != "" && *ns != pvc.Namespace {
		r.Recorder.Eventf(&pvc, nil, corev1.EventTypeWarning, ReasonUnsupported, "Populate", "%s", "cross-namespace dataSourceRef is not supported")
		return ctrl.Result{}, nil
	}

	var src v1alpha1.VSphereVolumeSource
	if err := r.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: srcName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			r.Recorder.Eventf(&pvc, nil, corev1.EventTypeWarning, ReasonSourceNotFound, "Populate", "VSphereVolumeSource %s not found", srcName)
			return ctrl.Result{}, nil // the source watch requeues
		}
		return ctrl.Result{}, err
	}
	if err := ValidateSource(&src, r.Opts.AllowInsecureTLS, r.Opts.AllowedServer); err != nil {
		r.Recorder.Eventf(&pvc, nil, corev1.EventTypeWarning, ReasonInvalidSource, "Populate", "%s", err.Error())
		_ = r.setAccepted(ctx, &src, false, "Invalid", err.Error())
		return ctrl.Result{}, r.updatePopulation(ctx, &src, &pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message = v1alpha1.PhaseFailed, err.Error()
		})
	}
	if err := r.setAccepted(ctx, &src, true, "Valid", "source accepted"); err != nil {
		return ctrl.Result{}, err
	}

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		msg := "the PVC must name a StorageClass with a CSI provisioner; static volumes cannot be populated"
		r.Recorder.Eventf(&pvc, nil, corev1.EventTypeWarning, ReasonUnsupported, "Populate", "%s", msg)
		return ctrl.Result{}, r.updatePopulation(ctx, &src, &pvc, func(p *v1alpha1.PopulationStatus) { p.Phase, p.Message = v1alpha1.PhaseFailed, msg })
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.updatePopulation(ctx, &src, &pvc, func(p *v1alpha1.PopulationStatus) {
				p.Phase, p.Message = v1alpha1.PhasePending, "waiting for StorageClass "+*pvc.Spec.StorageClassName
			})
		}
		return ctrl.Result{}, err
	}
	nodeName := ""
	if sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
		nodeName = pvc.Annotations[AnnSelectedNode]
		if nodeName == "" {
			return ctrl.Result{}, r.updatePopulation(ctx, &src, &pvc, func(p *v1alpha1.PopulationStatus) {
				p.Phase, p.Message = v1alpha1.PhaseWaitingForFirstConsumer, "waiting for a pod that uses the PVC to be scheduled"
			})
		}
	}

	if !controllerutil.ContainsFinalizer(&pvc, Finalizer) {
		patch := client.MergeFrom(pvc.DeepCopy())
		controllerutil.AddFinalizer(&pvc, Finalizer)
		if err := r.Patch(ctx, &pvc, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	prime, err := r.ensurePrime(ctx, &pvc, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}

	var job batchv1.Job
	err = r.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: JobName(uid)}, &job)
	if apierrors.IsNotFound(err) {
		j := BuildJob(&pvc, prime, &src, nodeName, r.Opts.Job)
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		logger.Info("created worker job", "job", j.Name)
		r.Recorder.Eventf(&pvc, nil, corev1.EventTypeNormal, ReasonStarted, "Populate", "started worker Job %s", j.Name)
		now := metav1.Now()
		return ctrl.Result{}, r.updatePopulation(ctx, &src, &pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message, p.JobName = v1alpha1.PhaseRunning, "worker started", j.Name
			if p.StartTime == nil {
				p.StartTime = &now
			}
		})
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if job.Labels[LabelPVCUID] != uid {
		return ctrl.Result{}, fmt.Errorf("job %s does not belong to PVC UID %s", job.Name, uid)
	}
	if job.DeletionTimestamp != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	switch {
	case jobCondition(&job, batchv1.JobComplete):
		return r.handleComplete(ctx, &pvc, prime, &src, &job)
	case jobCondition(&job, batchv1.JobFailed):
		msg := "worker Job failed"
		if t, _ := r.lastTermination(ctx, &job, corev1.PodFailed); t != nil && t.Error != "" {
			msg = t.Error
		}
		r.Recorder.Eventf(&pvc, nil, corev1.EventTypeWarning, ReasonFailed, "Populate", "population failed after %d attempt(s): %s", job.Status.Failed, msg)
		now := metav1.Now()
		return ctrl.Result{}, r.updatePopulation(ctx, &src, &pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message, p.FailedAttempts, p.CompletionTime = v1alpha1.PhaseFailed, msg, job.Status.Failed, &now
		})
	default:
		return r.handleRunning(ctx, &pvc, &src, &job)
	}
}

func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *Reconciler) ensurePrime(ctx context.Context, pvc *corev1.PersistentVolumeClaim, nodeName string) (*corev1.PersistentVolumeClaim, error) {
	var prime corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: pvc.Namespace, Name: PrimeName(string(pvc.UID))}
	err := r.Get(ctx, key, &prime)
	if err == nil {
		if prime.Labels[LabelPVCUID] != string(pvc.UID) {
			return nil, fmt.Errorf("prime PVC %s exists but belongs to another PVC", key.Name)
		}
		return &prime, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	p := BuildPrime(pvc, nodeName)
	if err := r.Create(ctx, p); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return p, r.Get(ctx, key, p)
		}
		return nil, err
	}
	return p, nil
}

// handleComplete verifies the worker's validation evidence and rebinds the
// prime volume to the user's PVC.
func (r *Reconciler) handleComplete(ctx context.Context, pvc, prime *corev1.PersistentVolumeClaim, src *v1alpha1.VSphereVolumeSource, job *batchv1.Job) (ctrl.Result, error) {
	t, err := r.lastTermination(ctx, job, corev1.PodSucceeded)
	if err != nil {
		return ctrl.Result{}, err
	}
	if msg := validateTermination(t, pvc); msg != "" {
		// A completed Job alone is not proof of a correct disk.
		r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, ReasonNoValidationData, "Populate", "%s", msg)
		now := metav1.Now()
		return ctrl.Result{}, r.updatePopulation(ctx, src, pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message, p.CompletionTime = v1alpha1.PhaseFailed, msg, &now
		})
	}
	result := &v1alpha1.PopulationResult{
		VirtualSize: t.VirtualSize, GuestDigest: t.GuestDigest, DestinationDigest: t.DestinationDigest,
		ExportBytes: t.ExportBytes, ExportSHA256: t.ExportSHA256, ManifestStatus: t.ManifestStatus, LeaseKey: t.LeaseKey,
		VMMoRef: t.VMMoRef, DiskKey: t.DiskKey, ServerVersion: t.ServerVersion, QemuImgVersion: t.QemuImgVersion,
		ExportMillis: int64(t.ExportSeconds * 1000), TotalMillis: int64(t.TotalSeconds * 1000),
	}

	if prime.Spec.VolumeName == "" {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: prime.Spec.VolumeName}, &pv); err != nil {
		return ctrl.Result{}, err
	}
	ref := pv.Spec.ClaimRef
	switch {
	case ref != nil && ref.UID == pvc.UID:
		// Already rebound; wait for the PV controller to bind the PVC.
		return ctrl.Result{}, r.updatePopulation(ctx, src, pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message, p.Result = v1alpha1.PhaseRebinding, "waiting for the PVC to bind to "+pv.Name, result
		})
	case ref != nil && ref.UID == prime.UID:
		r.Recorder.Eventf(pvc, nil, corev1.EventTypeNormal, ReasonValidated, "Populate",
			"destination digest %s matches the independently decoded export (%d bytes)", t.DestinationDigest, t.VirtualSize)
		patch := map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{
				AnnPopulatedFrom: pvc.Namespace + "/" + src.Name,
				AnnGuestDigest:   t.GuestDigest,
			}},
			"spec": map[string]any{"claimRef": map[string]any{
				"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"namespace": pvc.Namespace, "name": pvc.Name, "uid": string(pvc.UID), "resourceVersion": pvc.ResourceVersion,
			}},
		}
		b, _ := json.Marshal(patch)
		if err := r.Patch(ctx, &pv, client.RawPatch(types.StrategicMergePatchType, b)); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(pvc, nil, corev1.EventTypeNormal, ReasonRebound, "Populate", "rebound populated volume %s to this PVC", pv.Name)
		return ctrl.Result{}, r.updatePopulation(ctx, src, pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message, p.Result = v1alpha1.PhaseRebinding, "rebound "+pv.Name, result
		})
	default:
		return ctrl.Result{}, fmt.Errorf("volume %s is bound to an unexpected claim", pv.Name)
	}
}

// validateTermination returns a non-empty reason when the worker's result is
// not sufficient evidence of a correct population.
func validateTermination(t *worker.Termination, pvc *corev1.PersistentVolumeClaim) string {
	switch {
	case t == nil:
		return "worker Job completed but no worker result was found; not treating the volume as populated"
	case !t.Validated:
		return "worker Job completed but its result is not validated; not treating the volume as populated"
	case t.GuestDigest == "" || t.GuestDigest != t.DestinationDigest:
		return "worker result lacks matching source and destination digests; not treating the volume as populated"
	case t.VirtualSize <= 0:
		return "worker result has no virtual size"
	}
	req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if t.VirtualSize > req.Value() {
		return fmt.Sprintf("populated disk (%d bytes) is larger than the PVC request (%d bytes)", t.VirtualSize, req.Value())
	}
	return ""
}

// lastTermination returns the termination message of the most recent worker
// pod of the job in the given phase.
func (r *Reconciler) lastTermination(ctx context.Context, job *batchv1.Job, phase corev1.PodPhase) (*worker.Termination, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/controller-uid": string(job.UID)}); err != nil {
		return nil, err
	}
	var best *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != phase {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	if best == nil {
		return nil, nil
	}
	for _, cs := range best.Status.ContainerStatuses {
		if cs.Name == workerContainer && cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
			var t worker.Termination
			if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &t); err != nil {
				return nil, nil //nolint:nilerr // a malformed message is treated as missing validation evidence
			}
			return &t, nil
		}
	}
	return nil, nil
}

func (r *Reconciler) handleRunning(ctx context.Context, pvc *corev1.PersistentVolumeClaim, src *v1alpha1.VSphereVolumeSource, job *batchv1.Job) (ctrl.Result, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/controller-uid": string(job.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	msg := "worker pending"
	var prog *v1alpha1.PopulationProgress
	for i := range pods.Items {
		p := &pods.Items[i]
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				msg = fmt.Sprintf("worker pod %s waiting: %s: %s", p.Name, w.Reason, w.Message)
			}
		}
		if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			msg = "worker running"
			prog = r.pollProgress(ctx, p.Status.PodIP)
		}
	}
	poll := r.Opts.ProgressPollInterval
	if poll <= 0 {
		poll = 15 * time.Second
	}
	return ctrl.Result{RequeueAfter: poll}, r.updatePopulation(ctx, src, pvc, func(p *v1alpha1.PopulationStatus) {
		p.Phase, p.Message, p.JobName, p.FailedAttempts = v1alpha1.PhaseRunning, msg, job.Name, job.Status.Failed
		if prog != nil {
			p.Progress = prog
		}
	})
}

func (r *Reconciler) pollProgress(ctx context.Context, ip string) *v1alpha1.PopulationProgress {
	if r.Opts.HTTPClient == nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	u := "http://" + net.JoinHostPort(ip, strconv.Itoa(progressPort)) + "/progress"
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	resp, err := r.Opts.HTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var s progress.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&s); err != nil {
		return nil
	}
	now := metav1.Now()
	return &v1alpha1.PopulationProgress{
		WorkerPhase: s.Phase, StreamBytes: s.StreamBytes, GuestBytes: s.GuestBytes, VirtualSize: s.VirtualSize,
		TransferPercent: int32(s.TransferPercent), ConvertPercent: int32(s.ConvertPercent), VerifiedBytes: s.VerifiedBytes,
		BytesPerSecond: int64(s.ThroughputBytesPerSecond), UpdateTime: &now,
	}
}

// finish removes the prime PVC and worker Job (once safe) and the finalizer,
// and records the final phase.
func (r *Reconciler) finish(ctx context.Context, pvc *corev1.PersistentVolumeClaim, srcName, phase, msg string, result *v1alpha1.PopulationResult) (ctrl.Result, error) {
	uid := string(pvc.UID)
	var prime corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: PrimeName(uid)}, &prime)
	if err == nil {
		if phase == v1alpha1.PhaseSucceeded && prime.Status.Phase == corev1.ClaimBound {
			// The PV controller has not yet noticed the rebind.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if prime.DeletionTimestamp == nil {
			if err := r.Delete(ctx, &prime); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	var job batchv1.Job
	err = r.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: JobName(uid)}, &job)
	if err == nil && job.DeletionTimestamp == nil {
		if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	var src v1alpha1.VSphereVolumeSource
	if err := r.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: srcName}, &src); err == nil {
		now := metav1.Now()
		if phase == v1alpha1.PhaseCancelled {
			r.Recorder.Eventf(pvc, nil, corev1.EventTypeNormal, ReasonCancelled, "Populate", "%s", msg)
		}
		if err := r.updatePopulation(ctx, &src, pvc, func(p *v1alpha1.PopulationStatus) {
			p.Phase, p.Message, p.Progress = phase, msg, nil
			if result != nil {
				p.Result = result
			}
			if p.CompletionTime == nil {
				p.CompletionTime = &now
			}
		}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if controllerutil.ContainsFinalizer(pvc, Finalizer) {
		patch := client.MergeFrom(pvc.DeepCopy())
		controllerutil.RemoveFinalizer(pvc, Finalizer)
		if err := r.Patch(ctx, pvc, patch); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// updatePopulation applies mutate to the status entry of pvc, retrying on conflicts.
func (r *Reconciler) updatePopulation(ctx context.Context, src *v1alpha1.VSphereVolumeSource, pvc *corev1.PersistentVolumeClaim, mutate func(*v1alpha1.PopulationStatus)) error {
	key := client.ObjectKeyFromObject(src)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.VSphereVolumeSource
		if err := r.Get(ctx, key, &cur); err != nil {
			return err
		}
		before := cur.Status.DeepCopy()
		idx := -1
		for i := range cur.Status.Populations {
			if cur.Status.Populations[i].PVCUID == pvc.UID {
				idx = i
			}
		}
		if idx < 0 {
			cur.Status.Populations = append(cur.Status.Populations, v1alpha1.PopulationStatus{PVCName: pvc.Name, PVCUID: pvc.UID, Phase: v1alpha1.PhasePending})
			idx = len(cur.Status.Populations) - 1
		}
		entry := &cur.Status.Populations[idx]
		prevPhase := entry.Phase
		mutate(entry)
		if entry.Phase != prevPhase && entry.Phase == v1alpha1.PhaseRunning {
			entry.CompletionTime = nil
		}
		summarize(&cur)
		cur.Status.ObservedGeneration = cur.Generation
		if equalStatus(before, &cur.Status) {
			return nil
		}
		return r.Status().Update(ctx, &cur)
	})
}

func equalStatus(a, b *v1alpha1.VSphereVolumeSourceStatus) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// summarize derives the Populating and Failed conditions from the entries.
func summarize(src *v1alpha1.VSphereVolumeSource) {
	running, failed := 0, 0
	var failMsg string
	for _, p := range src.Status.Populations {
		switch p.Phase {
		case v1alpha1.PhaseRunning, v1alpha1.PhaseRebinding, v1alpha1.PhasePending, v1alpha1.PhaseWaitingForFirstConsumer:
			running++
		case v1alpha1.PhaseFailed:
			failed++
			failMsg = p.PVCName + ": " + p.Message
		}
	}
	setCond := func(t string, ok bool, reason, msg string) {
		st := metav1.ConditionFalse
		if ok {
			st = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&src.Status.Conditions, metav1.Condition{Type: t, Status: st, Reason: reason, Message: truncate(msg, 1024), ObservedGeneration: src.Generation})
	}
	setCond(v1alpha1.ConditionPopulating, running > 0, "Populations", fmt.Sprintf("%d population(s) in progress", running))
	if failed > 0 {
		setCond(v1alpha1.ConditionFailed, true, "PopulationFailed", failMsg)
	} else {
		setCond(v1alpha1.ConditionFailed, false, "NoFailures", "no failed populations")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (r *Reconciler) setAccepted(ctx context.Context, src *v1alpha1.VSphereVolumeSource, ok bool, reason, msg string) error {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	if c := meta.FindStatusCondition(src.Status.Conditions, v1alpha1.ConditionAccepted); c != nil &&
		c.Status == st && c.Reason == reason && c.ObservedGeneration == src.Generation {
		return nil
	}
	key := client.ObjectKeyFromObject(src)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.VSphereVolumeSource
		if err := r.Get(ctx, key, &cur); err != nil {
			return err
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionAccepted, Status: st, Reason: reason,
			Message: truncate(msg, 1024), ObservedGeneration: cur.Generation})
		if err := r.Status().Update(ctx, &cur); err != nil {
			return err
		}
		src.Status = cur.Status
		return nil
	})
}
