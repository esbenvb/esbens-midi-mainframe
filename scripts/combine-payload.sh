#!/usr/bin/env bash
# Join the system (built once for every board) with one board's kernel parts into that board's
# payload folder, the files the SD card needs for it. Pure file work: no Buildroot, seconds.
#
#   scripts/combine-payload.sh [--out DIR] OS_DIR KERNEL_DIR [KERNEL_DIR...]
#
#   OS_DIR      image/output/userland/emm-os              (rootfs-os.cpio.gz)
#   KERNEL_DIR  image/output/<board>/emm-kernel-<board>   (kernel, device trees, GPU firmware,
#               modules-<piN>.cpio.gz); one payload is made per KERNEL_DIR.
#   --out DIR   where the payload folders go (default: next to each KERNEL_DIR). Each is
#               DIR/emm-payload-<board>/ (several KERNEL_DIRs: DIR/<board>/emm-payload-<board>/).
#
# rootfs-<piN>.cpio.gz is the system archive followed by the board's modules archive: the Linux
# kernel unpacks concatenated cpio archives (each gzip'd separately) as one initramfs, so the
# board gets its own modules at /usr/lib/modules without the system being built again.
# config.txt, cmdline.txt, README-FIRST.txt and emm.txt.example come from the repository.
# scripts/package-sdcard.sh merges the payloads of the boards (and adds emmd).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$SCRIPT_DIR/lib.sh"

OUT=""
ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --out)     OUT=${2:?}; shift 2 ;;
    -h|--help) print_usage_comment "$0"; exit 0 ;;
    -*)        die "unknown option: $1" ;;
    *)         ARGS+=("$1"); shift ;;
  esac
done
[ "${#ARGS[@]}" -ge 2 ] || die "give the system folder and at least one kernel folder (see --help)"
OS=${ARGS[0]}
KERNELS=("${ARGS[@]:1}")
[ -f "$OS/rootfs-os.cpio.gz" ] || die "$OS/rootfs-os.cpio.gz not found (is that the emm-os folder of the userland build?)"
BOOT_TEXT=(image/board/emm/config.txt image/board/emm/cmdline.txt image/board/emm/README-FIRST.txt packaging/emm.txt.example)
for f in "${BOOT_TEXT[@]}"; do [ -f "$ROOT/$f" ] || die "missing $ROOT/$f"; done

for k in "${KERNELS[@]}"; do
  [ -d "$k" ] || die "not a directory: $k"
  name=$(basename "$k")
  case "$name" in emm-kernel-rpi3|emm-kernel-rpi4|emm-kernel-rpi5) ;; *) die "$k: expected a folder named emm-kernel-rpi3, -rpi4 or -rpi5" ;; esac
  board=${name#emm-kernel-}
  case "$board" in rpi3) pi=pi3 ;; rpi4) pi=pi4 ;; rpi5) pi=pi5 ;; esac
  mods=$k/modules-$pi.cpio.gz
  [ -f "$mods" ] || die "$mods not found"

  if [ -z "$OUT" ]; then dest=$(dirname "$k")
  elif [ "${#KERNELS[@]}" -eq 1 ]; then dest=$OUT
  else dest=$OUT/$board; fi
  payload=$dest/emm-payload-$board
  log "combining the system and $board's kernel parts into $payload"
  rm -rf "$payload"
  mkdir -p "$payload"
  # cp -R keeps this working with BSD cp on macOS as well as GNU cp.
  cp -R "$k"/. "$payload"/
  rm -f "$payload/modules-$pi.cpio.gz"
  cat "$OS/rootfs-os.cpio.gz" "$mods" > "$payload/rootfs-$pi.cpio.gz"
  for f in "${BOOT_TEXT[@]}"; do cp "$ROOT/$f" "$payload/$(basename "$f")"; done
done
