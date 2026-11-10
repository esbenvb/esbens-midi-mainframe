#!/usr/bin/env bash
# Copy a build folder to the boot partition of a running EMM device over SSH.
#
#   scripts/push-boot.sh [--dry-run] [--reboot] [--dest DIR] [--ssh-opts "OPTS"] SOURCE root@host
#   scripts/push-boot.sh [--dry-run] [--reboot] [--dest DIR] [--ssh-opts "OPTS"] --auto root@host
#
# SOURCE is a folder (image/output/<board>/emm-payload-<board>, an unpacked SD-card
# zip, ...) or just a board name (rpi3, rpi4, rpi5), which means the payload that
# image/build.sh left in image/output/. With --auto there is no SOURCE: the script
# asks the device which board it is (the SoC in its device tree) and uses that
# board's payload. rpi3 = Pi 3 B/B+/A+ and Zero 2 W, rpi4 = Pi 4, 400, CM4, CM4S,
# rpi5 = Pi 5, 500/500+, CM5 and CM5 Lite. A board it does not recognise is an error.
#
# Every regular file directly in the folder goes to DIR on the device (default /boot,
# the FAT32 boot partition). Files that are already identical there (SHA-256) are
# skipped. Nothing is deleted on the device, so emm.txt and the emm/ state folder
# stay as they are.
#
# The image has Dropbear but no sftp-server, scp or rsync, so each file is streamed
# through "cat". It is written under a temporary name and renamed once its size matches:
# a dropped connection never leaves a half-written kernel behind. SSH must be switched
# on in the web UI first (System tab); the device logs in as root. The connection
# is opened once and shared, so a password is asked for once.
# A new kernel or rootfs only takes effect after a reboot (--reboot does that).
# --dry-run lists what would be copied; with --auto it still connects to read the board.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$SCRIPT_DIR/lib.sh"

SOURCE=""
TARGET=""
DEST=/boot
REBOOT=0
AUTO=0
SSH_OPTS=""

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)  DRY_RUN=1; shift ;;
    --reboot)   REBOOT=1; shift ;;
    --auto)     AUTO=1; shift ;;
    --dest)     DEST=${2:?--dest needs a directory}; shift 2 ;;
    --ssh-opts) SSH_OPTS=${2:?--ssh-opts needs a value}; shift 2 ;;
    -h|--help)  print_usage_comment "$0"; exit 0 ;;
    -*)         die "unknown option: $1" ;;
    *)
      if [ -z "$SOURCE" ]; then SOURCE=$1
      elif [ -z "$TARGET" ]; then TARGET=$1
      else die "too many arguments: $1"
      fi
      shift ;;
  esac
done
if [ "$AUTO" = 1 ]; then
  # The single argument is the device.
  [ -z "$TARGET" ] || die "--auto picks the folder itself: give only the device, not '$SOURCE'"
  TARGET=$SOURCE
  SOURCE=""
fi
if [ -z "$TARGET" ] || { [ "$AUTO" = 0 ] && [ -z "$SOURCE" ]; }; then
  print_usage_comment "$0" >&2
  exit 2
fi
have ssh || die "ssh not found"

case "$DEST" in /*) ;; *) die "--dest must be an absolute path on the device" ;; esac
case "$DEST" in *[!A-Za-z0-9_./-]*) die "--dest may only contain letters, digits and _ . / -" ;; esac

if have sha256sum; then local_sha() { sha256sum "$1" | cut -d' ' -f1; }
elif have shasum;   then local_sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else die "need sha256sum or shasum"
fi

# One shared connection: with password login this asks once, not once per file.
# The control socket sits in a short temp path (macOS limits socket paths to ~100 characters).
CTL_DIR=$(mktemp -d /tmp/emm-push.XXXXXX)
# shellcheck disable=SC2206  # intentional word splitting of user-supplied options
SSH_BASE=(-o ControlMaster=auto -o "ControlPath=$CTL_DIR/c" -o ControlPersist=120
          -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 $SSH_OPTS "$TARGET")
# SSH never reads this script's stdin (-n), so a caller that loops over hosts keeps its
# input; SSH_IN is for the one call that streams a file into the device.
SSH=(ssh -n "${SSH_BASE[@]}")
SSH_IN=(ssh "${SSH_BASE[@]}")
cleanup() {
  ssh -o "ControlPath=$CTL_DIR/c" -O exit "$TARGET" >/dev/null 2>&1 || true
  rm -rf "$CTL_DIR"
}
trap cleanup EXIT

CONNECTED=0
connect() {
  [ "$CONNECTED" = 0 ] || return 0
  log "connecting to $TARGET"
  "${SSH[@]}" "test -d '$DEST' && test -w '$DEST'" \
    || die "cannot reach $TARGET, or $DEST is missing or not writable there (is SSH switched on in the web UI?)"
  CONNECTED=1
}

# detect_board: print the board name (rpi3, rpi4 or rpi5) of the device, judged by
# the SoC in its device tree (read-only; the same /proc/device-tree/model that the
# web UI shows). A Pi 3 B, 3 B+, 3 A+ and Zero 2 W all report brcm,bcm2837; the
# Pi 4, 400 and CM4 brcm,bcm2711; the Pi 5, 500 and CM5 brcm,bcm2712.
detect_board() {
  local compat model
  # The device tree strings are NUL-separated, which a shell variable cannot hold.
  compat=$("${SSH[@]}" "cat /proc/device-tree/compatible" 2>/dev/null | tr '\0' ' ') || true
  model=$("${SSH[@]}" "cat /proc/device-tree/model" 2>/dev/null | tr -d '\0') || true
  [ -n "$compat" ] || die "could not read /proc/device-tree/compatible on $TARGET: is this a Raspberry Pi running the EMM image? Name the board or the folder instead of --auto."
  log "device reports: ${model:-unknown model} ($compat)"
  case " $compat" in
    *" brcm,bcm2712 "*) echo rpi5 ;;
    *" brcm,bcm2711 "*) echo rpi4 ;;
    *" brcm,bcm2837 "*|*" brcm,bcm2710 "*) echo rpi3 ;;
    *) die "no payload for this board (${model:-unknown}): only the Pi 3 family, 4/400/CM4 and 5/500/CM5 have one. Name the folder instead of --auto." ;;
  esac
}

if [ "$AUTO" = 1 ]; then
  connect
  SOURCE=$(detect_board)
  log "board: $SOURCE"
fi

case "$SOURCE" in
  rpi3|rpi4|rpi5)
    BOARD=$SOURCE
    SOURCE=$ROOT/image/output/$BOARD/emm-payload-$BOARD
    [ -d "$SOURCE" ] || die "no $BOARD build yet ($SOURCE is missing): run image/build.sh --boards $BOARD"
    ;;
esac
[ -d "$SOURCE" ] || die "not a folder: $SOURCE"

FILES=()
for f in "$SOURCE"/*; do
  [ -f "$f" ] || continue
  case "${f##*/}" in *\'*) die "file name with a quote is not supported: ${f##*/}" ;; esac
  FILES+=("$f")
done
[ "${#FILES[@]}" -gt 0 ] || die "no files in $SOURCE"

if [ "$DRY_RUN" = 1 ]; then
  log "(dry run) would copy from $SOURCE to $TARGET:$DEST:"
  for f in "${FILES[@]}"; do printf '    %s\n' "${f##*/}" >&2; done
  [ "$REBOOT" = 0 ] || log "(dry run) would reboot the device afterwards"
  exit 0
fi

connect

copied=0
for f in "${FILES[@]}"; do
  name=${f##*/}
  want=$(local_sha "$f")
  have_sha=$("${SSH[@]}" "sha256sum '$DEST/$name' 2>/dev/null" | cut -d' ' -f1 || true)
  if [ "$have_sha" = "$want" ]; then
    printf '  %-34s unchanged\n' "$name" >&2
    continue
  fi
  printf '  %-34s %s ... ' "$name" "$(du -h "$f" | cut -f1)" >&2
  size=$(wc -c < "$f" | tr -d ' ')
  # Only rename when every byte arrived: a connection that dies mid-stream can still end cat normally.
  "${SSH_IN[@]}" "cat > '$DEST/.$name.new' && [ \$(wc -c < '$DEST/.$name.new') -eq $size ] && mv -f '$DEST/.$name.new' '$DEST/$name'" < "$f" \
    || { printf '\n' >&2; "${SSH[@]}" "rm -f '$DEST/.$name.new'" || true; die "copying $name failed"; }
  got=$("${SSH[@]}" "sha256sum '$DEST/$name'" | cut -d' ' -f1)
  [ "$got" = "$want" ] || die "$name differs on the device after copying (checksum $got, expected $want)"
  printf 'ok\n' >&2
  copied=$((copied + 1))
done

"${SSH[@]}" "sync"
log "$copied file(s) copied, $((${#FILES[@]} - copied)) unchanged"

if [ "$REBOOT" = 1 ]; then
  log "rebooting $TARGET"
  # The connection drops as the device goes down; that is not an error.
  "${SSH[@]}" "reboot" || true
else
  log "reboot the device (or run again with --reboot) for a new kernel or rootfs to take effect"
fi
