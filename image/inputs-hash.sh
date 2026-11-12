#!/usr/bin/env bash
# Print a short hash of everything that goes into one part of the image:
#
#   image/inputs-hash.sh userland|rpi3|rpi4|rpi5 [--files]
#
# The same hash means the same build inputs, so a release can reuse the result of an earlier
# build instead of running Buildroot again (.github/workflows/release.yml). It covers the files
# the build reads, not the Go code: emmd is not part of the image. --files lists them instead.
#
#   userland   the system every board runs: its defconfig, overlay, packaging files, hooks
#   rpiN       that board's kernel parts: its defconfig, kernel fragment, patches, hooks
#
# A new Buildroot version or a changed Dockerfile changes every hash. Documentation, the
# boot-partition text files (config.txt, cmdline.txt, ...) and scripts that only package things
# are not inputs: changing them never rebuilds anything.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
cd "$ROOT"

what=${1:?usage: inputs-hash.sh userland|rpi3|rpi4|rpi5 [--files]}
mode=${2:-}

common=(image/buildroot.env image/Dockerfile image/board/emm/post-image.sh)
case "$what" in
  userland)
    files=("${common[@]}" image/configs/emm_userland_defconfig image/board/emm/post-build.sh image/board/emm/os-api
           packaging/systemd/emmd.service packaging/systemd/emm-ssh.service packaging/avahi/emm-http.service
           packaging/nm/zz-emm.conf packaging/nm/zz-emm-appliance.conf packaging/udev/90-emm-usb-midi.rules)
    while IFS= read -r f; do files+=("$f"); done < <(find image/board/emm/overlay \( -type f -o -type l \) | LC_ALL=C sort)
    ;;
  rpi3|rpi4|rpi5)
    files=("${common[@]}" "image/configs/emm_${what}_defconfig" image/board/emm/linux.fragment
           image/board/emm/kernel-required.txt "image/board/emm/kernel-required.$what.txt")
    while IFS= read -r f; do files+=("$f"); done < <(find image/patches \( -type f -o -type l \) | LC_ALL=C sort)
    ;;
  *) echo "inputs-hash.sh: unknown part '$what' (userland, rpi3, rpi4, rpi5)" >&2; exit 2 ;;
esac

for f in "${files[@]}"; do
  [ -f "$f" ] || { echo "inputs-hash.sh: missing input $f" >&2; exit 1; }
done
if [ "$mode" = --files ]; then printf '%s\n' "${files[@]}"; exit 0; fi

if command -v sha256sum >/dev/null 2>&1; then sum() { sha256sum "$@"; }; else sum() { shasum -a 256 "$@"; }; fi
# Hash the per-file hashes with their names: a file moving or being renamed changes the result.
printf '%s\n' "${files[@]}" | LC_ALL=C sort -u | while IFS= read -r f; do sum "$f"; done | sum | cut -c1-12
