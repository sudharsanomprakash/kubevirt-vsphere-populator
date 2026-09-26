#!/usr/bin/env bash
# End-to-end test: a disposable kind cluster with a CSI hostpath driver, the
# populator controller and worker images built from this checkout, and the
# fake vSphere test double (NOT a real vCenter/ESXi). It populates
#   - a Filesystem PVC (Immediate binding) from a bootable Ubuntu 24.04 disk
#   - a Block PVC (WaitForFirstConsumer) from a synthetic odd-sized disk
# and compares each destination with the source raw image using sha256sum,
# independently of the populator's own digests. Optionally boots the
# migrated Ubuntu disk with QEMU (test/e2e/boot-check.sh).
#
# Requirements: docker, go, git, qemu-img, curl; kind and kubectl are
# downloaded (pinned) if missing. Set BOOT_CHECK=0 to skip the boot, KEEP=1
# to keep the cluster, and EXTRA_DOCKER_BUILD_ARGS for sandboxed builders.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=${E2E_WORK:-$ROOT/hack/e2e/work}
CLUSTER=${CLUSTER:-kvsp-e2e}
KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.0
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-kindest/node:v1.37.0}
HOSTPATH_TAG=v1.18.0
PROVISIONER_TAG=v6.3.0
REGISTRAR_TAG=v2.18.0
UBUNTU_IMG_URL=https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img
UBUNTU_IMG_SHA256=${UBUNTU_IMG_SHA256:-46b0dbaffa6950a7da5ff2dc5ed34c46084610b3b6d1fae8f1ec2d7e953984a3}
BLOCK_DISK_SIZE=$((300 * 1024 * 1024 + 7 * 512))
BOOT_CHECK=${BOOT_CHECK:-1}
NS=vm-migration-e2e

mkdir -p "$WORK/bin" "$WORK/data" "$WORK/logs"
REPORT="$WORK/e2e-report.txt"
: > "$REPORT"
log() { echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$REPORT"; }
export PATH="$WORK/bin:$PATH"

if ! command -v kind >/dev/null; then
  curl -fsSL -o "$WORK/bin/kind" "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-amd64"
  chmod +x "$WORK/bin/kind"
fi
if ! command -v kubectl >/dev/null; then
  curl -fsSL -o "$WORK/bin/kubectl" "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl"
  chmod +x "$WORK/bin/kubectl"
fi

log "== source disks"
if [[ ! -f "$WORK/data/ubuntu-source.raw" ]]; then
  curl -fsSL -o "$WORK/data/ubuntu.qcow2" "$UBUNTU_IMG_URL"
  echo "$UBUNTU_IMG_SHA256  $WORK/data/ubuntu.qcow2" | sha256sum -c -
  qemu-img convert -f qcow2 -O raw "$WORK/data/ubuntu.qcow2" "$WORK/data/ubuntu-source.raw"
fi
(cd "$ROOT" && go run ./test/e2e/mkdisk -size "$BLOCK_DISK_SIZE" -seed 7 -o "$WORK/data/synthetic-block.raw")
DISK0_SIZE=$(stat -c %s "$WORK/data/ubuntu-source.raw")
DISK1_SIZE=$(stat -c %s "$WORK/data/synthetic-block.raw")
DISK0_SHA=$(sha256sum "$WORK/data/ubuntu-source.raw" | cut -d' ' -f1)
DISK1_SHA=$(sha256sum "$WORK/data/synthetic-block.raw" | cut -d' ' -f1)
log "disk0 ubuntu-source.raw size=$DISK0_SIZE sha256=$DISK0_SHA (from $UBUNTU_IMG_URL sha256 $UBUNTU_IMG_SHA256)"
log "disk1 synthetic-block.raw size=$DISK1_SIZE sha256=$DISK1_SHA"

log "== build test tools (CSI hostpath $HOSTPATH_TAG, external-provisioner $PROVISIONER_TAG, node-driver-registrar $REGISTRAR_TAG)"
TOOLS="$WORK/tools"
mkdir -p "$TOOLS/bin"
build_csi() {
  local repo=$1 tag=$2 pkg=$3 out=$4
  if [[ ! -x "$TOOLS/bin/$out" ]]; then
    rm -rf "$WORK/src/$repo"
    git clone -q --depth 1 -b "$tag" "https://github.com/kubernetes-csi/$repo.git" "$WORK/src/$repo"
    (cd "$WORK/src/$repo" && CGO_ENABLED=0 GOTOOLCHAIN=auto go build -ldflags "-X main.version=$tag" -o "$TOOLS/bin/$out" "$pkg")
  fi
}
build_csi csi-driver-host-path "$HOSTPATH_TAG" ./cmd/hostpathplugin hostpathplugin
build_csi external-provisioner "$PROVISIONER_TAG" ./cmd/csi-provisioner csi-provisioner
build_csi node-driver-registrar "$REGISTRAR_TAG" ./cmd/csi-node-driver-registrar csi-node-driver-registrar
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$TOOLS/bin/fake-vsphere" ./test/e2e/fake-vsphere)
cp "$ROOT/test/e2e/Dockerfile.e2e-tools" "$TOOLS/Dockerfile"
docker build -q -t kvsp-e2e/tools:local "$TOOLS" >/dev/null

log "== build product images"
# shellcheck disable=SC2086
docker build -q -f "$ROOT/build/Dockerfile.controller" ${EXTRA_DOCKER_BUILD_ARGS:-} --build-arg VERSION=e2e --build-arg COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)" -t kvsp-controller:e2e "$ROOT" >/dev/null
# shellcheck disable=SC2086
docker build -q -f "$ROOT/build/Dockerfile.worker" ${EXTRA_DOCKER_BUILD_ARGS:-} --build-arg VERSION=e2e --build-arg COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)" -t kvsp-worker:e2e "$ROOT" >/dev/null
log "worker image: $(docker run --rm --entrypoint qemu-img kvsp-worker:e2e --version | head -1)"

log "== kind cluster ($KIND_NODE_IMAGE)"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
sed "s|__DATA_DIR__|$WORK/data|" "$ROOT/test/e2e/kind-config.yaml" > "$WORK/kind-config.yaml"
if [[ "$(stat -fc %T /sys/fs/cgroup)" != cgroup2fs ]]; then
  # Kubernetes >= 1.35 kubelets refuse cgroup v1 hosts by default. For this
  # disposable test cluster only, allow it.
  log "host uses cgroup v1: setting kubelet failCgroupV1=false for the test cluster"
  cat >> "$WORK/kind-config.yaml" <<'PATCH'
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    failCgroupV1: false
PATCH
fi
if ! sh -c 'echo -999 > /proc/self/oom_score_adj' 2>/dev/null; then
  # Nested/sandboxed hosts without CAP_SYS_RESOURCE cannot lower OOM scores,
  # which containerd does for pod sandboxes; restrict it for this test cluster.
  log "host cannot lower oom_score_adj: setting containerd restrict_oom_score_adj=true for the test cluster"
  cat >> "$WORK/kind-config.yaml" <<'PATCH'
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri"]
      restrict_oom_score_adj = true
PATCH
fi
kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --config "$WORK/kind-config.yaml" --wait 180s
cleanup() {
  kubectl logs -n vsphere-populator-system deploy/vsphere-populator-controller > "$WORK/logs/controller.log" 2>&1 || true
  kubectl logs -n fake-vsphere deploy/fake-vsphere > "$WORK/logs/fake-vsphere.log" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp > "$WORK/logs/events.txt" 2>&1 || true
  if [[ "${KEEP:-0}" != 1 ]]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
log "server version: $(kubectl version -o json | grep -m1 gitVersion | tr -d ' ",')"
for img in kvsp-e2e/tools:local kvsp-controller:e2e kvsp-worker:e2e; do kind load docker-image --name "$CLUSTER" "$img" >/dev/null; done

log "== CSI hostpath driver"
kubectl apply -f "$ROOT/test/e2e/csi-hostpath.yaml" >/dev/null
kubectl -n csi-hostpath rollout status ds/csi-hostpath --timeout=180s

log "== populator"
kubectl apply -f "$ROOT/deploy/manager/namespace.yaml" -f "$ROOT/deploy/crd/" -f "$ROOT/deploy/rbac/rbac.yaml" >/dev/null
sed -e "s|image: ghcr.io/sudharsanomprakash/kubevirt-vsphere-populator-controller:.*|image: kvsp-controller:e2e|" \
    -e "s|--worker-image=.*|--worker-image=kvsp-worker:e2e|" "$ROOT/deploy/manager/deployment.yaml" | kubectl apply -f - >/dev/null
kubectl -n vsphere-populator-system rollout status deploy/vsphere-populator-controller --timeout=180s

log "== fake vSphere (test double, not a real vSphere)"
kubectl apply -f "$ROOT/test/e2e/fake-vsphere.yaml" >/dev/null
kubectl -n fake-vsphere rollout status deploy/fake-vsphere --timeout=600s
READY=$(kubectl -n fake-vsphere exec deploy/fake-vsphere -- cat /tmp/ready)
UUID=$(echo "$READY" | sed -n 1p)
THUMB=$(echo "$READY" | sed -n 2p)
log "fake VM instanceUUID=$UUID"

log "== workload"
sed -e "s|__THUMBPRINT__|\"$THUMB\"|" -e "s|__INSTANCE_UUID__|$UUID|" -e "s|__DISK0_SIZE__|$DISK0_SIZE|" -e "s|__DISK1_SIZE__|$DISK1_SIZE|g" \
  "$ROOT/test/e2e/workload.yaml" | kubectl apply -f - >/dev/null
START=$(date +%s)

# Capture worker logs while the Jobs exist (the controller deletes them after success).
( while true; do
    for p in $(kubectl -n "$NS" get pods -l app.kubernetes.io/component=worker -o name 2>/dev/null); do
      kubectl -n "$NS" logs "$p" > "$WORK/logs/$(basename "$p").log" 2>/dev/null || true
    done
    sleep 5
  done ) &
LOGGER=$!

wait_phase() {
  local src=$1 deadline=$(( $(date +%s) + ${2:-1800} ))
  while (( $(date +%s) < deadline )); do
    local phase
    phase=$(kubectl -n "$NS" get vspherevolumesource "$src" -o jsonpath='{.status.populations[0].phase}' 2>/dev/null || true)
    case "$phase" in
      Succeeded) return 0 ;;
      Failed) kubectl -n "$NS" get vspherevolumesource "$src" -o yaml; return 1 ;;
    esac
    sleep 10
  done
  echo "timeout waiting for $src"; kubectl -n "$NS" get vspherevolumesource "$src" -o yaml; return 1
}
wait_phase app-01-disk0
log "disk0 population Succeeded after $(( $(date +%s) - START ))s"
wait_phase app-01-disk1
log "disk1 population Succeeded after $(( $(date +%s) - START ))s"
kill "$LOGGER" 2>/dev/null || true

kubectl -n "$NS" get pvc -o wide | tee -a "$REPORT"
kubectl -n "$NS" get vspherevolumesource -o yaml > "$WORK/logs/sources.yaml"
for s in app-01-disk0 app-01-disk1; do
  log "$s result: $(kubectl -n "$NS" get vspherevolumesource "$s" -o jsonpath='{.status.populations[0].result}')"
done
PV0=$(kubectl -n "$NS" get pvc app-01-root -o jsonpath='{.spec.volumeName}')
log "PV $PV0 annotations: $(kubectl get pv "$PV0" -o jsonpath='{.metadata.annotations}')"
if kubectl -n "$NS" get pvc -o name | grep -q vsphere-populator-prime; then log "FAIL: prime PVCs remain"; exit 1; fi

log "== independent content comparison (sha256sum of source vs destination)"
kubectl -n "$NS" wait pod/block-consumer --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s >/dev/null
BLOCK_OUT=$(kubectl -n "$NS" logs block-consumer)
echo "$BLOCK_OUT" | tee -a "$REPORT"
echo "$BLOCK_OUT" | grep -q "^$DISK1_SHA " || { log "FAIL: block destination sha256 differs from source"; exit 1; }
echo "$BLOCK_OUT" | grep -q "^nonzero-tail-bytes=0$" || { log "FAIL: block device tail not zero"; exit 1; }
log "PASS: block destination matches source ($DISK1_SHA) and the tail is zero"

cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: fs-reader, namespace: $NS}
spec:
  restartPolicy: Never
  securityContext: {runAsUser: 107, fsGroup: 107}
  containers:
    - name: reader
      image: kvsp-worker:e2e
      imagePullPolicy: Never
      command: [sh, -c, 'ls -ln /data; stat -c "size=%s blocks=%b mode=%a uid=%u" /data/disk.img; sha256sum /data/disk.img; sleep 3600']
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: app-01-root}}]
YAML
kubectl -n "$NS" wait pod/fs-reader --for=condition=Ready --timeout=300s >/dev/null
for _ in $(seq 1 120); do kubectl -n "$NS" logs fs-reader | grep -qE '^[0-9a-f]{64}  /data/disk.img$' && break; sleep 5; done
FS_OUT=$(kubectl -n "$NS" logs fs-reader)
echo "$FS_OUT" | tee -a "$REPORT"
echo "$FS_OUT" | grep -q "^$DISK0_SHA  /data/disk.img" || { log "FAIL: filesystem destination sha256 differs from source"; exit 1; }
log "PASS: filesystem destination disk.img matches source ($DISK0_SHA)"

if [[ "$BOOT_CHECK" == 1 ]]; then
  log "== boot check of the migrated disk (QEMU, not KubeVirt)"
  kubectl -n "$NS" exec fs-reader -- cat /data/disk.img > "$WORK/data/migrated-disk.img"
  COPY_SHA=$(sha256sum "$WORK/data/migrated-disk.img" | cut -d' ' -f1)
  [[ "$COPY_SHA" == "$DISK0_SHA" ]] || { log "FAIL: copied image differs"; exit 1; }
  "$ROOT/test/e2e/boot-check.sh" "$WORK/data/migrated-disk.img" 1500 | tee -a "$REPORT"
  rm -f "$WORK/data/migrated-disk.img"
fi
log "E2E PASSED"
