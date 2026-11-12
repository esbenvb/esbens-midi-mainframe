#!/usr/bin/env bash
# Merge the per-board payload directories produced by the Buildroot post-image
# step into one SD-card tree and zip it. The result works on Raspberry Pi 3 (B, B+, A+),
# Zero 2 W, 4, 400, CM4, CM4S, 5, 500, 500+, CM5 and CM5 Lite (the boards whose device
# trees the builds install): the three kernels, the three initramfs files, one
# config.txt whose [pi3]/[pi02]/[pi4]/[pi5] sections pick the right pair.
#
#   scripts/package-sdcard.sh [--version V] [--out DIR] [--suffix S] [--app EMMD] [--dry-run] PAYLOAD_DIR [PAYLOAD_DIR...]
#
# --app EMMD is the application (dist/emmd-linux-arm64): it goes onto the card as "emmd", where the
# image starts it from (the image itself does not contain it; see usr/libexec/emm-launch).
#
# The tree also gets an update/ folder (update.sh, the push script and a list of files per board
# present), so the same zip that makes a new card updates a running device over the network.
#
# Output: DIR/emm-sdcard-V/ (the tree) and DIR/emm-sdcard-V.zip (+ .sha256). With
# --suffix S (for example rpi4, for a zip of one board) the names are emm-sdcard-V-S.
# Characters in V other than A-Z a-z 0-9 . _ + - (for example the "/" of a tag
# such as release/1.0) are replaced by "-" in file names and in the README stamp.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$SCRIPT_DIR/lib.sh"

VERSION=""
SUFFIX=""
APP=""
OUT="$ROOT/dist"
PAYLOADS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION=${2:?}; shift 2 ;;
    --out)     OUT=${2:?}; shift 2 ;;
    --suffix)  SUFFIX=${2:?}; shift 2 ;;
    --app)     APP=${2:?}; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) print_usage_comment "$0"; exit 0 ;;
    -*)        die "unknown option: $1" ;;
    *)         PAYLOADS+=("$1"); shift ;;
  esac
done
[ "${#PAYLOADS[@]}" -gt 0 ] || die "give at least one payload directory (see --help)"
[ -n "$VERSION" ] || VERSION=$(emm_version "$ROOT")
# The version ends up in directory and file names and in a sed replacement
# (README stamp), so keep it to characters that are safe in both. "git describe"
# can legitimately give a "/" (tag release/1.0); renaming is friendlier than
# failing at the end of a build that took hours.
SAFE_VERSION=$(printf '%s' "$VERSION" | LC_ALL=C tr -c 'A-Za-z0-9._+-' '-')
if [ "$SAFE_VERSION" != "$VERSION" ]; then
  warn "version '$VERSION' contains characters that are not safe in file names; using '$SAFE_VERSION'"
  VERSION=$SAFE_VERSION
fi

NAME="emm-sdcard-$VERSION${SUFFIX:+-$SUFFIX}"
TREE="$OUT/$NAME"

# Every file the Pi firmware or the kernel needs for the boards we claim to support.
REQUIRED_COMMON=(config.txt cmdline.txt emm.txt.example README-FIRST.txt emmd)
REQUIRED_PI3=(kernel_pi3.img bootcode.bin start.elf fixup.dat rootfs-pi3.cpio.gz bcm2710-rpi-3-b.dtb bcm2710-rpi-zero-2-w.dtb)
REQUIRED_PI4=(kernel8.img start4.elf fixup4.dat rootfs-pi4.cpio.gz bcm2711-rpi-4-b.dtb)
REQUIRED_PI5=(kernel_2712.img rootfs-pi5.cpio.gz bcm2712-rpi-5-b.dtb bcm2712-rpi-500.dtb)

for d in "${PAYLOADS[@]}"; do
  [ -d "$d" ] || die "not a directory: $d"
done

log "assembling $TREE"
run rm -rf "$TREE"
run mkdir -p "$TREE"
for d in "${PAYLOADS[@]}"; do
  # cp -R keeps this working with BSD cp on macOS as well as GNU cp.
  run cp -R "$d"/. "$TREE"/
done

if [ -n "$APP" ]; then
  [ -f "$APP" ] || die "--app: $APP not found"
  run cp "$APP" "$TREE/emmd"
fi

if [ "$DRY_RUN" != 1 ]; then
  # Version stamp in the readme.
  if grep -q '@VERSION@' "$TREE/README-FIRST.txt" 2>/dev/null; then
    sed "s/@VERSION@/$VERSION/g" "$TREE/README-FIRST.txt" > "$TREE/README-FIRST.txt.tmp"
    mv "$TREE/README-FIRST.txt.tmp" "$TREE/README-FIRST.txt"
  fi
  printf '%s\n' "$VERSION" > "$TREE/VERSION"

  # Which boards did we get? Require the full set for each board that is present.
  missing=()
  have_pi3=0; have_pi4=0; have_pi5=0
  [ -e "$TREE/kernel_pi3.img" ] && have_pi3=1
  [ -e "$TREE/kernel8.img" ] && have_pi4=1
  [ -e "$TREE/kernel_2712.img" ] && have_pi5=1
  [ "$have_pi3$have_pi4$have_pi5" != 000 ] || die "none of kernel_pi3.img (Pi 3), kernel8.img (Pi 4) or kernel_2712.img (Pi 5) found in the payloads"
  for f in "${REQUIRED_COMMON[@]}"; do [ -e "$TREE/$f" ] || missing+=("$f"); done
  if [ "$have_pi3" = 1 ]; then for f in "${REQUIRED_PI3[@]}"; do [ -e "$TREE/$f" ] || missing+=("$f"); done; fi
  if [ "$have_pi4" = 1 ]; then for f in "${REQUIRED_PI4[@]}"; do [ -e "$TREE/$f" ] || missing+=("$f"); done; fi
  if [ "$have_pi5" = 1 ]; then for f in "${REQUIRED_PI5[@]}"; do [ -e "$TREE/$f" ] || missing+=("$f"); done; fi
  if [ "${#missing[@]}" -gt 0 ]; then
    die "the assembled tree is missing: ${missing[*]}"
  fi
  [ "$have_pi3" = 1 ] || warn "no Pi 3 payload: this card image will not boot a Pi 3 or Zero 2 W"
  [ "$have_pi4" = 1 ] || warn "no Pi 4 payload: this card image will not boot a Pi 4, 400 or CM4"
  [ "$have_pi5" = 1 ] || warn "no Pi 5 payload: this card image will not boot a Pi 5, 500 or CM5"

  # update/: what it takes to update a running device from this very tree, over SSH. The folder
  # is a few KB and lands on the card too, which does no harm (nothing on the Pi runs it).
  UPD="$TREE/update"
  mkdir -p "$UPD/scripts" "$UPD/manifests"
  cp "$SCRIPT_DIR/push-boot.sh" "$SCRIPT_DIR/lib.sh" "$UPD/scripts/"
  for d in "${PAYLOADS[@]}"; do
    # The board of a payload is told by its kernel. What it needs: every file of the payload (its
    # own kernel, system and firmware plus the shared boot files), the application and the stamp.
    if   [ -e "$d/kernel_pi3.img" ];  then b=rpi3
    elif [ -e "$d/kernel8.img" ];     then b=rpi4
    elif [ -e "$d/kernel_2712.img" ]; then b=rpi5
    else continue; fi
    { (cd "$d" && find . -maxdepth 1 -type f ! -name '.*' | sed 's|^\./||'); echo VERSION; echo emmd; } \
      | LC_ALL=C sort -u > "$UPD/manifests/$b.txt"
  done
  cat > "$UPD/update.sh" <<'EOF'
#!/usr/bin/env bash
# Update a running EMM device (the SD-card image) over the network, with the files of this package.
#
#   update/update.sh [--reboot] [--dry-run] [--ssh-opts "OPTS"] root@emm.local
#
# First switch SSH on in the web interface (System tab) and set a password there.
# The device is asked which Raspberry Pi it is, and only that board's files are copied to its
# boot partition. Your settings (emm.txt and the emm/ folder) are not touched. A config.txt,
# cmdline.txt or emmd that differs is kept as <name>.previous (emmd.previous is what the device
# falls back to if the new emmd does not start). A new system or kernel takes effect after a
# reboot (--reboot does that). Do not unplug the device while it copies.
# To update only the application, which needs no reboot, use the emm-app-update package.
# More options: update/scripts/push-boot.sh --help
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TREE=$(cd "$HERE/.." && pwd)
case "${1:-}" in -h|--help) sed -n '2,/^set /p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;; esac
pick=--auto
for a in "$@"; do [ "$a" = --board ] && pick=""; done
exec "$HERE/scripts/push-boot.sh" ${pick:+"$pick"} --from "$TREE" --manifest-dir "$HERE/manifests" "$@"
EOF
  chmod +x "$UPD/update.sh" "$UPD/scripts/push-boot.sh"
  cat > "$UPD/README.txt" <<EOF
Update a running device with this package
=========================================

This folder is for a device that already runs the EMM SD-card image. It updates the system,
kernel and application of that device over the network, from the files next to this folder.
(To put EMM on a new card you do not need it. Nothing on the Pi uses it.)

  1. In the device's web interface, System tab: set a web password, then switch SSH on.
  2. On your computer (macOS or Linux, with ssh), unzip this package. In the unzipped folder run:

         update/update.sh --reboot root@emm.local

     It asks for the SSH password you set, finds out which Raspberry Pi it is talking to and
     copies that board's files. Add --dry-run first to see what would be copied.

Your settings, presets and network setup stay as they are. Network MIDI (RTP-MIDI) is not
meant for this, and neither is WiFi that comes and goes: use a cable if you can, and do not
remove power while it copies. If something goes wrong, the SD card can always be written
again from this package.
EOF

  # Nothing the FAT32 card should not carry.
  find "$TREE" \( -name '.DS_Store' -o -name '._*' \) -delete
fi

ZIP="$OUT/$NAME.zip"
log "writing $ZIP"
if [ "$DRY_RUN" = 1 ]; then
  run sh -c "cd '$OUT' && zip -r -X -9 '$NAME.zip' '$NAME'"
else
  rm -f "$ZIP"
  if have zip; then
    # -X: no extra file attributes, so the archive is the same on every host.
    (cd "$OUT" && zip -q -r -X -9 "$NAME.zip" "$NAME")
  elif have python3; then
    python3 - "$OUT" "$NAME" <<'PY'
import os, sys, zipfile
out, name = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(os.path.join(out, name + ".zip"), "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
    for dp, dn, fn in os.walk(os.path.join(out, name)):
        dn.sort()
        for f in sorted(fn):
            p = os.path.join(dp, f)
            z.write(p, os.path.relpath(p, out))
PY
  else
    die "need 'zip' or 'python3' to create the archive"
  fi
  if have shasum; then
    (cd "$OUT" && shasum -a 256 "$NAME.zip" > "$NAME.zip.sha256")
  elif have sha256sum; then
    (cd "$OUT" && sha256sum "$NAME.zip" > "$NAME.zip.sha256")
  fi
  log "done: $ZIP ($(du -h "$ZIP" | cut -f1))"
  printf '\nUnzip the *contents* of %s onto a FAT32-formatted SD card (see docs/INSTALL.md).\n' "$NAME" >&2
fi
