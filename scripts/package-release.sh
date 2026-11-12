#!/usr/bin/env bash
# Assemble everything a GitHub release offers, from built pieces. No building happens here.
#
#   scripts/package-release.sh --version V [--out DIR] [--dist DIR] [--manual PDF]
#                              [--notes FILE] PAYLOAD_DIR [PAYLOAD_DIR...]
#
# PAYLOAD_DIR is a board payload from image/build.sh (image/output/<board>/emm-payload-<board>);
# the directory name says the board (rpi3, rpi4 or rpi5). --dist is the folder with
# emmd-linux-arm64 and emmd-linux-armv7 (default dist/); --manual is the PDF manual.
#
# Files written to DIR (default release/):
#   emm-sdcard-V.zip                 SD card for any supported board (all payloads given)
#   emm-sdcard-V-rpi3.zip ...        the same for one board
#   emm-update-V.zip                 over-the-air update of a running device: system, kernel and app
#   emm-app-update-V.zip             over-the-air update of the application (emmd) alone: one small file
#   emm-pios-V-arm64.tar.gz          installer bundle for Raspberry Pi OS Lite 64-bit
#   emmd-V-linux-arm64, ...-armv7    the daemon alone
#   EMM-manual-V.pdf                 the manual
#   SHA256SUMS                       checksums of all of the above
# --notes FILE also writes the release description (the same table) to FILE.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$SCRIPT_DIR/lib.sh"

VERSION=""
OUT="$ROOT/release"
DIST="$ROOT/dist"
MANUAL=""
NOTES=""
PAYLOADS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION=${2:?}; shift 2 ;;
    --out)     OUT=${2:?}; shift 2 ;;
    --dist)    DIST=${2:?}; shift 2 ;;
    --manual)  MANUAL=${2:?}; shift 2 ;;
    --notes)   NOTES=${2:?}; shift 2 ;;
    -h|--help) print_usage_comment "$0"; exit 0 ;;
    -*)        die "unknown option: $1" ;;
    *)         PAYLOADS+=("$1"); shift ;;
  esac
done
[ -n "$VERSION" ] || die "give --version (for example v0.1-alpha1)"
[ "${#PAYLOADS[@]}" -gt 0 ] || die "give at least one payload directory (see --help)"
case "$VERSION" in *[!A-Za-z0-9._+-]*) die "version '$VERSION' may only contain letters, digits and . _ + -" ;; esac
[ -f "$DIST/emmd-linux-arm64" ] || die "$DIST/emmd-linux-arm64 is missing (make dist VERSION=$VERSION)"
[ -z "$MANUAL" ] || [ -f "$MANUAL" ] || die "manual not found: $MANUAL"
have zip || die "need zip"
case "$OUT" in /*) ;; *) OUT=$PWD/$OUT ;; esac

BOARDS=()
for d in "${PAYLOADS[@]}"; do
  [ -d "$d" ] || die "not a directory: $d"
  b=${d%/}; b=${b##*/}; b=${b#emm-payload-}
  case "$b" in rpi3|rpi4|rpi5) BOARDS+=("$b") ;; *) die "cannot tell the board from '$d' (expected .../emm-payload-rpi3, -rpi4 or -rpi5)" ;; esac
done

log "assembling the release $VERSION in $OUT"
rm -rf "$OUT"
mkdir -p "$OUT"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# --- SD-card zips: all boards together, and one per board ---------------------
"$SCRIPT_DIR/package-sdcard.sh" --version "$VERSION" --out "$WORK/sd" --app "$DIST/emmd-linux-arm64" "${PAYLOADS[@]}"
mv "$WORK/sd/emm-sdcard-$VERSION.zip" "$OUT/"
UNIFIED_TREE="$WORK/sd/emm-sdcard-$VERSION"
i=0
for b in "${BOARDS[@]}"; do
  "$SCRIPT_DIR/package-sdcard.sh" --version "$VERSION" --suffix "$b" --out "$WORK/sd-$b" --app "$DIST/emmd-linux-arm64" "${PAYLOADS[$i]}"
  mv "$WORK/sd-$b/emm-sdcard-$VERSION-$b.zip" "$OUT/"
  i=$((i + 1))
done

# --- Update zip: the unified tree, the push script and a file list per board --------
UP="$WORK/emm-update-$VERSION"
mkdir -p "$UP/scripts" "$UP/manifests"
cp -R "$UNIFIED_TREE" "$UP/boot"
cp "$SCRIPT_DIR/push-boot.sh" "$SCRIPT_DIR/lib.sh" "$UP/scripts/"
i=0
for b in "${BOARDS[@]}"; do
  # What the board needs: every file of its payload (its own kernel, system and
  # firmware plus the common config files), the application (emmd) and the VERSION stamp.
  { (cd "${PAYLOADS[$i]}" && find . -maxdepth 1 -type f ! -name '.*' | sed 's|^\./||'); echo VERSION; echo emmd; } | LC_ALL=C sort -u > "$UP/manifests/$b.txt"
  i=$((i + 1))
done
cat > "$UP/update.sh" <<'EOF'
#!/usr/bin/env bash
# Update a running EMM device (the SD-card image) over the network.
#
#   ./update.sh [--reboot] [--dry-run] [--ssh-opts "OPTS"] root@emm.local
#
# First switch SSH on in the web interface (System tab) and set a password there.
# The device is asked which Raspberry Pi it is, and only that board's files are copied
# to its boot partition. Your settings (emm.txt and the emm/ folder) are not touched.
# A config.txt, cmdline.txt or emmd that differs is kept as <name>.previous (emmd.previous is
# what the device falls back to if the new emmd does not start). A new system or kernel takes
# effect after a reboot (--reboot does that). Do not unplug the device while it copies.
# To update only the application, which needs no reboot, use the emm-app-update package.
# More options: ./scripts/push-boot.sh --help
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
case "${1:-}" in -h|--help) sed -n '2,/^set /p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;; esac
pick=--auto
for a in "$@"; do [ "$a" = --board ] && pick=""; done
exec "$HERE/scripts/push-boot.sh" ${pick:+"$pick"} --from "$HERE/boot" --manifest-dir "$HERE/manifests" "$@"
EOF
chmod +x "$UP/update.sh" "$UP/scripts/push-boot.sh"
cat > "$UP/README.txt" <<EOF
Esben's MIDI Mainframe $VERSION: update package
==============================================

For a device that already runs the EMM SD-card image. To put EMM on a new card, use the
emm-sdcard zip instead.

  1. In the device's web interface, System tab: set a web password, then switch SSH on.
  2. On your computer (macOS or Linux, with ssh), unzip this package and run:

         ./update.sh --reboot root@emm.local

     It asks for the SSH password you set, finds out which Raspberry Pi it is talking to
     and copies that board's files. Add --dry-run first to see what would be copied.

Your settings, presets and network setup stay as they are. Network MIDI (RTP-MIDI) is
not meant for this, and neither is WiFi that comes and goes: use a cable if you can,
and do not remove power while it copies. If something goes wrong, the SD card can
always be written again from the emm-sdcard zip.
EOF
(cd "$WORK" && zip -q -r -X -9 "$OUT/emm-update-$VERSION.zip" "emm-update-$VERSION")

# --- App update zip: emmd alone, no reboot -----------------------------------------
AP="$WORK/emm-app-update-$VERSION"
mkdir -p "$AP/scripts" "$AP/boot" "$AP/manifests"
cp "$SCRIPT_DIR/push-boot.sh" "$SCRIPT_DIR/lib.sh" "$AP/scripts/"
cp "$DIST/emmd-linux-arm64" "$AP/boot/emmd"
chmod +x "$AP/boot/emmd"
for b in "${BOARDS[@]}"; do echo emmd > "$AP/manifests/$b.txt"; done
cat > "$AP/update.sh" <<'EOF'
#!/usr/bin/env bash
# Update the application (emmd) of a running EMM device (the SD-card image) over the network.
#
#   ./update.sh [--dry-run] [--reboot] [--ssh-opts "OPTS"] root@emm.local
#
# First switch SSH on in the web interface (System tab) and set a password there.
# Only /boot/emmd is copied; the system and kernel on the card are not touched. The previous
# emmd is kept as emmd.previous and used if the new one does not start. The new emmd is started
# right away (the web interface restarts; no reboot). Your settings are not touched.
# This only works if the card already runs a system that this emmd supports; the application
# says so on the device (journalctl -u emmd) and falls back to the previous one if it does not:
# then use the emm-update package, which brings the system along.
# More options: ./scripts/push-boot.sh --help
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
case "${1:-}" in -h|--help) sed -n '2,/^set /p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;; esac
pick=--auto
for a in "$@"; do [ "$a" = --board ] && pick=""; done
restart=--restart-app
for a in "$@"; do [ "$a" = --reboot ] && restart=""; done
exec "$HERE/scripts/push-boot.sh" ${pick:+"$pick"} ${restart:+"$restart"} --from "$HERE/boot" --manifest-dir "$HERE/manifests" "$@"
EOF
chmod +x "$AP/update.sh" "$AP/scripts/push-boot.sh"
cat > "$AP/README.txt" <<EOF
Esben's MIDI Mainframe $VERSION: application update
==================================================

For a device that already runs the EMM SD-card image. It replaces only the application (emmd,
about 10 MB) on the card: no new system, no reboot.

  1. In the device's web interface, System tab: set a web password, then switch SSH on.
  2. On your computer (macOS or Linux, with ssh), unzip this package and run:

         ./update.sh root@emm.local

The previous application stays on the card as emmd.previous and is used automatically if the
new one cannot start. If this version needs a newer system than the one on the card, it will
not start and the previous one runs instead: use the emm-update package then.
EOF
(cd "$WORK" && zip -q -r -X -9 "$OUT/emm-app-update-$VERSION.zip" "emm-app-update-$VERSION")

# --- Raspberry Pi OS bundle: the binary, the installer and its files ----------------
PI="$WORK/emm-pios-$VERSION-arm64"
mkdir -p "$PI/dist" "$PI/scripts"
cp "$DIST/emmd-linux-arm64" "$PI/dist/"
chmod +x "$PI/dist/emmd-linux-arm64"
cp "$SCRIPT_DIR/deploy.sh" "$SCRIPT_DIR/install-on-pi.sh" "$SCRIPT_DIR/lib.sh" "$PI/scripts/"
cp -R "$ROOT/packaging" "$PI/packaging"
cp "$ROOT/LICENSE" "$PI/" 2>/dev/null || true
printf '%s\n' "$VERSION" > "$PI/VERSION"
cat > "$PI/install.sh" <<'EOF'
#!/usr/bin/env bash
# Install EMM on THIS Raspberry Pi (64-bit Raspberry Pi OS Lite, Bookworm or Trixie).
# Safe to run again; that is also how you update. Extra flags go to the installer.
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ "$(id -u)" != 0 ]; then exec sudo bash "$HERE/scripts/install-on-pi.sh" "$@"; fi
exec bash "$HERE/scripts/install-on-pi.sh" "$@"
EOF
cat > "$PI/deploy.sh" <<'EOF'
#!/usr/bin/env bash
# Install or update EMM on a Raspberry Pi over SSH, from this computer:
#   ./deploy.sh pi@raspberrypi.local
# (--dry-run shows the plan first; --uninstall removes EMM.)
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scripts/deploy.sh" "$@"
EOF
chmod +x "$PI/install.sh" "$PI/deploy.sh" "$PI"/scripts/*.sh
cat > "$PI/README.txt" <<EOF
Esben's MIDI Mainframe $VERSION: installer for Raspberry Pi OS Lite (64-bit)
===========================================================================

You need a Pi 3, 4, 5 or Zero 2 W running Raspberry Pi OS Lite (64-bit), Bookworm or
Trixie, with SSH switched on and a network connection (see docs/INSTALL.md in the project).

From your computer (macOS or Linux):    ./deploy.sh pi@raspberrypi.local
Or on the Pi itself:                    ./install.sh

Then open http://emm.local/. Run the same command again with a newer bundle to update;
your settings stay. Remove EMM with: ./deploy.sh --uninstall pi@raspberrypi.local
EOF
COPYFILE_DISABLE=1 tar -C "$WORK" -czf "$OUT/emm-pios-$VERSION-arm64.tar.gz" "emm-pios-$VERSION-arm64"

# --- Binaries and the manual -------------------------------------------------------
cp "$DIST/emmd-linux-arm64" "$OUT/emmd-$VERSION-linux-arm64"
if [ -f "$DIST/emmd-linux-armv7" ]; then cp "$DIST/emmd-linux-armv7" "$OUT/emmd-$VERSION-linux-armv7"; fi
chmod +x "$OUT"/emmd-*-linux-*   # downloaded CI artifacts arrive without the executable bit
if [ -n "$MANUAL" ]; then cp "$MANUAL" "$OUT/EMM-manual-$VERSION.pdf"; fi

# --- Checksums ---------------------------------------------------------------------
(
  cd "$OUT"
  files=$(ls | LC_ALL=C sort)
  # shellcheck disable=SC2086  # file names here contain no spaces
  if have sha256sum; then sha256sum $files > SHA256SUMS; else shasum -a 256 $files > SHA256SUMS; fi
)

# --- Release description -----------------------------------------------------------
if [ -n "$NOTES" ]; then
  mkdir -p "$(dirname "$NOTES")"
  cat > "$NOTES" <<EOF
## Esben's MIDI Mainframe $VERSION

**What to download**

| File | For | How |
|---|---|---|
| \`emm-sdcard-$VERSION.zip\` | A **new SD card**, any supported Pi (3 B/B+/A+, Zero 2 W, 4, 400, CM4, 5, 500, CM5) | Format the card FAT32, unzip, copy the *contents* onto it, boot |
| \`emm-sdcard-$VERSION-rpi3.zip\`, \`-rpi4.zip\`, \`-rpi5.zip\` | The same, smaller download for one board family | The same |
| \`emm-update-$VERSION.zip\` | A Pi that **already runs the EMM card image**: update system, kernel and application over the network | Switch SSH on in the web interface, unzip, run \`./update.sh --reboot root@emm.local\`. Settings stay |
| \`emm-app-update-$VERSION.zip\` | The same Pi, when only the **application** changed (a few MB, no reboot) | The same, \`./update.sh root@emm.local\` |
| \`emm-pios-$VERSION-arm64.tar.gz\` | A Pi running **Raspberry Pi OS Lite 64-bit** | Extract, run \`./deploy.sh pi@raspberrypi.local\` (or \`./install.sh\` on the Pi). Run it again to update |
| \`emmd-$VERSION-linux-arm64\`, \`-armv7\` | Your own packaging (armv7 is untested) | The daemon alone, static, web interface included |
| \`EMM-manual-$VERSION.pdf\` | Everyone | The user manual |
| \`SHA256SUMS\` | Everyone | \`shasum -a 256 -c SHA256SUMS\` |

Network MIDI (RTP-MIDI) and Bluetooth are fine for playing music, but do not rely on them
for backing up or restoring a device or for firmware updates. Use USB or a cable for those.

**Status.** This is an early release. The Raspberry Pi OS installer is the better-tested route. The SD-card
image is new: it has been built for all three board families, but not every board and feature has been
verified on real hardware yet. Please report what works and what does not.
EOF
fi

log "done: $(ls "$OUT" | wc -l | tr -d ' ') files in $OUT"
(cd "$OUT" && ls -l)
