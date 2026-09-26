package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/sudharsanomprakash/kubevirt-vsphere-populator/api/v1alpha1"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/controller"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/testutil"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/worker"
)

var (
	cfg    *rest.Config
	scheme = runtime.NewScheme()
	k      client.Client
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS not set; skipping controller tests (run `make test`)")
		if os.Getenv("KVSP_REQUIRE_ENVTEST") == "1" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.Level(zapcore.Level(zapLevel()))))
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "crd")}, ErrorIfCRDPathMissing: true}
	var err error
	cfg, err = env.Start()
	if err != nil {
		fmt.Println("envtest:", err)
		os.Exit(1)
	}
	k, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func zapLevel() int8 {
	if os.Getenv("KVSP_VERBOSE") == "1" {
		return -2
	}
	return 2
}

type manager struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startManager(t *testing.T, opts controller.Options) *manager {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: configController()})
	if err != nil {
		t.Fatal(err)
	}
	r := &controller.Reconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("vsphere-populator"), Opts: opts}
	if err := r.SetupWithManager(mgr, 4); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &manager{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	return m
}

func (m *manager) stop() {
	m.cancel()
	<-m.done
}

func defaultOpts() controller.Options {
	return controller.Options{Job: controller.JobOptions{Image: "example.test/worker:dev", FileRunAsUser: 107, BlockRunAsUser: 0}, ProgressPollInterval: time.Second}
}

var nsCounter int
var nsMu sync.Mutex

func namespace(t *testing.T) string {
	t.Helper()
	nsMu.Lock()
	nsCounter++
	name := fmt.Sprintf("t%d-%s", nsCounter, strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")))
	nsMu.Unlock()
	if len(name) > 60 {
		name = name[:60]
	}
	name = strings.TrimRight(name, "-")
	if err := k.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

func storageClass(t *testing.T, name string, wffc bool) {
	t.Helper()
	mode := storagev1.VolumeBindingImmediate
	if wffc {
		mode = storagev1.VolumeBindingWaitForFirstConsumer
	}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "hostpath.csi.example.test", VolumeBindingMode: &mode}
	if err := k.Create(context.Background(), sc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func source(t *testing.T, ns string, mutate func(*v1alpha1.VSphereVolumeSource)) *v1alpha1.VSphereVolumeSource {
	t.Helper()
	s := &v1alpha1.VSphereVolumeSource{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-01-disk0"},
		Spec: v1alpha1.VSphereVolumeSourceSpec{
			Server:               "vcenter.example.test",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "vsphere-credentials"},
			VirtualMachine:       v1alpha1.VMSelector{InstanceUUID: "5012a6c4-8d2b-4f5e-9c1a-0123456789ab"},
			Disk:                 v1alpha1.DiskSelector{BusLocation: "scsi:0:0"},
		},
	}
	if mutate != nil {
		mutate(s)
	}
	if err := k.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func pvc(t *testing.T, ns, sc string, block bool) *corev1.PersistentVolumeClaim {
	t.Helper()
	mode := corev1.PersistentVolumeFilesystem
	if block {
		mode = corev1.PersistentVolumeBlock
	}
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-01-root"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(sc),
			VolumeMode:       &mode,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
			DataSourceRef:    &corev1.TypedObjectReference{APIGroup: ptr.To(v1alpha1.Group), Kind: v1alpha1.Kind, Name: "web-01-disk0"},
		},
	}
	if err := k.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func consistently(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 15; i++ {
		if !f() {
			t.Fatalf("condition violated: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func get(obj client.Object, ns, name string) error {
	return k.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
}

func population(t *testing.T, ns string) *v1alpha1.PopulationStatus {
	var s v1alpha1.VSphereVolumeSource
	if err := get(&s, ns, "web-01-disk0"); err != nil {
		return nil
	}
	if len(s.Status.Populations) == 0 {
		return nil
	}
	return &s.Status.Populations[0]
}

// provision simulates the CSI provisioner and PV controller binding the prime PVC.
func provision(t *testing.T, prime *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	t.Helper()
	ctx := context.Background()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + string(prime.UID)},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              *prime.Spec.StorageClassName,
			VolumeMode:                    prime.Spec.VolumeMode,
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "hostpath.csi.example.test", VolumeHandle: string(prime.UID)}},
			ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: prime.Namespace, Name: prime.Name, UID: prime.UID},
		},
	}
	if err := k.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	prime.Spec.VolumeName = pv.Name
	if err := k.Update(ctx, prime); err != nil {
		t.Fatal(err)
	}
	prime.Status.Phase = corev1.ClaimBound
	if err := k.Status().Update(ctx, prime); err != nil {
		t.Fatal(err)
	}
	return pv
}

func termination(validated bool, digest string) string {
	t := worker.Termination{Validated: validated, VirtualSize: 8 << 30, GuestDigest: "kvsp-chunked-sha256-v1:" + digest,
		DestinationDigest: "kvsp-chunked-sha256-v1:" + digest, ExportBytes: 123, ManifestStatus: "verified", LeaseKey: "/vm-42/VirtualLsiLogicController0:0"}
	if !validated {
		t.Error = "export stream: vmdk format error: stream truncated"
	}
	b, _ := json.Marshal(t)
	return string(b)
}

// finishJob simulates the Job controller: it creates a terminated worker pod
// and marks the Job complete or failed.
func finishJob(t *testing.T, job *batchv1.Job, succeeded bool, message string) {
	t.Helper()
	ctx := context.Background()
	phase := corev1.PodSucceeded
	exit := int32(0)
	if !succeeded {
		phase, exit = corev1.PodFailed, 1
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace, GenerateName: job.Name + "-",
			Labels:      map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID), controller.LabelManagedBy: controller.ManagedByValue},
			Annotations: map[string]string{controller.AnnPVCName: job.Annotations[controller.AnnPVCName]},
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
	if err := k.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "worker", Image: job.Spec.Template.Spec.Containers[0].Image,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: message}},
	}}}
	if err := k.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := get(job, job.Namespace, job.Name); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	job.Status.StartTime = &now
	if succeeded {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
	} else {
		job.Status.Failed = 3
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "BackoffLimitExceeded"},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "BackoffLimitExceeded"},
		}
	}
	if err := k.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
}

func waitJob(t *testing.T, ns string, p *corev1.PersistentVolumeClaim) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	eventually(t, "worker job", func() bool { return get(&job, ns, controller.JobName(string(p.UID))) == nil })
	return &job
}

func waitPrime(t *testing.T, ns string, p *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	t.Helper()
	var prime corev1.PersistentVolumeClaim
	eventually(t, "prime pvc", func() bool { return get(&prime, ns, controller.PrimeName(string(p.UID))) == nil })
	return &prime
}

// bindTarget simulates the PV controller binding the target PVC after the rebind.
func bindTarget(t *testing.T, target *corev1.PersistentVolumeClaim, prime *corev1.PersistentVolumeClaim, pvName string) {
	t.Helper()
	ctx := context.Background()
	eventually(t, "target update", func() bool {
		if err := get(target, target.Namespace, target.Name); err != nil {
			return false
		}
		target.Spec.VolumeName = pvName
		return k.Update(ctx, target) == nil
	})
	target.Status.Phase = corev1.ClaimBound
	if err := k.Status().Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := get(prime, prime.Namespace, prime.Name); err == nil {
		prime.Status.Phase = corev1.ClaimLost
		_ = k.Status().Update(ctx, prime)
	}
}

func TestFilesystemPopulationLifecycle(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)

	prime := waitPrime(t, ns, target)
	if prime.Spec.StorageClassName == nil || *prime.Spec.StorageClassName != "csi-immediate" || prime.Spec.DataSourceRef != nil ||
		prime.OwnerReferences[0].UID != target.UID || prime.Spec.Resources.Requests.Storage().String() != "10Gi" {
		t.Fatalf("prime spec %+v", prime.Spec)
	}
	job := waitJob(t, ns, target)
	eventually(t, "finalizer", func() bool {
		_ = get(target, ns, target.Name)
		return strings.Contains(strings.Join(target.Finalizers, ","), controller.Finalizer)
	})
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	if *job.Spec.Parallelism != 1 || *job.Spec.Completions != 1 || *job.Spec.PodReplacementPolicy != batchv1.Failed || *job.Spec.BackoffLimit != 2 {
		t.Fatalf("job spec %+v", job.Spec)
	}
	if len(c.VolumeDevices) != 0 || !hasMount(c, "/target") || *pod.SecurityContext.RunAsUser != 107 || *pod.SecurityContext.FSGroup != 107 {
		t.Fatalf("filesystem worker misconfigured: %+v %+v", c, pod.SecurityContext)
	}
	if *pod.AutomountServiceAccountToken || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("worker pod not locked down")
	}
	if !strings.Contains(strings.Join(c.Args, " "), "--target-mode=file") || len(c.Env) != 1 {
		t.Fatalf("args %v env %v", c.Args, c.Env)
	}
	eventually(t, "running status", func() bool { p := population(t, ns); return p != nil && p.Phase == v1alpha1.PhaseRunning })

	pv := provision(t, prime)
	finishJob(t, job, true, termination(true, strings.Repeat("ab", 32)))

	eventually(t, "pv rebind", func() bool {
		_ = get(pv, "", pv.Name)
		return pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.UID == target.UID
	})
	if pv.Annotations[controller.AnnPopulatedFrom] != ns+"/web-01-disk0" || !strings.HasSuffix(pv.Annotations[controller.AnnGuestDigest], strings.Repeat("ab", 32)) {
		t.Fatalf("pv annotations %v", pv.Annotations)
	}
	eventually(t, "rebinding status", func() bool {
		p := population(t, ns)
		return p != nil && p.Phase == v1alpha1.PhaseRebinding && p.Result != nil && p.Result.ManifestStatus == "verified"
	})
	bindTarget(t, target, prime, pv.Name)

	eventually(t, "success", func() bool { p := population(t, ns); return p != nil && p.Phase == v1alpha1.PhaseSucceeded })
	eventually(t, "cleanup", func() bool {
		_ = get(target, ns, target.Name)
		var j batchv1.Job
		jobGone := apierrors.IsNotFound(get(&j, ns, job.Name)) || j.DeletionTimestamp != nil
		var pr corev1.PersistentVolumeClaim
		primeGone := apierrors.IsNotFound(get(&pr, ns, prime.Name)) || pr.DeletionTimestamp != nil
		return jobGone && primeGone && !strings.Contains(strings.Join(target.Finalizers, ","), controller.Finalizer)
	})
}

func hasMount(c corev1.Container, path string) bool {
	for _, m := range c.VolumeMounts {
		if m.MountPath == path {
			return true
		}
	}
	return false
}

func TestBlockModeJob(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, func(s *v1alpha1.VSphereVolumeSource) {
		s.Spec.Worker = &v1alpha1.WorkerSpec{BackoffLimit: ptr.To(int32(5)), Scratch: &v1alpha1.ScratchSpec{StorageClassName: ptr.To("scratch-sc")}}
	})
	target := pvc(t, ns, "csi-immediate", true)
	job := waitJob(t, ns, target)
	c := job.Spec.Template.Spec.Containers[0]
	if len(c.VolumeDevices) != 1 || c.VolumeDevices[0].DevicePath != "/dev/vsphere-populator-target" || hasMount(c, "/target") {
		t.Fatalf("block worker misconfigured: %+v", c)
	}
	if !strings.Contains(strings.Join(c.Args, " "), "--target-mode=block") || *job.Spec.BackoffLimit != 5 {
		t.Fatalf("args %v", c.Args)
	}
	var scratch *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "scratch" {
			scratch = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if scratch == nil || scratch.Ephemeral == nil || *scratch.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName != "scratch-sc" {
		t.Fatalf("scratch volume %+v", scratch)
	}
	if job.Spec.Template.Spec.SecurityContext.FSGroup != nil {
		t.Fatal("fsGroup must not be set for block volumes")
	}
}

func TestWaitForFirstConsumer(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-wffc", true)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-wffc", false)
	eventually(t, "waiting status", func() bool {
		p := population(t, ns)
		return p != nil && p.Phase == v1alpha1.PhaseWaitingForFirstConsumer
	})
	consistently(t, "no prime or job before node selection", func() bool {
		var pr corev1.PersistentVolumeClaim
		var j batchv1.Job
		return apierrors.IsNotFound(get(&pr, ns, controller.PrimeName(string(target.UID)))) && apierrors.IsNotFound(get(&j, ns, controller.JobName(string(target.UID))))
	})
	// The scheduler selects a node for a consumer pod.
	_ = get(target, ns, target.Name)
	target.Annotations = map[string]string{controller.AnnSelectedNode: "worker-node-2"}
	if err := k.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	prime := waitPrime(t, ns, target)
	if prime.Annotations[controller.AnnSelectedNode] != "worker-node-2" {
		t.Fatalf("prime annotations %v", prime.Annotations)
	}
	job := waitJob(t, ns, target)
	terms := job.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if terms[0].MatchFields[0].Values[0] != "worker-node-2" {
		t.Fatalf("affinity %+v", terms)
	}
}

func TestCompletedJobWithoutValidationIsNotTrusted(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)
	prime := waitPrime(t, ns, target)
	job := waitJob(t, ns, target)
	pv := provision(t, prime)
	// Exit code 0 but no validation evidence.
	finishJob(t, job, true, `{"validated":false}`)
	eventually(t, "failed status", func() bool {
		p := population(t, ns)
		return p != nil && p.Phase == v1alpha1.PhaseFailed && strings.Contains(p.Message, "not validated")
	})
	consistently(t, "pv is not rebound", func() bool {
		_ = get(pv, "", pv.Name)
		return pv.Spec.ClaimRef.UID == prime.UID
	})
	var s v1alpha1.VSphereVolumeSource
	_ = get(&s, ns, "web-01-disk0")
	if !conditionTrue(s.Status.Conditions, v1alpha1.ConditionFailed) {
		t.Fatalf("conditions %+v", s.Status.Conditions)
	}
}

func TestMismatchedDigestsAreNotTrusted(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)
	prime := waitPrime(t, ns, target)
	job := waitJob(t, ns, target)
	pv := provision(t, prime)
	msg := `{"validated":true,"virtualSize":1024,"guestDigest":"kvsp-chunked-sha256-v1:aa","destinationDigest":"kvsp-chunked-sha256-v1:bb"}`
	finishJob(t, job, true, msg)
	eventually(t, "failed status", func() bool {
		p := population(t, ns)
		return p != nil && p.Phase == v1alpha1.PhaseFailed && strings.Contains(p.Message, "digests")
	})
	consistently(t, "pv is not rebound", func() bool { _ = get(pv, "", pv.Name); return pv.Spec.ClaimRef.UID == prime.UID })
}

func conditionTrue(cs []metav1.Condition, typ string) bool {
	for _, c := range cs {
		if c.Type == typ {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func TestFailedJobReportsWorkerError(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)
	job := waitJob(t, ns, target)
	finishJob(t, job, false, termination(false, ""))
	eventually(t, "failed status", func() bool {
		p := population(t, ns)
		return p != nil && p.Phase == v1alpha1.PhaseFailed && strings.Contains(p.Message, "stream truncated") && p.FailedAttempts == 3
	})
}

func TestInsecureTLSRequiresControllerPermission(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, func(s *v1alpha1.VSphereVolumeSource) {
		s.Spec.TLS = &v1alpha1.TLSSpec{InsecureSkipVerifyForTestingOnly: true}
	})
	target := pvc(t, ns, "csi-immediate", false)
	eventually(t, "rejected", func() bool {
		var s v1alpha1.VSphereVolumeSource
		_ = get(&s, ns, "web-01-disk0")
		for _, c := range s.Status.Conditions {
			if c.Type == v1alpha1.ConditionAccepted && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, "insecure") {
				return true
			}
		}
		return false
	})
	consistently(t, "no job", func() bool {
		var j batchv1.Job
		return apierrors.IsNotFound(get(&j, ns, controller.JobName(string(target.UID))))
	})
}

func TestAllowedServerPattern(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	opts := defaultOpts()
	opts.AllowedServer = func(s string) bool { return strings.HasSuffix(s, ".corp.example.test") }
	m := startManager(t, opts)
	defer m.stop()
	source(t, ns, nil) // vcenter.example.test is not allowed
	target := pvc(t, ns, "csi-immediate", false)
	eventually(t, "rejected", func() bool { p := population(t, ns); return p != nil && p.Phase == v1alpha1.PhaseFailed })
	var j batchv1.Job
	if !apierrors.IsNotFound(get(&j, ns, controller.JobName(string(target.UID)))) {
		t.Fatal("job created for a disallowed server")
	}
}

func TestSourceCreatedLater(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	target := pvc(t, ns, "csi-immediate", false)
	consistently(t, "no job without source", func() bool {
		var j batchv1.Job
		return apierrors.IsNotFound(get(&j, ns, controller.JobName(string(target.UID))))
	})
	source(t, ns, nil)
	waitJob(t, ns, target)
}

func TestPVCDeletionCancels(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	defer m.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)
	prime := waitPrime(t, ns, target)
	job := waitJob(t, ns, target)
	eventually(t, "finalizer", func() bool {
		_ = get(target, ns, target.Name)
		return len(target.Finalizers) > 0
	})
	if err := k.Delete(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cleanup after deletion", func() bool {
		var j batchv1.Job
		var pr corev1.PersistentVolumeClaim
		jobGone := apierrors.IsNotFound(get(&j, ns, job.Name)) || j.DeletionTimestamp != nil
		primeGone := apierrors.IsNotFound(get(&pr, ns, prime.Name)) || pr.DeletionTimestamp != nil
		var p corev1.PersistentVolumeClaim
		err := get(&p, ns, target.Name)
		targetReleased := apierrors.IsNotFound(err) || !strings.Contains(strings.Join(p.Finalizers, ","), controller.Finalizer)
		return jobGone && primeGone && targetReleased
	})
	eventually(t, "cancelled status", func() bool { p := population(t, ns); return p != nil && p.Phase == v1alpha1.PhaseCancelled })
}

// TestControllerRestart stops the controller after the worker Job exists and
// finishes the population with a new controller instance.
func TestControllerRestart(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m := startManager(t, defaultOpts())
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)
	prime := waitPrime(t, ns, target)
	job := waitJob(t, ns, target)
	m.stop()

	pv := provision(t, prime)
	finishJob(t, job, true, termination(true, strings.Repeat("cd", 32)))

	m2 := startManager(t, defaultOpts())
	defer m2.stop()
	eventually(t, "rebind after restart", func() bool {
		_ = get(pv, "", pv.Name)
		return pv.Spec.ClaimRef.UID == target.UID
	})
	bindTarget(t, target, prime, pv.Name)
	eventually(t, "success after restart", func() bool { p := population(t, ns); return p != nil && p.Phase == v1alpha1.PhaseSucceeded })
	var jobs batchv1.JobList
	_ = k.List(context.Background(), &jobs, client.InNamespace(ns))
	for _, j := range jobs.Items {
		if j.Name != job.Name {
			t.Fatalf("unexpected extra job %s", j.Name)
		}
	}
}

// TestConcurrentReconcilers runs two controller instances without leader
// election plus direct concurrent Reconcile calls against the same PVC; the
// deterministic names ensure a single prime PVC and a single Job.
func TestConcurrentReconcilers(t *testing.T) {
	testutil.RequireEnvtest(t)
	ns := namespace(t)
	storageClass(t, "csi-immediate", false)
	m1 := startManager(t, defaultOpts())
	defer m1.stop()
	m2 := startManager(t, defaultOpts())
	defer m2.stop()
	source(t, ns, nil)
	target := pvc(t, ns, "csi-immediate", false)

	direct := &controller.Reconciler{Client: k, Recorder: noopRecorder{}, Opts: defaultOpts()}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = direct.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: target.Name}})
		}()
	}
	wg.Wait()
	waitJob(t, ns, target)
	consistently(t, "exactly one job and one prime", func() bool {
		var jobs batchv1.JobList
		var pvcs corev1.PersistentVolumeClaimList
		_ = k.List(context.Background(), &jobs, client.InNamespace(ns))
		_ = k.List(context.Background(), &pvcs, client.InNamespace(ns), client.MatchingLabels{controller.LabelComponent: "prime"})
		return len(jobs.Items) == 1 && len(pvcs.Items) == 1
	})
}
