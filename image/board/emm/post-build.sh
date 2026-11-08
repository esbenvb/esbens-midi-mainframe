#!/usr/bin/env bash
# Buildroot post-build hook: put EMM into the target root filesystem.
# Called as: post-build.sh TARGET_DIR BOARD   (BOARD from BR2_ROOTFS_POST_SCRIPT_ARGS)
# Reads from the repository: dist/emmd-linux-arm64 and packaging/.
set -euo pipefail

TARGET_DIR=${1:?usage: post-build.sh TARGET_DIR BOARD}
BOARD=${2:-}
EXT=${BR2_EXTERNAL_EMM_PATH:?run this from Buildroot (BR2_EXTERNAL_EMM_PATH is not set)}
REPO=$(cd "$EXT/.." && pwd)

die() { echo "post-build.sh: $*" >&2; exit 1; }

# put MODE SRC DST: install a file, creating parent directories (no GNU-only
# "install -D", so the script also runs on BSD for testing).
put() { mkdir -p "$(dirname "$3")" && install -m "$1" "$2" "$3"; }

BIN=${EMMD_BINARY:-$REPO/dist/emmd-linux-arm64}
[ -f "$BIN" ] || die "$BIN not found. Build the daemon first:
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o dist/emmd-linux-arm64 ./cmd/emmd"

PKG=$REPO/packaging
for f in systemd/emmd.service avahi/emm-http.service nm/zz-emm.conf nm/zz-emm-appliance.conf \
         udev/90-emm-usb-midi.rules emm.txt.example; do
  [ -f "$PKG/$f" ] || die "missing $PKG/$f"
done

# The units we depend on must have been installed by their packages. If one is
# missing, the defconfig and this script have drifted apart: fail the build now,
# not at first boot.
for u in dbus NetworkManager avahi-daemon bluetooth; do
  [ -e "$TARGET_DIR/usr/lib/systemd/system/$u.service" ] \
    || die "$u.service is not in the target: check the package is enabled in the defconfig"
done

put 0755 "$BIN" "$TARGET_DIR/usr/bin/emmd"
put 0644 "$PKG/systemd/emmd.service" "$TARGET_DIR/usr/lib/systemd/system/emmd.service"
put 0644 "$PKG/avahi/emm-http.service" "$TARGET_DIR/etc/avahi/services/emm-http.service"
put 0644 "$PKG/nm/zz-emm.conf" "$TARGET_DIR/etc/NetworkManager/conf.d/zz-emm.conf"
put 0644 "$PKG/nm/zz-emm-appliance.conf" "$TARGET_DIR/etc/NetworkManager/conf.d/zz-emm-appliance.conf"
put 0644 "$PKG/udev/90-emm-usb-midi.rules" "$TARGET_DIR/etc/udev/rules.d/90-emm-usb-midi.rules"

# NetworkManager: profiles are rendered here by emmd at boot (mode 0700 or NM ignores them);
# the dnsmasq conf dir lets emmd add the captive-portal DNS wildcard to the setup AP.
install -d -m 0700 "$TARGET_DIR/etc/NetworkManager/system-connections"
install -d -m 0755 "$TARGET_DIR/etc/NetworkManager/dnsmasq-shared.d"
install -d -m 0755 "$TARGET_DIR/etc/avahi/services"

# NetworkManager writes /etc/resolv.conf itself (rc-manager=file); remove the
# skeleton's placeholder symlink so a regular file can be created.
rm -f "$TARGET_DIR/etc/resolv.conf"

# Enable the services. emmd pulls the other three in through Wants=, but enabling
# them directly keeps them up if emmd is ever stopped.
wants="$TARGET_DIR/etc/systemd/system/multi-user.target.wants"
mkdir -p "$wants"
for u in emmd NetworkManager avahi-daemon bluetooth; do
  ln -sf "/usr/lib/systemd/system/$u.service" "$wants/$u.service"
done

# Release stamp for support and debugging (read it on the console: cat /etc/emm-release).
# Nothing reads it yet: the web UI's system page shows the version baked into emmd.
version=unknown
[ -f "$REPO/dist/VERSION" ] && version=$(cat "$REPO/dist/VERSION")
{
  echo "EMM_VERSION=$version"
  echo "EMM_BOARD=$BOARD"
} > "$TARGET_DIR/etc/emm-release"

echo "post-build.sh: EMM $version installed for ${BOARD:-?}"
