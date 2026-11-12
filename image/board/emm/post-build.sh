#!/usr/bin/env bash
# Buildroot post-build hook: put EMM's system parts into the target root filesystem.
# Called as: post-build.sh TARGET_DIR userland   (from BR2_ROOTFS_POST_SCRIPT_ARGS)
# Reads from the repository: packaging/ and board/emm/. The daemon itself (emmd) is NOT
# part of the image: it is the file /boot/emmd, started by usr/libexec/emm-launch, so that
# the image depends only on the files hashed by image/inputs-hash.sh and can be reused.
set -euo pipefail

TARGET_DIR=${1:?usage: post-build.sh TARGET_DIR BOARD}
BOARD=${2:-}
EXT=${BR2_EXTERNAL_EMM_PATH:?run this from Buildroot (BR2_EXTERNAL_EMM_PATH is not set)}
REPO=$(cd "$EXT/.." && pwd)

die() { echo "post-build.sh: $*" >&2; exit 1; }

# put MODE SRC DST: install a file, creating parent directories (no GNU-only
# "install -D", so the script also runs on BSD for testing).
put() { mkdir -p "$(dirname "$3")" && install -m "$1" "$2" "$3"; }

PKG=$REPO/packaging
for f in systemd/emmd.service systemd/emm-ssh.service avahi/emm-http.service nm/zz-emm.conf nm/zz-emm-appliance.conf \
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

[ -x "$TARGET_DIR/usr/sbin/dropbear" ] || die "dropbear is not in the target: check BR2_PACKAGE_DROPBEAR in the defconfig"

put 0644 "$EXT/board/emm/os-api" "$TARGET_DIR/etc/emm-os-api"
put 0644 "$PKG/systemd/emmd.service" "$TARGET_DIR/usr/lib/systemd/system/emmd.service"
put 0644 "$PKG/systemd/emm-ssh.service" "$TARGET_DIR/usr/lib/systemd/system/emm-ssh.service"
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

# SSH stays off until the user switches it on in the web UI. Buildroot's own
# dropbear.service must not start it at boot, whatever the systemd presets say:
# take any enabling symlink away, and keep the unit from being pulled in.
rm -f "$TARGET_DIR"/etc/systemd/system/*.wants/dropbear.service
ln -sf /dev/null "$TARGET_DIR/etc/systemd/system/dropbear.service"
# Dropbear's host keys live on the boot partition (emmd points it there).
rm -f "$TARGET_DIR/etc/dropbear"

# Stamp for support and debugging (read it on the console: cat /etc/emm-release). Only values
# that follow from the image's inputs, never a build time or a version: the same inputs give
# the same bytes, which is what lets a release reuse an earlier build.
{
  echo "EMM_OS_API=$(tr -d '\n' < "$EXT/board/emm/os-api")"
  echo "EMM_OS_INPUTS=${EMM_OS_HASH:-unknown}"
} > "$TARGET_DIR/etc/emm-release"

echo "post-build.sh: EMM system installed (inputs ${EMM_OS_HASH:-unknown})"
