#!/usr/bin/env bash
# Install (or remove) Esben's MIDI Mainframe on Raspberry Pi OS Lite.
# Run ON the Pi, as root. Safe to run again: it never overwrites emm.txt or
# state.json / system.json, and replaces only files it owns.
#
#   sudo scripts/install-on-pi.sh [--binary PATH]
#   sudo scripts/install-on-pi.sh --uninstall [--purge] [--purge-network]
#
# Needs a 64-bit Raspberry Pi OS (Bookworm or Trixie) with NetworkManager,
# which is the default network stack on both. A 32-bit system is not supported;
# it can be tried by passing a binary built for it with --binary.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$SCRIPT_DIR/lib.sh"

usage() {
  cat <<'EOF'
Usage: install-on-pi.sh [options]

  --binary PATH           emmd binary to install (default: ./dist/emmd-linux-arm64,
                          ./emmd, or the one next to this script's bundle). A bare
                          file name means a file in the current directory. Required
                          on a 32-bit system, which is not supported (untested).
  --no-start              install and enable, but do not (re)start emmd
  --no-hostname           never touch the hostname
  --force-network-switch  on older images that use dhcpcd, disable it and switch
                          to NetworkManager (may drop your SSH session)
  --uninstall             remove EMM (keeps settings and the WiFi/Ethernet profiles;
                          see --purge and --purge-network)
  --purge                 with --uninstall: also delete state.json and system.json, /etc/default/emmd
                          and emm.txt. The emm-wifi / emm-eth NetworkManager profiles
                          are kept, so the Pi stays reachable.
  --purge-network         with --uninstall: also delete the emm-wifi / emm-eth
                          NetworkManager profiles, i.e. the WiFi and Ethernet settings
                          made through EMM (they hold the WiFi password). DANGEROUS over
                          SSH: after the next reboot the Pi has no saved network from
                          EMM and no setup network, and needs a cable or a keyboard.
  --dry-run               print what would be done
  -h, --help              this text
EOF
}

ORIG_ARGS=("$@")
BINARY=""
UNINSTALL=0
PURGE=0
PURGE_NET=0
NO_START=0
NO_HOSTNAME=0
FORCE_NET=0

while [ $# -gt 0 ]; do
  case "$1" in
    --binary)                BINARY=${2:?--binary needs a path}; shift 2 ;;
    --no-start)              NO_START=1; shift ;;
    --no-hostname)           NO_HOSTNAME=1; shift ;;
    --force-network-switch)  FORCE_NET=1; shift ;;
    --uninstall)             UNINSTALL=1; shift ;;
    --purge)                 PURGE=1; shift ;;
    --purge-network)         PURGE_NET=1; shift ;;
    --dry-run)               DRY_RUN=1; shift ;;
    -h|--help)               usage; exit 0 ;;
    *) usage >&2; die "unknown option: $1" ;;
  esac
done

# ---- preflight --------------------------------------------------------------

[ "$(uname -s)" = Linux ] || die "this installer is for Linux (Raspberry Pi OS); you are on $(uname -s)"
if [ "$DRY_RUN" != 1 ]; then
  [ "$(id -u)" -eq 0 ] || die "run as root: sudo $0 ${ORIG_ARGS[*]-}"
fi
have systemctl || die "systemctl not found: EMM needs a systemd-based Raspberry Pi OS"
if [ "$UNINSTALL" != 1 ] && [ "$PURGE$PURGE_NET" != 00 ]; then
  warn "--purge and --purge-network only apply together with --uninstall; ignored."
fi

# The systemctl binary also exists in chroots and containers, where it refuses to
# work ("System has not been booted with systemd as init system (PID 1)"). Detect
# that up front and skip everything that needs a running systemd, instead of
# aborting half-way with the files copied and nothing enabled.
HAVE_SYSTEMD=0
if [ -d /run/systemd/system ]; then
  HAVE_SYSTEMD=1
elif [ "$DRY_RUN" = 1 ]; then
  # A dry run shows the plan for a booted Pi, whatever machine it is run on.
  HAVE_SYSTEMD=1
  log "(dry run) systemd is not running on this machine; a real run here would only copy files. Showing the plan for a booted Pi."
elif [ "$UNINSTALL" = 1 ]; then
  warn "systemd is not running here (chroot or container?): removing the files, but not stopping or disabling anything."
else
  warn "systemd is not running here (chroot or container?): the files will be installed, but nothing is reloaded, enabled or started and the hostname is not touched. Run this installer again on the booted Pi."
fi
if [ "$FORCE_NET" = 1 ] && [ "$HAVE_SYSTEMD" != 1 ]; then
  die "--force-network-switch needs a running systemd"
fi

# EMM_BOOT_DIR overrides detection (used by tests; also handy for unusual layouts).
if [ -n "${EMM_BOOT_DIR:-}" ]; then
  BOOT=$EMM_BOOT_DIR
elif [ -f /boot/firmware/config.txt ]; then
  BOOT=/boot/firmware
elif [ -f /boot/config.txt ]; then
  BOOT=/boot
else
  BOOT=/boot/firmware
  [ "$UNINSTALL" = 1 ] || die "cannot find the Raspberry Pi boot partition (no config.txt in /boot/firmware or /boot). Is this Raspberry Pi OS?"
fi

# ---- uninstall --------------------------------------------------------------

uninstall() {
  if [ "$HAVE_SYSTEMD" = 1 ]; then
    log "stopping and disabling emmd"
    run_quiet systemctl disable --now emmd.service || true
  fi
  log "removing installed files"
  run rm -f /usr/bin/emmd /usr/bin/emmd.new \
            /etc/systemd/system/emmd.service \
            /etc/NetworkManager/conf.d/zz-emm.conf \
            /etc/udev/rules.d/90-emm-usb-midi.rules \
            "$BOOT/emm.txt.example"
  # emmd writes one Avahi service file per network MIDI port; all are emm-*.service.
  run rm -f /etc/avahi/services/emm-*.service
  # The setup access point must not outlive EMM. The WiFi/Ethernet profiles are kept
  # (so the Pi stays reachable) unless --purge-network.
  run rm -f /etc/NetworkManager/system-connections/emm-setup-ap.nmconnection \
            /etc/NetworkManager/dnsmasq-shared.d/emm-captive.conf
  if [ "$HAVE_SYSTEMD" = 1 ]; then
    run systemctl daemon-reload
  fi
  run_quiet udevadm control --reload-rules || true
  if [ "$PURGE" = 1 ]; then
    log "purging settings"
    run rm -rf /var/lib/emm
    run rm -f /etc/default/emmd "$BOOT/emm.txt" "$BOOT/reset-network"
  else
    log "kept: /var/lib/emm (web UI settings), /etc/default/emmd, $BOOT/emm.txt  (use --purge to delete)"
  fi
  if [ "$PURGE_NET" = 1 ]; then
    warn "--purge-network: deleting the NetworkManager profiles emm-wifi and emm-eth (the WiFi and Ethernet settings made through EMM)."
    warn "If this Pi is connected through one of them (for example over SSH), it will have NO network after the next reboot or NetworkManager reload, and no setup network either, because EMM is being removed. Have an Ethernet cable, or a keyboard and monitor, ready."
    run rm -f /etc/NetworkManager/system-connections/emm-wifi.nmconnection \
              /etc/NetworkManager/system-connections/emm-eth.nmconnection
    # Not reloading NetworkManager here: that would drop the connection this very
    # moment (and this SSH session with it). It keeps its copy until the next reload.
    warn "NetworkManager was NOT reloaded, so the current connection stays up until you reboot or run 'sudo nmcli connection reload'."
  else
    log "kept: NetworkManager profiles emm-wifi / emm-eth, your WiFi and Ethernet settings, so the Pi stays reachable  (use --purge-network to delete)"
    if [ "$HAVE_SYSTEMD" = 1 ] && have nmcli && systemctl is-active --quiet NetworkManager; then
      run nmcli connection reload || true
      run systemctl reload NetworkManager || true
    fi
  fi
  warn "left in place: the apt packages (avahi-daemon network-manager bluez dnsmasq-base iw rfkill ...) and the hostname."
  log "EMM removed."
}

if [ "$UNINSTALL" = 1 ]; then
  uninstall
  exit 0
fi

# ---- architecture and binary ------------------------------------------------
# The release binary is a static linux/arm64 build. What matters for "is this a
# 64-bit system" is the userland, not the kernel: a 32-bit Raspberry Pi OS can
# also run on a 64-bit kernel, where "uname -m" says armv8l (or aarch64, if asked
# by a 64-bit process) while getconf LONG_BIT says 32.

MACHINE=$(uname -m)

# userland_bits: 32 or 64, the word size of the installed userland.
userland_bits() {
  local b
  b=$(getconf LONG_BIT 2>/dev/null || true)
  case "$b" in 32|64) printf '%s\n' "$b"; return ;; esac
  if have file; then
    case "$(file -L /bin/sh 2>/dev/null)" in
      *32-bit*) echo 32; return ;;
      *64-bit*) echo 64; return ;;
    esac
  fi
  # No way to ask: believe the kernel's machine name.
  case "$MACHINE" in aarch64|arm64) echo 64 ;; *) echo 32 ;; esac
}

# Set BITS32=1 for a supported-in-principle 32-bit system, 0 for 64-bit.
case "$MACHINE" in
  aarch64|arm64)
    if [ "$(userland_bits)" = 64 ]; then BITS32=0; else BITS32=1; fi ;;
  armv7l|armv8l)
    BITS32=1 ;;
  armv6l)
    die "this is an ARMv6 board (Raspberry Pi 1, Zero or Zero W). EMM needs ARMv7 or newer and is built for 64-bit Raspberry Pi OS." ;;
  *) die "unsupported architecture $MACHINE (Raspberry Pi 3/4/5/Zero 2 W with 64-bit Raspberry Pi OS are supported)" ;;
esac

if [ "$BITS32" = 1 ]; then
  [ -n "$BINARY" ] || die "this is a 32-bit Raspberry Pi OS (uname -m: $MACHINE, userland: 32-bit). EMM's release binary is 64-bit and EMM is only built and tested for 64-bit Raspberry Pi OS: reinstall with the 64-bit image (recommended). To try it on this system anyway, build a binary that runs here (make dist-armv7, i.e. GOARCH=arm GOARM=7) and pass it with --binary PATH."
  warn "32-bit system ($MACHINE): not a supported configuration, and untested. Installing the binary you passed with --binary; its --version run below is the only check that it fits."
  EXPECTED_BUILD="a build that runs on this system ($MACHINE)"
else
  EXPECTED_BUILD="a linux/arm64 build"
fi

find_binary() {
  local c
  if [ -n "$BINARY" ]; then printf '%s\n' "$BINARY"; return; fi
  for c in "$ROOT/dist/emmd-linux-arm64" "$ROOT/emmd" "$ROOT/emmd-linux-arm64" "$PWD/emmd" "$PWD/dist/emmd-linux-arm64"; do
    if [ -f "$c" ]; then printf '%s\n' "$c"; return; fi
  done
}
BINARY=$(find_binary)
[ -n "$BINARY" ] || die "no emmd binary found. Build it (scripts/deploy.sh does that for you) or pass --binary PATH."
# A bare file name is a file in the current directory. Run as given, bash would
# look it up in $PATH instead ("command not found", reported as a wrong architecture).
case $BINARY in */*) ;; *) BINARY=./$BINARY ;; esac
[ -f "$BINARY" ] || die "no such file: $BINARY"
if [ "$DRY_RUN" != 1 ]; then
  chmod +x "$BINARY" 2>/dev/null || true
  "$BINARY" --version >/dev/null 2>&1 \
    || die "$BINARY does not run on this machine (wrong architecture or damaged). Expected $EXPECTED_BUILD."
fi
for f in packaging/systemd/emmd.service packaging/avahi/emm-http.service packaging/nm/zz-emm.conf \
         packaging/udev/90-emm-usb-midi.rules packaging/emm.txt.example; do
  [ -f "$ROOT/$f" ] || die "missing $ROOT/$f (run this script from a full checkout or a deploy bundle)"
done

# ---- network stack ----------------------------------------------------------
# EMM drives NetworkManager (WiFi client, setup access point). Raspberry Pi OS
# Bookworm and Trixie already use it. Older images use dhcpcd + wpa_supplicant,
# which cannot run next to it.

nm_active()    { systemctl is-active --quiet NetworkManager.service; }
legacy_stack() { systemctl is-enabled --quiet dhcpcd.service 2>/dev/null || systemctl is-active --quiet dhcpcd.service 2>/dev/null; }

SWITCH_NET=0
if legacy_stack && ! nm_active; then
  if [ "$FORCE_NET" = 1 ]; then
    SWITCH_NET=1
    warn "this image uses dhcpcd; switching to NetworkManager. An SSH session may drop; reconnect to the Pi's new address."
  else
    die "this image uses dhcpcd/wpa_supplicant, but EMM needs NetworkManager (the default on Bookworm and Trixie).
       Either reflash with a current Raspberry Pi OS Lite (recommended), or re-run with --force-network-switch
       to disable dhcpcd and enable NetworkManager."
  fi
fi

# ---- packages ---------------------------------------------------------------

PACKAGES=(avahi-daemon network-manager wpasupplicant bluez dnsmasq-base iw rfkill wireless-regdb)
missing=()
if have dpkg-query; then
  for p in "${PACKAGES[@]}"; do
    dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q '^install ok installed' || missing+=("$p")
  done
else
  die "apt/dpkg not found: this installer supports Debian-based Raspberry Pi OS only"
fi
if [ "${#missing[@]}" -gt 0 ]; then
  log "installing packages: ${missing[*]}"
  run env DEBIAN_FRONTEND=noninteractive apt-get update -qq
  run env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${missing[@]}"
else
  log "required packages already installed"
fi

if [ "$SWITCH_NET" = 1 ]; then
  run systemctl disable --now dhcpcd.service || true
  run systemctl enable --now NetworkManager.service
fi

# ---- files ------------------------------------------------------------------

log "installing emmd ($BINARY)"
run install -D -m 0755 "$BINARY" /usr/bin/emmd.new
run mv -f /usr/bin/emmd.new /usr/bin/emmd     # atomic, works while emmd is running

run install -D -m 0644 "$ROOT/packaging/systemd/emmd.service" /etc/systemd/system/emmd.service
run install -D -m 0644 "$ROOT/packaging/avahi/emm-http.service" /etc/avahi/services/emm-http.service
run install -D -m 0644 "$ROOT/packaging/nm/zz-emm.conf" /etc/NetworkManager/conf.d/zz-emm.conf
run install -D -m 0644 "$ROOT/packaging/udev/90-emm-usb-midi.rules" /etc/udev/rules.d/90-emm-usb-midi.rules
# NetworkManager only adds its --conf-dir to the setup AP's dnsmasq if this exists.
run install -d -m 0755 /etc/NetworkManager/dnsmasq-shared.d
run install -d -m 0755 /var/lib/emm

if [ ! -e /etc/default/emmd ]; then
  if [ "$BOOT" = /boot/firmware ]; then
    write_file /etc/default/emmd 0644 <<'EOF'
# Extra or overriding arguments for emmd (one flag per word). Uncomment to use.
# The built-in default is:
#EMMD_ARGS=--boot-dir=/boot/firmware --state=/var/lib/emm/state.json --allow-power
EOF
  else
    write_file /etc/default/emmd 0644 <<EOF
# This system keeps its boot files in $BOOT, not /boot/firmware.
EMMD_ARGS=--boot-dir=$BOOT --state=/var/lib/emm/state.json --allow-power
EOF
  fi
fi

# Never overwrite a user's emm.txt (or state.json / system.json). The .example is reference only.
run install -D -m 0644 "$ROOT/packaging/emm.txt.example" "$BOOT/emm.txt.example"
if [ -e "$BOOT/emm.txt" ]; then
  log "keeping your existing $BOOT/emm.txt"
else
  log "creating $BOOT/emm.txt (everything commented out)"
  run install -D -m 0644 "$ROOT/packaging/emm.txt.example" "$BOOT/emm.txt"
fi

# ---- hostname ---------------------------------------------------------------

HOSTNAME_CHANGED=0
if [ "$NO_HOSTNAME" = 1 ]; then
  :
elif [ "$HAVE_SYSTEMD" != 1 ]; then
  log "not touching the hostname (hostnamectl needs a running systemd; set 'hostname = ...' in $BOOT/emm.txt)"
else
  current=$(hostname 2>/dev/null || true)
  case "$current" in
    raspberrypi|localhost|debian|"")
      log "setting hostname to 'emm' (was '${current:-unset}'): the web UI will be at http://emm.local/"
      run hostnamectl set-hostname emm
      if [ -f /etc/hosts ]; then
        if grep -qE '^127\.0\.1\.1[[:space:]]' /etc/hosts; then
          run sed -i -E 's/^(127\.0\.1\.1[[:space:]]+).*/\1emm/' /etc/hosts
        else
          run sh -c "printf '127.0.1.1\temm\n' >> /etc/hosts"
        fi
      fi
      HOSTNAME_CHANGED=1 ;;
    *)
      log "keeping your hostname '$current' (set 'hostname = ...' in $BOOT/emm.txt to change it)" ;;
  esac
fi

# ---- USB host mode sanity ---------------------------------------------------
# EMM needs the Pi's USB ports in host mode. A gadget/OTG setup meant for
# plugging the Pi into a computer (dwc2 + g_ether/g_serial/...) would fight
# that on boards where the same port does both (Pi Zero, Pi 3, CM4).

if [ -f "$BOOT/config.txt" ] && grep -qE '^[[:space:]]*dtoverlay=dwc2' "$BOOT/config.txt"; then
  warn "$BOOT/config.txt enables the dwc2 overlay (USB OTG/gadget). If your MIDI devices do not show up, remove it or use dr_mode=host."
fi
if [ -f "$BOOT/cmdline.txt" ] && grep -qE 'modules-load=[^ ]*(dwc2|g_[a-z_]+)' "$BOOT/cmdline.txt"; then
  warn "$BOOT/cmdline.txt loads dwc2/g_* gadget modules; this turns the OTG port into a device port. Remove them to use it as a USB host."
fi
if have rfkill && rfkill list wifi 2>/dev/null | grep -q 'Soft blocked: yes'; then
  warn "WiFi is soft-blocked (usually: no regulatory country set). Set 'wifi_country = XX' in $BOOT/emm.txt, or run 'sudo raspi-config nonint do_wifi_country XX'."
fi

# ---- enable and start -------------------------------------------------------

if [ "$HAVE_SYSTEMD" != 1 ]; then
  run_quiet udevadm control --reload-rules || true
  printf '\n' >&2
  log "EMM's files are installed, but nothing was enabled or started: systemd is not running here."
  printf '    Run this installer again on the booted Pi (it is safe to run again).\n\n' >&2
  exit 0
fi

run systemctl daemon-reload
run_quiet udevadm control --reload-rules || true
for u in avahi-daemon.service NetworkManager.service bluetooth.service emmd.service; do
  run systemctl enable "$u" || warn "could not enable $u (is the package installed? no Bluetooth hardware is fine)"
done
if nm_active; then run systemctl reload NetworkManager.service || true; fi
if [ "$HOSTNAME_CHANGED" = 1 ]; then run systemctl restart avahi-daemon.service || true; fi

if [ "$NO_START" = 1 ]; then
  log "installed; not started (--no-start)."
else
  run systemctl restart emmd.service
  if [ "$DRY_RUN" != 1 ]; then
    sleep 2
    if ! systemctl is-active --quiet emmd.service; then
      warn "emmd is not running. Look at: journalctl -u emmd -b --no-pager | tail -50"
      exit 1
    fi
  fi
fi

# ---- done -------------------------------------------------------------------

h=$(hostname 2>/dev/null || echo emm)
printf '\n'
log "EMM is installed."
printf '    Open   http://%s.local/\n' "$h" >&2
ips=$(hostname -I 2>/dev/null || true)
if [ -n "${ips// /}" ]; then
  for ip in $ips; do
    case "$ip" in *:*) ;; *) printf '    or     http://%s/\n' "$ip" >&2 ;; esac
  done
fi
printf '    Config %s/emm.txt   Logs: journalctl -u emmd -f\n\n' "$BOOT" >&2
