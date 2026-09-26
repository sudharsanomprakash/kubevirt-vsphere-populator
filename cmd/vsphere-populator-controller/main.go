// Command vsphere-populator-controller runs the Kubernetes volume populator
// for VSphereVolumeSource.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/sudharsanomprakash/kubevirt-vsphere-populator/api/v1alpha1"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/controller"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/version"
)

func main() {
	var (
		workerImage, pullPolicy, scratchClass, allowedServers, metricsAddr, probeAddr, leaderNS, pullSecrets string
		allowInsecure, leaderElect, showVersion                                                              bool
		fileUID, blockUID                                                                                    int64
		poll                                                                                                 time.Duration
		maxConcurrent                                                                                        int
	)
	flag.StringVar(&workerImage, "worker-image", "", "worker container image (required)")
	flag.StringVar(&pullPolicy, "worker-image-pull-policy", string(corev1.PullIfNotPresent), "worker image pull policy")
	flag.StringVar(&pullSecrets, "worker-image-pull-secrets", "", "comma-separated image pull secret names (must exist in each target namespace)")
	flag.StringVar(&scratchClass, "scratch-storage-class", "", "default StorageClass for worker scratch volumes (empty: emptyDir)")
	flag.StringVar(&allowedServers, "allowed-server-pattern", "", "regular expression that spec.server must fully match (empty: any server)")
	flag.BoolVar(&allowInsecure, "allow-insecure-tls-for-testing", false, "permit tls.insecureSkipVerifyForTestingOnly (test environments only)")
	flag.Int64Var(&fileUID, "worker-file-run-as-user", 107, "UID for filesystem-mode workers")
	flag.Int64Var(&blockUID, "worker-block-run-as-user", 0, "UID for block-mode workers")
	flag.DurationVar(&poll, "progress-poll-interval", 15*time.Second, "how often running workers are polled for progress")
	flag.IntVar(&maxConcurrent, "max-concurrent-reconciles", 4, "maximum PVCs reconciled in parallel")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint address (0 disables)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leaderElect, "leader-elect", true, "enable leader election")
	flag.StringVar(&leaderNS, "leader-election-namespace", "", "namespace for the leader election lease (default: in-cluster namespace)")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if showVersion {
		fmt.Println(version.String())
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	if workerImage == "" {
		setupLog.Error(nil, "--worker-image is required")
		os.Exit(2)
	}
	var allowed func(string) bool
	if allowedServers != "" {
		re, err := regexp.Compile("^(?:" + allowedServers + ")$")
		if err != nil {
			setupLog.Error(err, "invalid --allowed-server-pattern")
			os.Exit(2)
		}
		allowed = re.MatchString
	} else {
		setupLog.Info("no --allowed-server-pattern set: sources may name any vSphere server; see docs/security.md")
	}
	if allowInsecure {
		setupLog.Info("WARNING: insecure TLS is permitted for sources that request it; never use this outside test environments")
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	managed := labels.SelectorFromSet(labels.Set{controller.LabelManagedBy: controller.ManagedByValue})
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "vsphere-populator." + v1alpha1.Group,
		LeaderElectionNamespace: leaderNS,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			// Only worker Jobs and pods are cached.
			&batchv1.Job{}: {Label: managed},
			&corev1.Pod{}:  {Label: managed},
		}},
	})
	if err != nil {
		setupLog.Error(err, "create manager")
		os.Exit(1)
	}
	var secrets []corev1.LocalObjectReference
	for _, s := range strings.Split(pullSecrets, ",") {
		if s = strings.TrimSpace(s); s != "" {
			secrets = append(secrets, corev1.LocalObjectReference{Name: s})
		}
	}
	r := &controller.Reconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder("vsphere-populator"),
		Opts: controller.Options{
			Job: controller.JobOptions{Image: workerImage, ImagePullPolicy: corev1.PullPolicy(pullPolicy),
				FileRunAsUser: fileUID, BlockRunAsUser: blockUID, DefaultScratchStorageClass: scratchClass, ImagePullSecrets: secrets},
			AllowInsecureTLS:     allowInsecure,
			AllowedServer:        allowed,
			ProgressPollInterval: poll,
			HTTPClient:           &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}},
		},
	}
	if err := r.SetupWithManager(mgr, maxConcurrent); err != nil {
		setupLog.Error(err, "set up controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	setupLog.Info("starting", "version", version.String(), "workerImage", workerImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
