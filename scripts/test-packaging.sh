#!/usr/bin/env bash
# Test the file work of a release with fake build results: combine-payload.sh, package-sdcard.sh,
# package-release.sh and the lists push-boot.sh works from. No Buildroot, no Pi, no ssh.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0
ok()   { echo "ok   $1"; }
bad()  { echo "FAIL $1"; fail=1; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# --- fake build results -----------------------------------------------------------------------
mkdir -p "$T/os/emm-os" "$T/dist"
gz() { printf '%s' "$1" | gzip -9n; }
gz "SYSTEM-ARCHIVE" > "$T/os/emm-os/rootfs-os.cpio.gz"
printf 'x' > "$T/dist/emmd-linux-arm64"; printf 'y' > "$T/dist/emmd-linux-armv7"
mk() { # board kernel pi files...
  local b=$1 k=$2 pi=$3; shift 3
  local d=$T/k/$b/emm-kernel-$b; mkdir -p "$d"
  printf 'kernel-%s' "$b" > "$d/$k"; gz "MODULES-$b" > "$d/modules-$pi.cpio.gz"
  for f in "$@"; do printf 'f' > "$d/$f"; done
}
mk rpi3 kernel_pi3.img pi3 bootcode.bin start.elf fixup.dat bcm2710-rpi-3-b.dtb bcm2710-rpi-zero-2-w.dtb
mk rpi4 kernel8.img pi4 start4.elf fixup4.dat bcm2711-rpi-4-b.dtb
mk rpi5 kernel_2712.img pi5 bcm2712-rpi-5-b.dtb bcm2712-rpi-500.dtb

# --- combine ----------------------------------------------------------------------------------
for b in rpi3 rpi4 rpi5; do
  "$HERE/combine-payload.sh" --out "$T/pl/$b" "$T/os/emm-os" "$T/k/$b/emm-kernel-$b" 2>/dev/null
done
P4=$T/pl/rpi4/emm-payload-rpi4
check "combine: rootfs is the system followed by the board's modules" \
  '[ "$(gzip -dc "$P4/rootfs-pi4.cpio.gz")" = "SYSTEM-ARCHIVEMODULES-rpi4" ]'
check "combine: the modules file is not left in the payload" '[ ! -e "$P4/modules-pi4.cpio.gz" ]'
check "combine: kernel, firmware and boot text files are there" \
  '[ -f "$P4/kernel8.img" ] && [ -f "$P4/start4.elf" ] && [ -f "$P4/config.txt" ] && [ -f "$P4/cmdline.txt" ] && [ -f "$P4/emm.txt.example" ] && [ -f "$P4/README-FIRST.txt" ]'
check "combine: no emmd in a payload (the card gets it separately)" '[ ! -e "$P4/emmd" ]'

# --- release ----------------------------------------------------------------------------------
"$HERE/package-release.sh" --version v9.9.9 --out "$T/rel" --dist "$T/dist" \
  "$T/pl/rpi3/emm-payload-rpi3" "$T/pl/rpi4/emm-payload-rpi4" "$T/pl/rpi5/emm-payload-rpi5" >/dev/null 2>&1
for f in emm-sdcard-v9.9.9.zip emm-sdcard-v9.9.9-rpi3.zip emm-sdcard-v9.9.9-rpi4.zip emm-sdcard-v9.9.9-rpi5.zip \
         emm-app-update-v9.9.9.zip emm-pios-v9.9.9-arm64.tar.gz emmd-v9.9.9-linux-arm64 SHA256SUMS; do
  check "release has $f" '[ -f "$T/rel/$f" ]'
done
check "SHA256SUMS verifies" '(cd "$T/rel" && { sha256sum -c SHA256SUMS >/dev/null 2>&1 || shasum -a 256 -c SHA256SUMS >/dev/null 2>&1; })'
mkdir "$T/x" && ( cd "$T/x" && unzip -q "$T/rel/emm-sdcard-v9.9.9-rpi4.zip" )
check "card zip carries emmd and the board's files" \
  '[ -f "$T/x/emm-sdcard-v9.9.9-rpi4/emmd" ] && [ -f "$T/x/emm-sdcard-v9.9.9-rpi4/rootfs-pi4.cpio.gz" ] && [ -f "$T/x/emm-sdcard-v9.9.9-rpi4/kernel8.img" ]'
check "no separate emm-update zip any more" '[ ! -e "$T/rel/emm-update-v9.9.9.zip" ]'
# The card zip carries its own update/ folder.
mkdir "$T/u" && ( cd "$T/u" && unzip -q "$T/rel/emm-sdcard-v9.9.9.zip" )
U=$T/u/emm-sdcard-v9.9.9
check "card zip: update/ has the script (executable), the push script and a list per board" \
  '[ -x "$U/update/update.sh" ] && [ -x "$U/update/scripts/push-boot.sh" ] && [ -f "$U/update/scripts/lib.sh" ] && [ -f "$U/update/README.txt" ] && [ -f "$U/update/manifests/rpi3.txt" ] && [ -f "$U/update/manifests/rpi4.txt" ] && [ -f "$U/update/manifests/rpi5.txt" ]'
check "update list: the board's files (emmd, rootfs, kernel), not another board's kernel or the update folder" \
  'grep -qx emmd "$U/update/manifests/rpi4.txt" && grep -qx rootfs-pi4.cpio.gz "$U/update/manifests/rpi4.txt" && grep -qx kernel8.img "$U/update/manifests/rpi4.txt" && ! grep -q "kernel_2712\|update" "$U/update/manifests/rpi4.txt"'
check "every file an update list names is in the card tree" \
  'for b in rpi3 rpi4 rpi5; do while read -r n; do [ -f "$U/$n" ] || exit 1; done < "$U/update/manifests/$b.txt"; done'
out=$("$U/update/update.sh" --board rpi4 --dry-run root@x 2>&1 || true)
check "update dry run (all boards zip) sends the board's files only" \
  'printf "%s" "$out" | grep -q "kernel8.img" && printf "%s" "$out" | grep -q "rootfs-pi4" && printf "%s" "$out" | grep -q " emmd" && ! printf "%s" "$out" | grep -q "kernel_2712\|kernel_pi3\|update"'
# A zip for one board only updates that board.
mkdir "$T/u4" && ( cd "$T/u4" && unzip -q "$T/rel/emm-sdcard-v9.9.9-rpi4.zip" )
U4=$T/u4/emm-sdcard-v9.9.9-rpi4
check "per-board zip: only its own list" '[ -f "$U4/update/manifests/rpi4.txt" ] && [ ! -e "$U4/update/manifests/rpi5.txt" ]'
out=$("$U4/update/update.sh" --board rpi4 --dry-run root@x 2>&1 || true)
check "per-board zip dry run works for its board" 'printf "%s" "$out" | grep -q "kernel8.img"'
out=$("$U4/update/update.sh" --board rpi5 --dry-run root@x 2>&1 || true)
check "per-board zip refuses another board and says what it holds" \
  'printf "%s" "$out" | grep -q "no files for rpi5" && printf "%s" "$out" | grep -q "rpi4"'
mkdir "$T/a" && ( cd "$T/a" && unzip -q "$T/rel/emm-app-update-v9.9.9.zip" )
A=$T/a/emm-app-update-v9.9.9
check "app update zip: only emmd" '[ -f "$A/boot/emmd" ] && [ "$(ls "$A/boot" | wc -l | tr -d " ")" = 1 ] && [ "$(cat "$A/manifests/rpi5.txt")" = emmd ]'
out=$("$A/update.sh" --board rpi5 --dry-run root@x 2>&1 || true)
check "app update dry run sends emmd only and would restart it" \
  'printf "%s" "$out" | grep -q "emmd" && ! printf "%s" "$out" | grep -q "kernel" && printf "%s" "$out" | grep -q "restart emmd"'
exit "$fail"
