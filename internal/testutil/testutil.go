// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// RequireQemuImg returns the path of qemu-img. The test is skipped when
// qemu-img is not installed, unless KVSP_REQUIRE_QEMU_IMG=1 (set in CI), in
// which case it fails.
func RequireQemuImg(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("qemu-img")
	if err != nil {
		if os.Getenv("KVSP_REQUIRE_QEMU_IMG") == "1" {
			t.Fatalf("qemu-img is required but not found: %v", err)
		}
		t.Skip("qemu-img not installed")
	}
	return p
}

// RepoRoot returns the repository root directory.
func RepoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine source location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Fixture returns the path of an externally fetched fixture (see
// hack/fetch-fixtures.sh). The test is skipped when it is missing unless
// KVSP_REQUIRE_FIXTURES=1.
func Fixture(t testing.TB, name string) string {
	t.Helper()
	p := filepath.Join(RepoRoot(t), "testdata", "external", name)
	if _, err := os.Stat(p); err != nil {
		if os.Getenv("KVSP_REQUIRE_FIXTURES") == "1" {
			t.Fatalf("fixture %s is required but missing; run hack/fetch-fixtures.sh", name)
		}
		t.Skipf("fixture %s missing; run hack/fetch-fixtures.sh", name)
	}
	return p
}

// RequireEnvtest skips unless envtest assets are configured (the controller
// test binary already exits early without them; this documents the dependency).
func RequireEnvtest(t testing.TB) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
}
