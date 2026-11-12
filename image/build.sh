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
#   image/build.sh --parts "userland rpi5"   build exactly these parts and stop: "userland" is the system
#                                    every board runs, rpi3/rpi4/rpi5 are one board's kernel parts.
#                                    The results stay in image/output/<part>/ (CI builds one part per job
#                                    and combines them with scripts/combine-payload.sh afterwards).
#   image/build.sh --skip-emmd-build   use the existing dist/emmd-linux-arm64 instead of building
#                                    it (no Go needed; the file must exist).
#   image/build.sh --version V       version stamp (default: git describe).
#
# EMM_DL_DIR=/abs/path             keep the download cache (and the compiler cache inside it) in this
#                                    folder instead of the Docker volume. CI uses it to save and restore
#                                    the cache between runs (.github/workflows/image.yml).
#
# A normal run builds the system once (the userland, about as long as a board's kernel) and
# each board's kernel parts, then joins them per board (scripts/combine-payload.sh) and packs the
# SD-card zip. emmd is not part of the image: it goes next to the kernel files as "emmd".
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

# Buildroot version and checksum: image/buildroot.env (may be overridden from the environment).
# shellcheck source=image/buildroot.env
. "$HERE/buildroot.env"
IMAGE_TAG=emm-buildroot:$BR_VERSION
VOL_OUT=emm-buildroot-out
VOL_DL=${EMM_DL_DIR:-emm-buildroot-dl}   # a Docker volume name, or an absolute folder

MODE=build
BOARDS=(rpi3 rpi4 rpi5)
PARTS=()
VERSION=""
SKIP_EMMD=0
COMBINE=1

while [ $# -gt 0 ]; do
  case "$1" in
    --check-only) MODE=check; shift ;;
    --boards)     read -r -a BOARDS <<< "${2:?}"; shift 2 ;;
    --parts)      read -r -a PARTS <<< "${2:?}"; shift 2 ;;
    --version)    VERSION=${2:?}; shift 2 ;;
    --skip-emmd-build) SKIP_EMMD=1; shift ;;
    --clean)
      have docker || die "docker not found"
      log "removing Docker volumes $VOL_OUT and $VOL_DL (build tree and download cache)"
      case "$VOL_DL" in /*) docker volume rm "$VOL_OUT" || true ;; *) docker volume rm "$VOL_OUT" "$VOL_DL" || true ;; esac
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
for b in ${PARTS[@]+"${PARTS[@]}"}; do
  case "$b" in userland|rpi3|rpi4|rpi5) ;; *) die "unknown part '$b' (userland, rpi3, rpi4, rpi5)" ;; esac
done
# The parts to build: the system and the boards' kernel parts, unless --parts names them.
if [ "${#PARTS[@]}" -eq 0 ]; then PARTS=(userland "${BOARDS[@]}"); COMBINE=1; else COMBINE=0; fi
[ -n "$VERSION" ] || VERSION=$(emm_version "$ROOT")

"$HERE/mkconfigs.sh" --check || die "defconfigs are out of date: run image/mkconfigs.sh"

if [ "$MODE" = build ]; then
  # emmd is not in the image; it only goes into the SD-card zip, so only a full run needs it.
  if [ "$COMBINE" = 1 ]; then
    if [ "$SKIP_EMMD" = 0 ]; then
      build_emmd "$ROOT" "$VERSION"
    fi
    [ -f "$ROOT/dist/emmd-linux-arm64" ] || die "dist/emmd-linux-arm64 missing (see scripts/lib.sh build_emmd)"
  fi
  warn "a full build needs 15-30 GB free in Docker's disk image, about 8 GB RAM for Docker, and 1-2 hours per board."
fi

log "building the build container ($IMAGE_TAG)"
docker build -q -t "$IMAGE_TAG" \
  --build-arg "UID=$(id -u)" --build-arg "GID=$(id -g)" \
  -f "$HERE/Dockerfile" "$HERE" >/dev/null

docker volume create "$VOL_OUT" >/dev/null
case "$VOL_DL" in
  /*) mkdir -p "$VOL_DL" ;;
  *)  docker volume create "$VOL_DL" >/dev/null ;;
esac
# Named volumes are created root-owned; hand them to the build user.
docker run --rm --user 0 -v "$VOL_OUT:/out" -v "$VOL_DL:/dl" "$IMAGE_TAG" \
  chown -R "$(id -u):$(id -g)" /out /dl

log "running Buildroot ($MODE) for: ${PARTS[*]}"
docker run --rm -t \
  -v "$ROOT:/work:ro" -v "$VOL_OUT:/out" -v "$VOL_DL:/dl" \
  -e "BR_VERSION=$BR_VERSION" -e "BR_SHA256=$BR_SHA256" \
  "$IMAGE_TAG" /work/image/docker-build.sh "$MODE" "${PARTS[@]}"

if [ "$MODE" = check ]; then
  log "configuration check passed for: ${PARTS[*]}"
  exit 0
fi

# Copy each part's result out of the volume: image/output/<part>/emm-os (the system) or
# image/output/<part>/emm-kernel-<board> (a board's kernel parts).
for b in "${PARTS[@]}"; do
  dest=$HERE/output/$b
  mkdir -p "$dest"
  if [ "$b" = userland ]; then name=emm-os; else name=emm-kernel-$b; fi
  rm -rf "$dest/$name"
  docker run --rm -v "$VOL_OUT:/out:ro" -v "$dest:/dest" "$IMAGE_TAG" \
    cp -a "/out/$b/images/$name" /dest/
done
if [ "$COMBINE" = 0 ]; then
  log "built: ${PARTS[*]} (results in $HERE/output/<part>/)"
  exit 0
fi

# Join the system with each board's kernel parts, then merge the boards into the zip.
payloads=()
for b in "${BOARDS[@]}"; do
  "$ROOT/scripts/combine-payload.sh" --out "$HERE/output/$b" \
    "$HERE/output/userland/emm-os" "$HERE/output/$b/emm-kernel-$b"
  payloads+=("$HERE/output/$b/emm-payload-$b")
done
"$ROOT/scripts/package-sdcard.sh" --version "$VERSION" --out "$ROOT/dist" \
  --app "$ROOT/dist/emmd-linux-arm64" "${payloads[@]}"
