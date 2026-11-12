#!/usr/bin/env bash
# Buildroot post-image hook. Two kinds of build call it (BR2_ROOTFS_POST_SCRIPT_ARGS):
#
#   post-image.sh BINARIES_DIR userland
#       the system every board runs: $BINARIES_DIR/emm-os/rootfs-os.cpio.gz
#   post-image.sh BINARIES_DIR rpi3|rpi4|rpi5
#       one board: $BINARIES_DIR/emm-kernel-<board>/ with its kernel, device trees, GPU firmware
#       and modules-<piN>.cpio.gz (the kernel's modules, laid out for /usr/lib/modules)
#
# scripts/combine-payload.sh later joins the two into the SD-card files of a board:
# rootfs-<piN>.cpio.gz = the system archive followed by the board's modules archive (the Linux
# kernel unpacks concatenated cpio archives as one initramfs), plus config.txt and friends.
set -euo pipefail
# An unmatched glob below must expand to nothing, not stay a literal pattern.
shopt -s nullglob

BINARIES_DIR=${1:?usage: post-image.sh BINARIES_DIR userland|rpi3|rpi4|rpi5}
BOARD=${2:?argument missing (BR2_ROOTFS_POST_SCRIPT_ARGS)}

die() { echo "post-image.sh: $*" >&2; exit 1; }
need() { [ -f "$1" ] || die "expected build output not found: $1"; }

if [ "$BOARD" = userland ]; then
  need "$BINARIES_DIR/rootfs.cpio.gz"
  OS=$BINARIES_DIR/emm-os
  rm -rf "$OS"
  mkdir -p "$OS"
  cp "$BINARIES_DIR/rootfs.cpio.gz" "$OS/rootfs-os.cpio.gz"
  echo "${EMM_OS_HASH:-unknown}" > "$OS/INPUTS"
  echo "post-image.sh: system for all boards:"
  ( cd "$OS" && ls -l )
  exit 0
fi

# ---- one board ---------------------------------------------------------------------------
TARGET_DIR=${TARGET_DIR:?run this from Buildroot (TARGET_DIR is not set)}
PAYLOAD=$BINARIES_DIR/emm-kernel-$BOARD
rm -rf "$PAYLOAD"
mkdir -p "$PAYLOAD"

need "$BINARIES_DIR/Image"

case "$BOARD" in
  rpi3)
    # Pi 3 B/B+/A+ and Zero 2 W. Not kernel8.img: that name is the Pi 4 kernel's on the
    # merged card (config.txt's [pi3]/[pi02] sections name this file).
    cp "$BINARIES_DIR/Image" "$PAYLOAD/kernel_pi3.img"
    PI=pi3
    # No EEPROM bootloader on these: the card must carry bootcode.bin and the GPU firmware.
    for f in bootcode.bin start.elf fixup.dat; do
      need "$BINARIES_DIR/rpi-firmware/$f"
      cp "$BINARIES_DIR/rpi-firmware/$f" "$PAYLOAD/"
    done
    need "$BINARIES_DIR/bcm2710-rpi-3-b.dtb"
    need "$BINARIES_DIR/bcm2710-rpi-zero-2-w.dtb"
    dtbs=("$BINARIES_DIR"/bcm2710-rpi-*.dtb "$BINARIES_DIR"/bcm2837-rpi-*.dtb)
    ;;
  rpi4)
    cp "$BINARIES_DIR/Image" "$PAYLOAD/kernel8.img"
    PI=pi4
    # GPU firmware: the Pi 4 loads these from the card (its EEPROM only holds the
    # first-stage bootloader).
    need "$BINARIES_DIR/rpi-firmware/start4.elf"
    need "$BINARIES_DIR/rpi-firmware/fixup4.dat"
    cp "$BINARIES_DIR/rpi-firmware/start4.elf" "$BINARIES_DIR/rpi-firmware/fixup4.dat" "$PAYLOAD/"
    dtbs=("$BINARIES_DIR"/bcm2711-rpi-*.dtb)
    ;;
  rpi5)
    cp "$BINARIES_DIR/Image" "$PAYLOAD/kernel_2712.img"
    PI=pi5
    need "$BINARIES_DIR/bcm2712-rpi-5-b.dtb"
    need "$BINARIES_DIR/bcm2712-rpi-500.dtb"
    dtbs=("$BINARIES_DIR"/bcm2712*.dtb)
    ;;
  *) die "unknown board '$BOARD' (expected userland, rpi3, rpi4 or rpi5)" ;;
esac

[ -e "${dtbs[0]}" ] || die "no device tree blobs found in $BINARIES_DIR for $BOARD"
cp "${dtbs[@]}" "$PAYLOAD/"

# The kernel's modules (USB audio/MIDI, WiFi, Bluetooth) as a cpio archive of their own, with
# paths under usr/lib/modules (the system has /lib -> usr/lib). Buildroot has run depmod on them.
mods=$TARGET_DIR/usr/lib/modules
[ -d "$mods" ] || mods=$TARGET_DIR/lib/modules
[ -d "$mods" ] || die "no kernel modules in $TARGET_DIR (lib/modules): does the kernel config still build modules?"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/usr/lib"
cp -a "$mods" "$STAGE/usr/lib/modules"
( cd "$STAGE" && find usr -print0 | sort -z | cpio --null -o -H newc -R 0:0 --reproducible --quiet | gzip -9n ) \
  > "$PAYLOAD/modules-$PI.cpio.gz"
[ -s "$PAYLOAD/modules-$PI.cpio.gz" ] || die "empty modules archive"

echo "post-image.sh: kernel parts for $BOARD:"
( cd "$PAYLOAD" && ls -l )
