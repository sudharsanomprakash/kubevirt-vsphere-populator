package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/sudharsanomprakash/kubevirt-vsphere-populator/api/v1alpha1"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/worker"
)

func testSource() *v1alpha1.VSphereVolumeSource {
	return &v1alpha1.VSphereVolumeSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns"},
		Spec: v1alpha1.VSphereVolumeSourceSpec{
			Server:               "vcenter.example.test",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
			VirtualMachine:       v1alpha1.VMSelector{MoRef: "vm-42", Datacenter: "dc1"},
			Disk:                 v1alpha1.DiskSelector{DeviceKey: ptr.To(int32(2001)), ExpectedCapacityBytes: ptr.To(int64(1 << 30))},
			TLS:                  &v1alpha1.TLSSpec{Thumbprint: "AB:CD", TrustLeaseThumbprints: ptr.To(false)},
			Transfer:             &v1alpha1.TransferSpec{RequireManifest: true, IdleTimeout: &metav1.Duration{Duration: 90e9}},
		},
	}
}

func TestWorkerArgs(t *testing.T) {
	got := strings.Join(workerArgs(testSource(), false), " ")
	for _, want := range []string{"populate", "--server=vcenter.example.test", "--vm-moref=vm-42", "--datacenter=dc1", "--disk-key=2001",
		"--expected-capacity-bytes=1073741824", "--thumbprint=AB:CD", "--trust-lease-thumbprints=false", "--require-manifest",
		"--idle-timeout=1m30s", "--target-mode=file", "--credentials-dir=" + credentialsMount} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "insecure") || strings.Contains(strings.ToLower(got), "password") {
		t.Fatalf("unexpected args: %s", got)
	}
}

func TestScratchSize(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}}}}
	q := scratchSize(testSource(), pvc)
	want := int64(100<<30) + int64(100<<30)/50 + 256<<20
	if q.Value() != want {
		t.Fatalf("scratch %d want %d", q.Value(), want)
	}
}

func TestValidateSource(t *testing.T) {
	s := testSource()
	if err := ValidateSource(s, false, nil); err != nil {
		t.Fatal(err)
	}
	s.Spec.TLS.InsecureSkipVerifyForTestingOnly = true
	if err := ValidateSource(s, false, nil); err == nil {
		t.Fatal("insecure accepted without permission")
	}
	if err := ValidateSource(s, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSource(testSource(), false, func(string) bool { return false }); err == nil {
		t.Fatal("disallowed server accepted")
	}
}

func TestValidateTermination(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	ok := &worker.Termination{Validated: true, VirtualSize: 1 << 30, GuestDigest: "d", DestinationDigest: "d"}
	if msg := validateTermination(ok, pvc); msg != "" {
		t.Fatal(msg)
	}
	bad := []*worker.Termination{
		nil,
		{Validated: false, VirtualSize: 1, GuestDigest: "d", DestinationDigest: "d"},
		{Validated: true, VirtualSize: 1, GuestDigest: "d", DestinationDigest: "e"},
		{Validated: true, VirtualSize: 1},
		{Validated: true, VirtualSize: 2 << 30, GuestDigest: "d", DestinationDigest: "d"},
	}
	for i, b := range bad {
		if validateTermination(b, pvc) == "" {
			t.Errorf("case %d accepted", i)
		}
	}
}
