#!/usr/bin/env bash
# Runs INSIDE the build container (started by image/build.sh).
#   docker-build.sh check|build PART...      PART: userland (the system), rpi3, rpi4 or rpi5 (kernel parts)
# Mounts: /work = the repository (read-only), /out = build output, /dl = download cache.
set -euo pipefail

MODE=${1:?usage: docker-build.sh check|build PART...}; shift
BOARDS=("$@")   # the parts to build; "board" below means any of them
[ "${#BOARDS[@]}" -gt 0 ] || { echo "no parts given" >&2; exit 2; }
# BR_VERSION and BR_SHA256 come from image/buildroot.env (passed in by build.sh).
: "${BR_VERSION:?}" "${BR_SHA256:?}"

OUT=/out
DL=/dl
EXT=/work/image
export BR2_DL_DIR=$DL
export BR2_CCACHE_DIR=$DL/ccache   # compiler cache, kept in the download volume (see BR2_CCACHE)

tarball=$DL/buildroot-$BR_VERSION.tar.xz
if [ ! -f "$tarball" ]; then
  echo "==> downloading Buildroot $BR_VERSION"
  wget -q -O "$tarball.part" "https://buildroot.org/downloads/buildroot-$BR_VERSION.tar.xz"
  mv "$tarball.part" "$tarball"
fi
echo "$BR_SHA256  $tarball" | sha256sum -c -
BR=$OUT/buildroot-$BR_VERSION
if [ ! -d "$BR" ]; then
  echo "==> unpacking Buildroot"
  tar -C "$OUT" -xf "$tarball"
fi

for board in "${BOARDS[@]}"; do
  O=$OUT/$board
  mkdir -p "$O"
  echo "==> [$board] configuring"
  # PIPESTATUS via pipefail: a failing make must fail the script even though tee succeeds.
  make -C "$BR" O="$O" BR2_EXTERNAL="$EXT" "emm_${board}_defconfig" 2>&1 | tee "$O/defconfig.log"
  "$EXT/check-defconfig.sh" "$EXT/configs/emm_${board}_defconfig" "$O/.config" "$O/defconfig.log"

  if [ "$MODE" = check ]; then
    echo "==> [$board] configuration check passed (no build requested)"
    continue
  fi

  if [ "$board" = userland ]; then
    # The system: no kernel. EMM_OS_HASH is stamped into /etc/emm-release by post-build.sh.
    EMM_OS_HASH=$("$EXT/inputs-hash.sh" userland)
    export EMM_OS_HASH
    echo "==> [$board] building the system, inputs $EMM_OS_HASH (this takes a while)"
    make -C "$BR" O="$O" -j"$(nproc)"
    make -C "$BR" O="$O" ccache-stats 2>&1 | tail -n 20 || true
    [ -f "$O/images/emm-os/rootfs-os.cpio.gz" ] || { echo "system archive missing: $O/images/emm-os/rootfs-os.cpio.gz" >&2; exit 1; }
    echo "==> [$board] done"
    continue
  fi

  # Buildroot applies image/patches/linux/*.patch only when it extracts the kernel, so adding or
  # changing a patch does not touch an existing build tree. Re-extract when the patch set changed.
  psum=$(cat "$EXT"/patches/linux/*.patch 2>/dev/null | sha256sum | cut -d' ' -f1)
  if [ -d "$O/build/linux-custom" ] && [ "$(cat "$O/.emm-linux-patches" 2>/dev/null)" != "$psum" ]; then
    echo "==> [$board] kernel patches changed: re-extracting and re-patching the kernel"
    make -C "$BR" O="$O" linux-dirclean
  fi
  echo "$psum" > "$O/.emm-linux-patches"

  echo "==> [$board] configuring the kernel (downloads it; checks the options we rely on)"
  make -C "$BR" O="$O" linux-configure
  kcfg=$(find "$O/build" -maxdepth 2 -name .config -path '*/linux-*' ! -path '*linux-headers*' ! -path '*linux-tools*' | head -n1)
  [ -n "$kcfg" ] || { echo "kernel .config not found under $O/build" >&2; exit 1; }
  "$EXT/check-kernel-config.sh" "$kcfg" "$EXT/board/emm/kernel-required.txt" "$EXT/board/emm/kernel-required.$board.txt"

  echo "==> [$board] building (this takes a while)"
  make -C "$BR" O="$O" -j"$(nproc)"
  # How much of the build the compiler cache answered (a warm cache is what makes reruns fast).
  make -C "$BR" O="$O" ccache-stats 2>&1 | tail -n 20 || true
  [ -d "$O/images/emm-kernel-$board" ] || { echo "kernel parts missing: $O/images/emm-kernel-$board" >&2; exit 1; }
  echo "==> [$board] done"
done
