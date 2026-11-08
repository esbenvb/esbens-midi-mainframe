#!/usr/bin/env bash
# Build emmd for arm64 and install it on a Raspberry Pi over SSH.
#
#   scripts/deploy.sh [--dry-run] [--skip-build] user@host [-- <install-on-pi.sh flags>]
#
# The Pi needs 64-bit Raspberry Pi OS Lite (Bookworm/Trixie), SSH, and sudo.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$SCRIPT_DIR/lib.sh"

usage() {
  cat <<'EOF'
Usage: deploy.sh [options] user@host [-- install flags...]

  --skip-build        use the existing dist/emmd-linux-arm64
  --dry-run           print the commands instead of running them
  --ssh-opts "OPTS"   extra options for ssh (e.g. "-p 2222 -i ~/.ssh/pi")
  --uninstall         run the installer's --uninstall on the Pi instead
  -h, --help          this text

Everything after "--" is passed to install-on-pi.sh, e.g.
  scripts/deploy.sh pi@emm.local -- --no-hostname
  scripts/deploy.sh --uninstall pi@emm.local -- --purge
(--purge-network would also delete the WiFi/Ethernet profiles: see install-on-pi.sh --help.)
EOF
}

SKIP_BUILD=0
SSH_OPTS=""
TARGET=""
UNINSTALL=0
EXTRA=()

while [ $# -gt 0 ]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1; shift ;;
    --dry-run)    DRY_RUN=1; shift ;;
    --ssh-opts)   SSH_OPTS=${2:?--ssh-opts needs a value}; shift 2 ;;
    --uninstall)  UNINSTALL=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    --)           shift; EXTRA=("$@"); break ;;
    -*)           usage >&2; die "unknown option: $1" ;;
    *)            [ -z "$TARGET" ] || die "only one target host, got '$TARGET' and '$1'"; TARGET=$1; shift ;;
  esac
done
[ -n "$TARGET" ] || { usage >&2; exit 2; }
have ssh || die "ssh not found"
have tar || die "tar not found"

# shellcheck disable=SC2206  # intentional word splitting of user-supplied options
SSH=(ssh -o ServerAliveInterval=15 -o ServerAliveCountMax=4 $SSH_OPTS)

if [ "$UNINSTALL" = 0 ] && [ "$SKIP_BUILD" = 0 ]; then
  if [ "$DRY_RUN" = 1 ]; then
    log "(dry run) would build: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/emmd-linux-arm64 ./cmd/emmd"
  else
    build_emmd "$ROOT"
  fi
fi
if [ "$UNINSTALL" = 0 ] && [ "$DRY_RUN" != 1 ]; then
  [ -f "$ROOT/dist/emmd-linux-arm64" ] || die "dist/emmd-linux-arm64 is missing (drop --skip-build)"
fi

# Bundle = the pieces the installer needs, in the same layout as the repo, so the
# installer finds its files exactly as it does in a checkout.
FILES=(scripts/install-on-pi.sh scripts/lib.sh packaging)
[ "$UNINSTALL" = 1 ] || FILES+=(dist/emmd-linux-arm64)

# The bundle is unpacked on the Pi into a directory made by the Pi's own mktemp -d
# (mode 0700, random name, fails if it exists), never into a name that can be
# guessed or pre-created by another user: the installer in it runs as root through
# sudo. The directory is created and filled in one ssh call, which prints its path.
# (The commands are wrapped in sh -c so that they work whatever the Pi user's login
# shell is.)
STAGE_CMD='umask 077; d=$(mktemp -d /tmp/emm-install.XXXXXX) || exit 1; if tar -xf - -C "$d"; then printf "%s\n" "$d"; else rm -rf "$d"; exit 1; fi'
# Arguments for the installer, shell-quoted for the remote side. (Built as a
# string: empty arrays are awkward under "set -u" in the bash 3.2 that macOS ships.)
quoted=""
if [ "$UNINSTALL" = 1 ]; then quoted+=" --uninstall"; fi
for a in ${EXTRA[@]+"${EXTRA[@]}"}; do quoted+=$(printf ' %q' "$a"); done

log "uploading bundle to $TARGET"
if [ "$DRY_RUN" = 1 ]; then
  REMOTE_DIR="/tmp/emm-install.XXXXXX"   # placeholder: the real name is chosen by mktemp on the Pi
  printf '+ tar -C %q -cf - %s | %s %q %q\n' "$ROOT" "${FILES[*]}" "${SSH[*]}" "$TARGET" "sh -c '$STAGE_CMD'" >&2
else
  # COPYFILE_DISABLE stops macOS tar from adding ._* AppleDouble files.
  REMOTE_DIR=$(COPYFILE_DISABLE=1 tar -C "$ROOT" -cf - "${FILES[@]}" \
    | "${SSH[@]}" "$TARGET" "sh -c '$STAGE_CMD'" | tail -n 1) \
    || die "uploading the bundle to $TARGET failed"
  # What the Pi printed goes into a command that runs there as root: accept only the
  # shape mktemp gives, /tmp/emm-install. plus ASCII letters and digits.
  suffix=${REMOTE_DIR#/tmp/emm-install.}
  if [ "$suffix" = "$REMOTE_DIR" ] || [ -z "$suffix" ] \
     || [ -n "$(printf '%s' "$suffix" | LC_ALL=C tr -d 'A-Za-z0-9')" ]; then
    die "unexpected staging directory from $TARGET: $(printf '%q' "$REMOTE_DIR")"
  fi
  log "bundle is in $TARGET:$REMOTE_DIR"
fi

log "running the installer on $TARGET (sudo may ask for the password)"
# -t so sudo can prompt; the temp dir is removed whether or not the install worked.
REMOTE_CMD="sudo bash $REMOTE_DIR/scripts/install-on-pi.sh$quoted; rc=\$?; rm -rf $REMOTE_DIR; exit \$rc"
if [ "$DRY_RUN" = 1 ]; then
  printf '+ %s -t %q %q\n' "${SSH[*]}" "$TARGET" "$REMOTE_CMD" >&2
else
  rc=0
  "${SSH[@]}" -t "$TARGET" "$REMOTE_CMD" || rc=$?
  if [ "$rc" = 255 ]; then
    # ssh itself failed or the connection dropped: the remote "rm -rf" may not have run.
    warn "the connection to $TARGET failed or dropped; the staging directory $REMOTE_DIR (private, mode 0700) may still be there. Remove it with: ssh $TARGET rm -rf $REMOTE_DIR"
  fi
  exit "$rc"
fi
