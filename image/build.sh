#!/usr/bin/env bash
# Build the EMM appliance image inside Docker and package the SD-card zip.
#
#   image/build.sh --check-only      config-only validation of all boards. Downloads Buildroot
#                                    (~6 MB), runs "make emm_*_defconfig" and checks that every
#                                    setting was accepted. Minutes, under 1 GB. Needs only Docker.
#                                    DO THIS FIRST.
#   image/build.sh                   the real build: ALL boards, 1-2 hours each, 15-30 GB of disk
#                                    in Docker's disk image, and ~8 GB RAM for Docker. It first
#                                    cross-compiles emmd on THIS machine, so it needs Go too.
#   image/build.sh --boards rpi5     only one board (a quoted list for several: --boards "rpi4 rpi5").
#                                    rpi3 = Pi 3 B/B+/A+ and Zero 2 W; rpi4 = Pi 4, 400, CM4, CM4S;
#                                    rpi5 = Pi 5, 500/500+, CM5 and CM5 Lite.
#   image/build.sh --skip-emmd-build   use the existing dist/emmd-linux-arm64 instead of building
#                                    it (no Go needed; the file must exist).
#   image/build.sh --version V       version stamp (default: git describe).
#
# Output: dist/emm-sdcard-<version>.zip. The build tree lives in Docker volumes
# (fast on macOS), not in the repository; "image/build.sh --clean" deletes them.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$ROOT/scripts/lib.sh"

# A build runs for hours: keep the Mac awake (idle and, on AC power, system sleep) while it runs.
if [ -z "${EMM_CAFFEINATED:-}" ] && have caffeinate; then
  EMM_CAFFEINATED=1 exec caffeinate -is bash "${BASH_SOURCE[0]}" "$@"
fi

# Buildroot LTS pinned here. The SHA-256 comes from the signed release announcement
# https://buildroot.org/downloads/buildroot-2025.02.19.tar.xz.sign
BR_VERSION=${BR_VERSION:-2025.02.19}
BR_SHA256=${BR_SHA256:-e5e58bff1da91f022f60cefdfe280140c08023cd0aeec1e5f6f9da0ea866d69c}
IMAGE_TAG=emm-buildroot:$BR_VERSION
VOL_OUT=emm-buildroot-out
VOL_DL=emm-buildroot-dl

MODE=build
BOARDS=(rpi3 rpi4 rpi5)
VERSION=""
SKIP_EMMD=0

while [ $# -gt 0 ]; do
  case "$1" in
    --check-only) MODE=check; shift ;;
    --boards)     read -r -a BOARDS <<< "${2:?}"; shift 2 ;;
    --version)    VERSION=${2:?}; shift 2 ;;
    --skip-emmd-build) SKIP_EMMD=1; shift ;;
    --clean)
      have docker || die "docker not found"
      log "removing Docker volumes $VOL_OUT and $VOL_DL (build tree and download cache)"
      docker volume rm "$VOL_OUT" "$VOL_DL" || true
      exit 0 ;;
    -h|--help) print_usage_comment "$0"; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

have docker || die "Docker is required (https://docs.docker.com/get-docker/)"
docker info >/dev/null 2>&1 || die "the Docker daemon is not running"
for b in "${BOARDS[@]}"; do
  case "$b" in rpi3|rpi4|rpi5) ;; *) die "unknown board '$b' (rpi3, rpi4, rpi5)" ;; esac
done
[ -n "$VERSION" ] || VERSION=$(emm_version "$ROOT")

"$HERE/mkconfigs.sh" --check || die "defconfigs are out of date: run image/mkconfigs.sh"

if [ "$MODE" = build ]; then
  if [ "$SKIP_EMMD" = 0 ]; then
    build_emmd "$ROOT" "$VERSION"
  fi
  [ -f "$ROOT/dist/emmd-linux-arm64" ] || die "dist/emmd-linux-arm64 missing (see scripts/lib.sh build_emmd)"
  warn "a full build needs 15-30 GB free in Docker's disk image, about 8 GB RAM for Docker, and 1-2 hours per board."
fi

log "building the build container ($IMAGE_TAG)"
docker build -q -t "$IMAGE_TAG" \
  --build-arg "UID=$(id -u)" --build-arg "GID=$(id -g)" \
  -f "$HERE/Dockerfile" "$HERE" >/dev/null

docker volume create "$VOL_OUT" >/dev/null
docker volume create "$VOL_DL" >/dev/null
# Named volumes are created root-owned; hand them to the build user.
docker run --rm --user 0 -v "$VOL_OUT:/out" -v "$VOL_DL:/dl" "$IMAGE_TAG" \
  chown -R "$(id -u):$(id -g)" /out /dl

log "running Buildroot ($MODE) for: ${BOARDS[*]}"
docker run --rm -t \
  -v "$ROOT:/work:ro" -v "$VOL_OUT:/out" -v "$VOL_DL:/dl" \
  -e "BR_VERSION=$BR_VERSION" -e "BR_SHA256=$BR_SHA256" \
  "$IMAGE_TAG" /work/image/docker-build.sh "$MODE" "${BOARDS[@]}"

if [ "$MODE" = check ]; then
  log "configuration check passed for: ${BOARDS[*]}"
  exit 0
fi

# Copy each board's payload out of the volume and merge them into the zip.
payloads=()
for b in "${BOARDS[@]}"; do
  dest=$HERE/output/$b
  mkdir -p "$dest"
  rm -rf "$dest/emm-payload-$b"
  docker run --rm -v "$VOL_OUT:/out:ro" -v "$dest:/dest" "$IMAGE_TAG" \
    cp -a "/out/$b/images/emm-payload-$b" /dest/
  payloads+=("$dest/emm-payload-$b")
done
"$ROOT/scripts/package-sdcard.sh" --version "$VERSION" --out "$ROOT/dist" "${payloads[@]}"
