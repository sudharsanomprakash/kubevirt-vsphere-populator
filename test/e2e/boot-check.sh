#!/usr/bin/env bash
# Boots a raw disk image with QEMU (TCG emulation, no KVM required) through a
# copy-on-write overlay, so the image under test is not modified, and waits
# for a marker that cloud-init prints from inside the booted guest.
#
# This checks that a guest boots from the disk. It is not a KubeVirt boot.
#
# usage: boot-check.sh <raw-disk-image> [timeout-seconds]
set -euo pipefail
img=$1
timeout=${2:-900}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
marker="KVSP-BOOT-OK-$(date +%s)"

cat > "$work/user-data" <<UD
#cloud-config
bootcmd:
  - echo "$marker bootcmd" > /dev/ttyS0
runcmd:
  - echo "$marker runcmd \$(uname -r) \$(findmnt -n -o SOURCE /)" > /dev/ttyS0
  - poweroff
UD
echo "instance-id: kvsp-boot-check" > "$work/meta-data"
cloud-localds "$work/seed.img" "$work/user-data" "$work/meta-data"
qemu-img create -q -f qcow2 -F raw -b "$(realpath "$img")" "$work/overlay.qcow2"

accel=tcg
[[ -w /dev/kvm ]] && accel=kvm
echo "booting $(basename "$img") with accel=$accel (timeout ${timeout}s)"
start=$(date +%s)
set +e
timeout "$timeout" qemu-system-x86_64 -machine q35,accel=$accel -m 1536 -smp 2 -nographic -no-reboot \
  -drive file="$work/overlay.qcow2",if=virtio,format=qcow2 \
  -drive file="$work/seed.img",if=virtio,format=raw \
  -serial file:"$work/console.log" -monitor none -display none >/dev/null 2>&1
rc=$?
set -e
elapsed=$(( $(date +%s) - start ))
if grep -q "$marker runcmd" "$work/console.log"; then
  grep -a "$marker" "$work/console.log" | tr -d '\r'
  grep -a -m1 "Linux version" "$work/console.log" | tr -d '\r' || true
  echo "BOOT-CHECK PASSED after ${elapsed}s (qemu exit $rc)"
  exit 0
fi
echo "BOOT-CHECK FAILED after ${elapsed}s (qemu exit $rc); last console lines:"
tail -n 40 "$work/console.log" | tr -d '\r'
exit 1
