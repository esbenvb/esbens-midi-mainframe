#!/usr/bin/env bash
# Buildroot post-image hook: collect everything the SD card needs for ONE board
# into $BINARIES_DIR/emm-payload-<board>/ (file names as the card will have them).
# scripts/package-sdcard.sh later merges the Pi 4 and Pi 5 payloads and zips them.
# Called as: post-image.sh BINARIES_DIR BOARD   (BOARD from BR2_ROOTFS_POST_SCRIPT_ARGS)
set -euo pipefail

BINARIES_DIR=${1:?usage: post-image.sh BINARIES_DIR BOARD}
BOARD=${2:?board argument missing (BR2_ROOTFS_POST_SCRIPT_ARGS)}
EXT=${BR2_EXTERNAL_EMM_PATH:?run this from Buildroot (BR2_EXTERNAL_EMM_PATH is not set)}
REPO=$(cd "$EXT/.." && pwd)
BOARD_DIR=$EXT/board/emm

die() { echo "post-image.sh: $*" >&2; exit 1; }
need() { [ -f "$1" ] || die "expected build output not found: $1"; }

PAYLOAD=$BINARIES_DIR/emm-payload-$BOARD
rm -rf "$PAYLOAD"
mkdir -p "$PAYLOAD"

# ---- files common to both boards (identical in both payloads, merged by name) ----
cp "$BOARD_DIR/config.txt" "$PAYLOAD/config.txt"
cp "$BOARD_DIR/cmdline.txt" "$PAYLOAD/cmdline.txt"
cp "$BOARD_DIR/README-FIRST.txt" "$PAYLOAD/README-FIRST.txt"
cp "$REPO/packaging/emm.txt.example" "$PAYLOAD/emm.txt.example"

# ---- board specific ----
need "$BINARIES_DIR/Image"
need "$BINARIES_DIR/rootfs.cpio.gz"

case "$BOARD" in
  rpi4)
    cp "$BINARIES_DIR/Image" "$PAYLOAD/kernel8.img"
    cp "$BINARIES_DIR/rootfs.cpio.gz" "$PAYLOAD/rootfs-pi4.cpio.gz"
    # GPU firmware: the Pi 4 loads these from the card (its EEPROM only holds the
    # first-stage bootloader).
    need "$BINARIES_DIR/rpi-firmware/start4.elf"
    need "$BINARIES_DIR/rpi-firmware/fixup4.dat"
    cp "$BINARIES_DIR/rpi-firmware/start4.elf" "$BINARIES_DIR/rpi-firmware/fixup4.dat" "$PAYLOAD/"
    dtbs=("$BINARIES_DIR"/bcm2711-rpi-*.dtb)
    ;;
  rpi5)
    cp "$BINARIES_DIR/Image" "$PAYLOAD/kernel_2712.img"
    cp "$BINARIES_DIR/rootfs.cpio.gz" "$PAYLOAD/rootfs-pi5.cpio.gz"
    dtbs=("$BINARIES_DIR"/bcm2712*.dtb)
    ;;
  *) die "unknown board '$BOARD' (expected rpi4 or rpi5)" ;;
esac

[ -e "${dtbs[0]}" ] || die "no device tree blobs found in $BINARIES_DIR for $BOARD"
cp "${dtbs[@]}" "$PAYLOAD/"

echo "post-image.sh: payload for $BOARD:"
( cd "$PAYLOAD" && ls -l )
