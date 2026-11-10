# The appliance SD-card image

The goal: copy a few files onto a FAT32 SD card, put it in a Pi, and EMM runs. No Linux partition, no installer, nothing on the card that a power cut can corrupt.

> **Status: written, statically checked, never built or booted.** The tree in `image/` was assembled from Buildroot's documentation and source without a Linux build host or a Pi. Start with `image/build.sh --check-only` (minutes): it validates the configuration against the real Buildroot Kconfig. Treat the first full build and boot as bring-up work. The [UNVERIFIED](#unverified) list is the honest to-do list. The supported, tested path today is [Raspberry Pi OS + installer](INSTALL.md).

## How it works

```
Pi bootloader (EEPROM on the Pi 4/5; bootcode.bin, start.elf and fixup.dat from the card on the Pi 3 family)
  └─ reads the FAT32 partition: config.txt  ([pi3] / [pi02] / [pi4] / [pi5] sections)
       ├─ loads  kernel_pi3.img | kernel8.img | kernel_2712.img   (Linux, built by Buildroot)
       ├─ loads  bcm2710-*.dtb | bcm2711-*.dtb | bcm2712-*.dtb    (chosen by the firmware per board)
       └─ loads  rootfs-pi3.cpio.gz | rootfs-pi4.cpio.gz | rootfs-pi5.cpio.gz  into RAM, right behind the kernel
            (config.txt: "initramfs <file> followkernel")

Linux unpacks the initramfs as its root filesystem (no root= on the command line)
  └─ /init (Buildroot's script: mounts devtmpfs) → exec /sbin/init = systemd
       ├─ fstab: /dev/mmcblk0p1 → /boot  (the same FAT32 partition, rw,sync)
       ├─ NetworkManager, avahi-daemon, bluetoothd, wpa_supplicant (D-Bus activated)
       └─ emmd  (reads /boot/emm.txt, keeps settings in /boot/emm/state.json and system.json)
```

The whole system is therefore **one file per board** that the Pi firmware loads, and `/etc`, `/var`, `/tmp` live in RAM. On the card there is nothing but FAT32. One zip serves all boards because the kernels and initramfs are named per board and `config.txt` filters on `[pi3]`/`[pi02]`/`[pi4]`/`[pi5]`. Raspberry Pi's [model filters](https://www.raspberrypi.com/documentation/computers/config_txt.html#model-filters) make `[pi3]` match the Pi 3 B/B+/A+ (and CM3), `[pi02]` the Zero 2 W, `[pi4]` the Pi 400/CM4/CM4S too, and `[pi5]` the Pi 500/500+, CM5 and CM5 Lite too. The build installs the device trees of these boards:

| Board (build) | Device trees | Kernel |
|---|---|---|
| `rpi3`: Pi 3 B, 3B+, 3A+, Zero 2 W | `bcm2710-rpi-3-b`, `-3-b-plus`, `bcm2837-rpi-3-a-plus`, `bcm2710-rpi-zero-2-w` | Linux 6.6.28, `bcmrpi3` defconfig, 64-bit |
| `rpi4`: Pi 4 B, 400, CM4, CM4S | `bcm2711-rpi-4-b`, `-400`, `-cm4`, `-cm4s` | Linux 6.6.28, `bcm2711` defconfig |
| `rpi5`: Pi 5, 500/500+, CM5, CM5 Lite | `bcm2712-rpi-5-b`, `bcm2712d0-rpi-5-b`, `-500`, `-cm5-cm5io`, `-cm5-cm4io`, `-cm5l-cm5io`, `-cm5l-cm4io` | Linux 6.6.74, `bcm2712` defconfig |

The Pi 5 build uses a newer kernel commit than the other two (`stable_20250127` of `rpi-6.6.y`), because the commit Buildroot pins (6.6.28) has no device tree for the Pi 500/500+ or the CM5 Lite. Hash checking is on, so that commit has its own `linux.hash` in `image/patches/linux/` (Buildroot's own hash file does not list it). The Compute Module 3/3+ and the Pi 2 are not covered.

### Why a separate initramfs file, and not "linked into the kernel"

Buildroot can build the cpio archive into the kernel image (`BR2_TARGET_ROOTFS_INITRAMFS`). That forces a second kernel build pass per board, and makes every rootfs change a kernel change. The Pi firmware loads a separate file natively (`initramfs … followkernel` is the documented mechanism), so the kernel and the system update independently and the build stays simple. Its cost is one more file on the card.

### Why one rootfs per board

The Pi 3, Pi 4 and Pi 5 kernels differ (`bcmrpi3`, `bcm2711` and `bcm2712` configurations, different `uname -r`), and the root filesystem carries the kernel modules (`/lib/modules/<release>`) for its own kernel. So Buildroot is run once per board, and each payload gets its own initramfs. The extra cost is a full build per board; the builds share the download cache. The Pi 3 kernel is deliberately not `kernel8.img`, the Pi 4's name on the same card: `config.txt` names each board's file with `kernel=`.

### Which GPU firmware files, and why none for the Pi 5

The Pi 4 loads its GPU firmware (`start4.elf`, `fixup4.dat`) from the card. The Pi 3 family has no EEPROM bootloader at all, so its card also needs `bootcode.bin`, `start.elf` and `fixup.dat`. The Pi 5 has its firmware in the EEPROM and only needs `config.txt` to mark the partition bootable (Raspberry Pi's `config.txt` documentation).

## Building

Requirements: Docker, and Go on the host (a full `build.sh` cross-compiles `emmd` there first, see `scripts/lib.sh`; with a prebuilt `dist/emmd-linux-arm64` and `--skip-emmd-build` Go is not needed; `--check-only` needs only Docker); **15–30 GB of free disk space in Docker's disk image**, about 8 GB of RAM given to Docker, and 1–2 hours *per board*. The build tree lives in Docker volumes (`emm-buildroot-out`, `emm-buildroot-dl`), not in the repository, because bind mounts are slow on macOS; `image/build.sh --clean` removes them. The download volume also holds a ccache (`/dl/ccache`, 5 GB by default), so rebuilding a kernel after a patch changes takes minutes. `build.sh` runs under `caffeinate` so the Mac does not sleep mid-build. If a build is much slower than 1–2 hours per board, check Docker Desktop's CPU and memory limits (Settings → Resources).

```bash
image/build.sh --check-only        # first: validate the configuration, minutes, < 1 GB
image/build.sh                     # then: all boards (rpi3 rpi4 rpi5) → dist/emm-sdcard-<version>.zip
image/build.sh --boards rpi5       # one board (a quoted list for several: --boards "rpi3 rpi4")
image/build.sh --skip-emmd-build   # use the existing dist/emmd-linux-arm64 (no Go needed)
```

What `build.sh` does: cross-compiles `emmd` (`dist/emmd-linux-arm64`), builds the container from `image/Dockerfile`, downloads and checksum-verifies Buildroot 2025.02.19 (the LTS; its SHA-256 is pinned in `build.sh`), and for each board runs inside the container:

1. `make emm_<board>_defconfig` and **`image/check-defconfig.sh`**: Kconfig silently drops symbols whose dependencies aren't met; this fails the build if any line of our defconfig did not end up in `.config`, or if a symbol we turned off was forced on by another.
2. `make linux-configure` and **`image/check-kernel-config.sh`**: checks the generated kernel `.config` against `image/board/emm/kernel-required.txt` (USB MIDI, initramfs, VFAT, systemd basics, WiFi, Bluetooth) and the board's own `kernel-required.<board>.txt` (its USB host controller, and for the Pi 3 family the SD controller), before the long build starts.
3. The full `make`. Hooks in `image/board/emm/`: `post-build.sh` installs `emmd`, the units from `packaging/` and enables services; `post-image.sh` collects one board's SD-card files into `emm-payload-<board>/`.

Then `scripts/package-sdcard.sh` merges the payloads, checks that every file the firmware needs is present, and writes `dist/emm-sdcard-<version>.zip` with a `.sha256`.

The defconfigs in `image/configs/` are **generated** from `image/defconfig.d/` (a shared part plus one per board) by `image/mkconfigs.sh`; `build.sh` refuses to run if they are out of date. They follow Buildroot's own `raspberrypi3_64` / `raspberrypi4_64` / `raspberrypi5` defconfigs: the same Raspberry Pi kernel commit (so Buildroot's `linux.hash` matches; the exception is the Pi 5, see above), the same 6.6 headers and DT names, with systemd, the cpio rootfs and the EMM packages on top.

## Power loss and the FAT32 partition

The honest trade-off:

* **ext4 root** (what Raspberry Pi OS does): journaled, but a pull of the plug mid-write can still damage the filesystem, and the SD card wears from logging and swap. A damaged root means a box that doesn't boot.
* **This design**: the running system is in RAM, and the card is only written when you change settings. The weak point is FAT32, which has no journal. Three things limit the exposure:
  1. `/boot` is mounted `rw,sync,dirsync`: every write and directory change reaches the card before the call returns, so there is no window of delayed writes after the settings are saved.
  2. EMM writes rarely (on *Save*) and replaces files atomically (write a temporary file, then rename).
  3. The partition is checked at every boot: `systemd-fsck` runs `fsck.fat` (pass 2 in `fstab`) and repairs a dirty FAT.
* What remains: cutting the power *during* a save can lose that save, and at worst leave a file needing repair. The firmware files (`kernel*.img`, `rootfs-*.cpio.gz`) are only written when you update the card, from a computer. Because the system comes from RAM, the worst case is "your latest settings are lost", not "the box doesn't boot".
* `sync` makes writes slow (FAT, small files: fine) and, on a flash card, wear is proportional to writes, which here are a handful per day at most.

Logging is volatile (`journald` `Storage=volatile`, 8 MB), so there is nothing to wear out or fill up, and also nothing to read after a crash.

## Design choices worth knowing

* **Console login:** `root` with an empty password on the HDMI console (tty1). Anyone with physical access has the box anyway; it makes the first boot debuggable. Set `BR2_TARGET_GENERIC_ROOT_PASSWD` or disable root login in `defconfig.d/common.defconfig` if that bothers you.
* **SSH is off until the user switches it on** in the web UI (System tab). The image carries Dropbear (`BR2_PACKAGE_DROPBEAR`), but nothing enables it: `post-build.sh` masks Buildroot's `dropbear.service` and installs `emm-ssh.service` (no `[Install]` section), which `emmd` starts and stops through `systemctl`. `emmd` writes `/run/emm-ssh.env` (host key `…/emm/ssh/dropbear_ed25519_host_key` on the boot partition, so the fingerprint survives reboots; `-s` when there is no password), puts root's password hash (SHA-512 crypt, computed in `internal/sshaccess`, because the image has no tool for it) into `/etc/shadow` and the keys into `/root/.ssh/authorized_keys`. Switching it off stops the unit and puts root's empty console password back. Dropbear refuses empty passwords, so SSH never works without a password or key, and the web UI will not switch it on without a web password.
* **No `dtoverlay=` support:** the card carries no overlays (smaller, nothing to get wrong). Bluetooth is probed by the kernel (`krnbt` defaults to on). To add overlays, enable `BR2_PACKAGE_RPI_FIRMWARE_INSTALL_DTB_OVERLAYS` and copy `overlays/` to the card.
* **Interface names:** `net.ifnames=0` in `cmdline.txt`, so Ethernet is `eth0` and WiFi `wlan0` (systemd's default would name the Pi's Ethernet `end0`).
* **Late WiFi driver:** the WiFi driver is a kernel module loaded by udev, so the wlan device can appear several seconds after `emmd` starts. `emmd` waits up to 30 s for it before deciding there is no WiFi adapter, and also notices a device that appears later.
* **USB autosuspend off** (`usbcore.autosuspend=-1`): a sleeping MIDI interface adds latency to the first message.
* **Debugging a boot that doesn't work:** `cmdline.txt` has no `quiet`, so systemd's progress shows on HDMI. For a serial console add `enable_uart=1` to `config.txt` and `console=serial0,115200` to `cmdline.txt` (Pi 5: the debug connector is `ttyAMA10`).
* **USB boot is not supported:** `fstab` mounts `/dev/mmcblk0p1`. SD cards and CM4 eMMC are both `mmcblk0`.
* **No NAT in setup mode:** `firewall-backend=none` in NetworkManager's config. The access point has no upstream to share.

## UNVERIFIED

Nothing in `image/` has been built or run. What was checked, and what was not:

**Checked against Buildroot 2025.02.19's sources** (read from the release tag): the existence and dependencies of every package and system symbol in `defconfig.d/` (NetworkManager and its CLI, wpa_supplicant with D-Bus/AP/WPA3, dnsmasq, avahi daemon, bluez5_utils, iw, wireless-regdb, util-linux rfkill, brcmfmac_sdio-firmware-rpi, dosfstools, systemd options, cpio rootfs, rpi-firmware variants, the kernel options); the kernel tarball commit and its hash entry in Buildroot's own `linux.hash`; that `fs/cpio/init` mounts devtmpfs and execs `/sbin/init`; that wpa_supplicant's makefile installs its systemd and D-Bus activation files; the Buildroot tarball's SHA-256 (from the signed announcement). Device trees for every board in the table above exist in the kernel commit its build uses (checked in that commit's `arch/arm64/boot/dts/broadcom/`; the Pi 500/500+ and CM5 Lite ones only from `stable_20250127` on). For the Pi 3 family and the Pi 5/500/CM5 boards (the Pi 4 build is unchanged), `make emm_<board>_defconfig` and `check-defconfig.sh` were run against the real Buildroot 2025.02.19 Kconfig on a Mac (all settings accepted), and Buildroot's own hash lookup was checked to find the new `linux.hash` entry for the Pi 5 kernel; the SHA-256 in it was calculated by the same download method that reproduces Buildroot's published hash for its pinned commit. The `[pi3]`, `[pi02]` and `[pi5]` filters are as documented by Raspberry Pi. The WiFi/Bluetooth firmware package ships board-specific files for the Pi 3 B, 3B+, 3A+ and Zero 2 W. That `systemd` is built with `-Dseccomp=disabled` unless `BR2_PACKAGE_LIBSECCOMP` is set (`package/systemd/systemd.mk`; `package/systemd/Config.in` does not select it), and that libseccomp supports aarch64 with the 6.6 headers. Raspberry Pi's documented `config.txt` behaviour: `[pi4]`/`[pi5]` filters, `initramfs … followkernel`, default kernel file names, no `start4.elf` on the Pi 5, `krnbt` default on. NetworkManager keys (`firewall-backend`, `rc-manager`, `wifi.powersave`, `[connectivity]`), and that NetworkManager reads `dnsmasq-shared.d`. BlueZ's `GattManager1` and `LEAdvertisingManager1` are not marked experimental in its current documentation.

**Not verified; expect to find problems here:**

1. The whole chain has never run: Docker build, `make`, `post-build.sh`/`post-image.sh` against real Buildroot output (they were exercised only against fake file trees), booting on a Pi 4 and a Pi 5. `check-defconfig.sh` and `check-kernel-config.sh` exist to catch the configuration part early; their pass/fail logic was tested against synthetic files only.
2. **Kconfig interactions.** A symbol may be dropped for a dependency this review missed. `check-defconfig.sh` reports exactly which; fix `defconfig.d/` and run `image/mkconfigs.sh`.
3. **systemd as PID 1 from an initramfs root** (Buildroot's `/init` helper, then systemd on a ramfs/tmpfs root). It is a common Buildroot setup, but not tried here. The unit's hardening avoids mount-namespace directives for this reason. Its seccomp-based directives (`LockPersonality=`, `RestrictSUIDSGID=`, `RestrictNamespaces=`, `SystemCallArchitectures=`) only take effect because `defconfig.d/common.defconfig` enables `BR2_PACKAGE_LIBSECCOMP` (systemd skips them without it); they are enforced on the image for the first time at its first boot, and `check-defconfig.sh` will show whether Kconfig kept the symbol.
4. **NetworkManager's `shared` mode without iptables/nftables.** The image ships neither, and sets `firewall-backend=none`. If AP activation fails, add `BR2_PACKAGE_NFTABLES=y` and drop that setting (the Pi kernel builds the netfilter modules). Also whether the NetworkManager in this Buildroot release knows `firewall-backend` (the key was read from the current documentation).
5. **WiFi/Bluetooth firmware names on Pi 5 / CM4 / CM5.** `brcmfmac_sdio-firmware-rpi` is LibreELEC's set (covers BCM43455: Pi 3B+, 4, 400, 5); board-specific variants such as `…raspberrypi,4-model-b…` fall back to the generic file names, which depends on the driver. Whether AP mode works on the stock firmware was not checked.
6. **Bluetooth attach.** The Pi's controller is attached by the kernel (serdev) with `krnbt` on; if `bluetoothd` finds no adapter, `btattach`/`hciattach` (`BR2_PACKAGE_BLUEZ5_UTILS_TOOLS`/`_DEPRECATED`) is the fallback. The BlueZ release in Buildroot 2025.02 was not checked for needing `--experimental`.
7. **Boot partition as `/dev/mmcblk0p1`**, `tz=UTC,fmask=0077,dmask=0077,sync,dirsync` options, and `systemd-fsck` repairing FAT32 with `fsck.fat -a` at boot; the real write latency of `sync` on a card.
8. **`usbcore.autosuspend=-1` and the udev rule** (`ATTR{bInterfaceSubClass}=="03"` → `echo on > …/../power/control`).
9. **Kernel options**: the list in `kernel-required.txt` and the three lines in `linux.fragment` come from reading the Raspberry Pi `bcm2711_defconfig` (and, for the Pi 3 family's `kernel-required.rpi3.txt`, `bcmrpi3_defconfig`); `bcm2712_defconfig` was not read. Modules are used for USB audio/MIDI, WiFi and Bluetooth (as in Raspberry Pi OS) and rely on udev autoload plus `modules-load.d/emm.conf` (`snd-usb-audio`).
10. **CM4/CM5 USB host mode**: the image installs no overlays, so these need extra `config.txt` work.
11. **Resource estimates** (rootfs size, RAM use, build time and disk) are estimates. A 1 GB Pi 4 should be fine; a Pi 4 with less RAM isn't made. The initramfs of the Pi 4 build unpacks to about 107 MB, all of it in RAM: fine on a Pi 3 (1 GB), but on the Zero 2 W (512 MB) it takes a fifth of the memory (the Pi 3 and Pi 5 builds were not measured).
12. **Dockerfile package list** is from the Buildroot manual's requirements and memory; the first `--check-only` run will show if something is missing.
13. **Hostname changes**: `emmd` runs `hostnamectl set-hostname` and then `systemctl restart avahi-daemon` (best effort, in `applyHostname` in `internal/netcfg/manager.go`) so that Avahi re-announces the name. Whether both work on the image (systemd-hostnamed over D-Bus, the Avahi restart) has not been tried. A manual `hostnamectl` does not restart Avahi.
14. The root filesystem has no persistent `machine-id`, so DHCP clients using it may look like a new host after each boot (reserve an address by MAC if you need a fixed one).
15. **Pi 3 family and Pi 5/500/CM5 Lite, none of it booted.** Pi 3: the 64-bit `bcmrpi3` kernel (Buildroot's own choice for the Pi 3, but with `kernel=kernel_pi3.img` and `arm_64bit=1` in `config.txt`), the USB host being the `dwc_otg` driver, the Zero 2 W's single OTG port acting as a host, whether the firmware finds a device tree for the 3A+ (`bcm2837-rpi-3-a-plus.dtb` is installed; the 3B/3B+/Zero 2 W ones are the names Raspberry Pi OS uses), and WiFi/Bluetooth on its chips (BCM43430 on the 3B, BCM43455 on the 3B+/3A+, BCM43436 on the Zero 2 W; also the 2.4 GHz-only setup network on the 3B and Zero 2 W). Pi 5 build: Linux 6.6.74 instead of 6.6.28, and the Pi 500 and CM5 Lite device trees, none of which has run.
16. **SSH access** (`internal/sshaccess`, `packaging/systemd/emm-ssh.service`, Dropbear in the defconfig): the Buildroot symbols are accepted by the real Kconfig, the SHA-512 crypt matches OpenSSL's output for several inputs, and the controller was tested against a fake `/etc/shadow` and a fake `systemctl`. Not tried on a Pi: Dropbear starting from the unit with the arguments `emmd` writes, a login with the password and with a key, that `systemctl` and the writes to `/etc/shadow` and `/root/.ssh` work from inside `emmd.service`'s hardening, and that Buildroot's own `dropbear.service` stays off.
