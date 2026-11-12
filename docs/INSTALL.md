# Installing EMM

Two ways to run Esben's MIDI Mainframe on a Raspberry Pi 3, 4 or 5 (or Zero 2 W, 400, 500, CM4, CM5):

| | A. Raspberry Pi OS + installer | B. Appliance SD card |
|---|---|---|
| What you do | Flash Raspberry Pi OS Lite, run one command from your computer | Copy a set of files onto a FAT32 SD card |
| What you get | A normal Debian system with EMM as a service; SSH, `apt`, logs | A small read-mostly system that runs from RAM; no Linux partition, nothing to corrupt when you pull the plug |
| Status | The path to start with | **Not yet built or booted on real hardware**, see [SDCARD-IMAGE.md](SDCARD-IMAGE.md) |

Both give the same EMM: same web interface, same `emm.txt`.

## Downloads

Every release on the [Releases page](https://github.com/esbenvb/esbens-midi-mainframe/releases) has ready-made files, so nothing needs to be built (the version, for example `v0.1-alpha1`, is part of each name):

| File | For | How to use it |
|---|---|---|
| `emm-sdcard-<version>.zip` | A **new SD card** for any supported Pi (3 B/B+/A+, Zero 2 W, 4, 400, CM4, 5, 500, CM5) | Format FAT32, unzip, copy the *contents* onto the card ([B](#b-appliance-sd-card)) |
| `emm-sdcard-<version>-rpi3.zip`, `-rpi4.zip`, `-rpi5.zip` | The same, a smaller download for one board family | The same |
| `emm-update-<version>.zip` | A Pi that **already runs the card image**: update system, kernel and application over the network | Switch SSH on in the web interface, unzip, run `./update.sh --reboot root@emm.local` ([Updating](#updating)) |
| `emm-app-update-<version>.zip` | The same Pi, when only the **application** changed (a few MB, no reboot) | The same, `./update.sh root@emm.local` |
| `emm-pios-<version>-arm64.tar.gz` | A Pi running **Raspberry Pi OS Lite 64-bit** ([A](#a-raspberry-pi-os-lite--installer)) | Extract, run `./deploy.sh pi@raspberrypi.local` from your computer, or `./install.sh` on the Pi. No Go needed. Run it again to update |
| `emmd-<version>-linux-arm64`, `-linux-armv7` | Your own packaging (armv7 is untested) | The daemon alone: static, the web interface is inside |
| `EMM-manual-<version>.pdf` | Everyone | The user manual |
| `SHA256SUMS` | Everyone | `shasum -a 256 -c SHA256SUMS` |

Releases are made by pushing a tag `release/vX.Y.Z` (the workflow is `.github/workflows/release.yml`; `make release-local` makes the same files on your machine from `dist/` and `image/output/`). A version with a dash, such as `v0.1-alpha1`, is published as a pre-release.

## A. Raspberry Pi OS Lite + installer

**You need** a Pi 3/4/5/Zero 2 W (a Pi 4 or 5 is strongly recommended, see [HARDWARE.md](HARDWARE.md)), an SD card, and a computer with Go ≥ 1.22 and `ssh`.

1. Flash **Raspberry Pi OS Lite (64-bit)**, Bookworm or Trixie, with Raspberry Pi Imager. In its settings turn on SSH, set a user and password, and (optional) your WiFi and WiFi country. 32-bit images are not supported: the installer refuses a 32-bit system (a 32-bit userland counts, even on a 64-bit kernel) unless you pass a binary you built for it with `--binary` (`make dist-armv7`), which is untested.
2. Boot the Pi on your network and make sure you can `ssh pi@raspberrypi.local` (use the user name you chose).
3. From a checkout of this repository on your computer:

   ```bash
   scripts/deploy.sh pi@raspberrypi.local
   ```

   `--dry-run` shows what it would do first. The script cross-compiles `emmd` for arm64, copies it and the installer to a private temporary directory on the Pi (made with `mktemp -d`, removed afterwards), and runs `scripts/install-on-pi.sh` from there with `sudo`. You can also copy the repository (with `dist/emmd-linux-arm64`) to the Pi and run `sudo scripts/install-on-pi.sh` yourself.

4. Open **http://emm.local/**.

The installer, which is safe to run again:

* installs `avahi-daemon network-manager wpasupplicant bluez dnsmasq-base iw rfkill wireless-regdb` if missing;
* installs `/usr/bin/emmd`, the `emmd` systemd service, an Avahi service announcing the web interface, a NetworkManager settings file (WiFi power-save off, no connectivity probing), and a udev rule that keeps USB MIDI devices out of USB autosuspend;
* creates `/boot/firmware/emm.txt` with everything commented out, **only if you have none**; it never overwrites `emm.txt`, `state.json` or `system.json`;
* sets the hostname to `emm` only if it is still a default one (`raspberrypi`, `localhost`, `debian`, or unset); `--no-hostname` leaves it alone;
* warns if a USB OTG/gadget setup (`dtoverlay=dwc2`, `g_ether`, …) would stop the USB ports acting as a host, or if WiFi is blocked for lack of a country;
* enables and starts everything. If systemd is not running (the installer is run in a chroot or container), it only copies the files, says so, and leaves the rest to a second run on the booted Pi.

On an older image that still uses `dhcpcd`, it stops and tells you to reflash; `--force-network-switch` switches to NetworkManager anyway (an SSH session may drop). Remove EMM with `scripts/deploy.sh --uninstall pi@…` or `sudo scripts/install-on-pi.sh --uninstall`. That keeps your settings and your WiFi/Ethernet profiles, so the Pi stays reachable. Extra flags go after `--` with `deploy.sh`, e.g. `scripts/deploy.sh --uninstall pi@… -- --purge`:

* `--purge` also deletes the settings: everything in `/var/lib/emm/` (`state.json`, `system.json`), `/etc/default/emmd`, `emm.txt` and `reset-network`. The NetworkManager profiles `emm-wifi` and `emm-eth` (the WiFi and Ethernet settings made through EMM) **are still kept**.
* `--purge-network` also deletes those two profiles, which hold your WiFi password. **This can strand the Pi:** if it is connected through one of them, it has no network after the next reboot (or `nmcli connection reload`), and no setup network either because EMM is gone, so you need an Ethernet cable or a keyboard and monitor. The installer prints a warning and does not reload NetworkManager itself, so an SSH session survives until you reboot.

## B. Appliance SD card

1. Get `emm-sdcard-<version>.zip` (build it with `image/build.sh`, see [SDCARD-IMAGE.md](SDCARD-IMAGE.md)). One zip works on Pi 3 B/B+/A+, Zero 2 W, Pi 4/400/CM4/CM4S and Pi 5/500/500+/CM5/CM5 Lite.
2. **Format the card as FAT32 with an MBR partition table.** The Raspberry Pi bootloader reads FAT32 only, and many cards over 32 GB come formatted exFAT. The system is a few tens of MB, so a small partition is plenty and a small card is easiest.

   *macOS* (this erases the whole card; check the disk number with `diskutil list` first):
   ```bash
   diskutil eraseDisk FAT32 EMM MBRFormat /dev/disk4
   ```
   *Linux* (replace `sdX`; this erases the card):
   ```bash
   sudo parted /dev/sdX --script mklabel msdos mkpart primary fat32 4MiB 100%
   sudo mkfs.vfat -F 32 -n EMM /dev/sdX1
   ```
   *Windows*: the built-in formatter refuses FAT32 above 32 GB, so use a card of 32 GB or less, or make a small partition with `diskpart` (check `list disk` first; this erases the disk):
   ```
   diskpart
   list disk
   select disk N
   clean
   convert mbr
   create partition primary size=4096
   format fs=fat32 quick label=EMM
   assign
   ```
3. Unzip and copy the **contents** of the `emm-sdcard-<version>` folder onto the card (so that `config.txt` is in the root of the card, not in a subfolder).
4. Put the card in the Pi, connect Ethernet (optional) and your powered USB hub, and power on.

The appliance keeps your settings on the card itself: `emm.txt` (hand-written) and, written by the web interface, `emm/state.json` (connections, presets, device names) and `emm/system.json` (network, web password and SSH, which are secrets). Pulling the plug is safe except in the second or so after you press *Save* in the web interface.

## First boot and network setup

* **With Ethernet** (DHCP): open **http://emm.local/**.
* **A Pi that is already online** (Raspberry Pi OS set up with Imager or `nmtui`, WiFi or Ethernet) keeps its connection: EMM adopts it, never tears it down, and does not open the setup network. Open **http://<the Pi's hostname>.local/** (for example `http://raspberrypi.local/`; EMM only renames the machine if you set `hostname` in `emm.txt` or in the web interface, or if the installer replaced a default name with `emm`). On such a Pi the setup network can take up to about 45 seconds to appear after boot instead of 5 if the Pi has a WiFi profile that is still connecting.
* **With WiFi but nothing online and no configuration**, EMM opens its own network, **EMM-Setup-XXXX** (open, no password, unless you set `ap_password`). Join it with a phone or laptop. A "sign in to network" page opens by itself; if it doesn't, go to **http://emm.local/** or **http://10.42.0.1/**. Choose your WiFi, enter the password, and EMM joins it. Your device drops off the setup network at that moment: reconnect to your normal WiFi and open **http://emm.local/**.
* At boot, if the saved WiFi can't be joined within about 45 seconds and Ethernet has no connection (wrong password, router gone), EMM opens the setup network instead. While it is in setup mode it checks every 30 seconds whether Ethernet has come up and leaves setup mode if so. If joining a network you just chose fails, the setup network comes back and the page says why.
* To force the setup network, create an empty file called **`reset-network`** next to `emm.txt` (on the card, or in `/boot/firmware` on Raspberry Pi OS) and reboot. EMM deletes it again.

### `emm.txt`

A text file in the root of the card (`/boot/firmware/emm.txt` on Raspberry Pi OS). One `key = value` per line. A line starting with `#` is a comment, and so is anything after a `#` that follows a space in a value (`hostname = studio  # my room` is `studio`). Put a value in quotes (`wifi_password = "correct horse #1"`) if it contains ` #` or starts or ends with a space; inside quotes everything is literal. A line EMM cannot use is ignored and the reason goes to its log (`journalctl -u emmd`). `emm.txt.example` has every key, commented out, with its rules. **Keys in `emm.txt` override the web interface and are applied on every boot**; leave a key out to manage it from the web interface.

| Key | Values | Default | Meaning |
|---|---|---|---|
| `hostname` | letters, digits, `-` (no dots), up to 63 characters | `emm` | Reachable as `<hostname>.local` |
| `wifi_country` | two letters, e.g. `DK` | unset | Regulatory domain. WiFi stays off in many regions until it is set; 5 GHz channels depend on it |
| `wifi_ssid`, `wifi_password` | name up to 32 bytes; password 8–63 characters or 64 hex digits | unset | WiFi network to join. An empty password means an open network. A password of the wrong length is tried anyway (with a logged warning) and the join fails |
| `ethernet` | `auto`, `dhcp`, `static`, `off` | `auto` | Wired network mode: `auto` is DHCP when a cable is plugged in, `dhcp` requires DHCP (IPv6 off) |
| `eth_address`, `eth_gateway`, `eth_dns` | `192.168.1.50/24`, address inside that network, one or more addresses (comma or space separated) | unset | Used **only** with `ethernet = static` written in `emm.txt`; with any other mode, or no `ethernet` line, they are ignored. Unusable static settings fall back to DHCP |
| `ap_ssid`, `ap_password` | name up to 32 bytes; password 8–63 characters or 64 hex digits | `EMM-Setup-XXXX`, none | The setup network. An empty password makes it open, and so does a password of the wrong length (it is ignored) |
| `rtp` | `on`/`off` (also `true`/`false`, `yes`/`no`, `1`/`0`) | `on` | Network MIDI (RTP-MIDI / AppleMIDI) |
| `ble` | `on`/`off` (same spellings) | `on` | Bluetooth LE MIDI |
| `ble_name` | text | `EMM` | Name shown when pairing |
| `web_password` | text | none | Password for the web interface. One set here is used instead of one saved from the web interface |
| `allowed_hosts` | host names separated by commas, spaces or semicolons; a leading dot allows a whole domain | none | Extra names the web interface accepts besides `<hostname>.local`, the IP address and `localhost` (for example a name your router gives the Pi: `emm.fritz.box`). Needed because the interface refuses unknown `Host` names as protection against DNS rebinding |

The passwords are stored in plain text on the card.

## Using the web interface

* **Backup** (System tab → *Backup*): *Download backup* saves the routing, presets, device names and the RTP/Bluetooth switches to a file; *Restore from file…* puts them back, replacing the current ones (connections to devices this unit has never seen are skipped and reported). The network setup, web password and SSH settings are **not** in a backup and a restore never changes them: they are the device's own. To have them on a freshly flashed card, either put the WiFi, hostname and web password in `emm.txt` (see `emm.txt.example`), or copy `system.json` from another unit's card into an `emm` folder on the new card (create the folder; on the appliance image this is `/boot/emm/system.json`, on Raspberry Pi OS `/var/lib/emm/system.json`). SSH is switched on in the web interface; to keep its host key (so the fingerprint does not change), copy the `emm/ssh` folder too.
* **Presets** (Matrix tab → *Presets*): save the whole routing (every connection and its filters) under a name, bring it back with *Recall*. Device names, hidden ports and network switches are not part of a preset. Recalling replaces the current routing; connections to a device EMM has forgotten are skipped and reported.
* **Moving a device to another hub port.** A USB device that reports no serial number is told apart by the hub port it sits in. When you move one, EMM carries its connections, name and network port over to the new port, as long as that is unambiguous: exactly one device of that model is attached and exactly one is remembered as absent. With two identical devices it cannot tell them apart, so give them their own ports and leave them there.
* **Forgetting unplugged devices** (Devices tab): *Forget* on an unplugged device, or *Forget all*, deletes its connections, name and settings. Do this for entries that are left over (for example from before this feature). A device you plug in again afterwards starts over as a new device.
* **SSH** (appliance image only; System tab → *SSH access*): off by default, and nothing listens on port 22 while it is off. Set a password for the web interface first (SSH is a root shell, so the switch stays locked until you do), then give SSH a password and/or paste public keys, and switch it on: `ssh root@emm.local`. It stays on across reboots until you switch it off, which also removes the password and keys from the running system. The host key is kept in `emm/ssh/` on the card, so its fingerprint (shown on the card) survives reboots. On Raspberry Pi OS the SSH card is not shown: use the system's own `sshd`.

## Updating

* **A:** run `scripts/deploy.sh pi@…` again, or `./deploy.sh pi@…` from a newer `emm-pios-…` download. Settings stay.
* **B, the application only:** the card image does not contain the application: it is the file `emmd` on the card, which the system starts from there. So an update of just the application is that one file. Download `emm-app-update-<version>.zip`, switch SSH on (System tab, set a password first), unzip it and run `./update.sh root@emm.local`: it copies `emmd` (the old one stays as `emmd.previous`) and restarts it, with no reboot. If the new application needs a newer system than the one on the card (the release notes say so when a release changes the system), it does not start and the previous one runs instead (`journalctl -u emmd`): use `emm-update` then. From a checkout, `make dist` and `scripts/push-boot.sh --board rpi4 --restart-app --from <folder with emmd> root@emm.local` do the same.
* **B, everything, over the network:** download `emm-update-<version>.zip`, switch SSH on in the web interface (System tab, set a password first), unzip it and run `./update.sh --reboot root@emm.local`. It asks the device which Raspberry Pi it is and copies only that board's files to the boot partition (system, kernel and `emmd`); `emm.txt` and the `emm/` folder are not touched, and a `config.txt` or `cmdline.txt` you changed is kept as `config.txt.previous`. Use a cable rather than WiFi if you can, do not use network MIDI for it, and do not remove power while it copies. From a checkout, `scripts/push-boot.sh --auto root@emm.local` does the same with your own build.
* **B, by hand:** build or download the new zip and copy its files over the old ones on the card (`kernel*.img`, `rootfs-*.cpio.gz`, `emmd`, `*.dtb`, `start4.elf`, `fixup4.dat`, `config.txt`, `cmdline.txt`). **Keep** `emm.txt` and the `emm/` folder.

## Troubleshooting

**`emm.local` doesn't open.** It relies on mDNS. macOS, iOS and Linux (with `avahi`/`nss-mdns`) resolve it. Windows 10/11 usually does; if it doesn't, or on a VPN or managed network, use the IP address. Android support is inconsistent, notably in apps. Find the address in your router's client list (look for `emm`), or `ping emm.local` from a computer where it works. Network MIDI (RTP-MIDI) discovery uses the same mDNS: on Windows you may need Bonjour (installed with iTunes or Apple's Bonjour Print Services) plus a RTP-MIDI driver.

**No WiFi at all / the setup network never appears.** Set `wifi_country` in `emm.txt` (or on Raspberry Pi OS: `sudo raspi-config nonint do_wifi_country DK`). Check `rfkill list`: "Soft blocked: yes" for wlan means no country. EMM does not open the setup network while Ethernet has a working connection. Check that `emmd` runs: `systemctl status emmd`, `journalctl -u emmd -b`.

**The setup page doesn't pop up on my phone.** Open `http://emm.local/` or `http://10.42.0.1/` by hand. Turn off a VPN, and "private DNS" on Android.

**A USB MIDI device doesn't show up.** Run `sudo emmd --list-devices` on the Pi (Raspberry Pi OS). The appliance has no shell; the matrix in the web interface shows what was detected, and a monitor on the HDMI port shows the boot messages. If the device isn't listed, check `lsusb` and `dmesg | tail` (Raspberry Pi OS).
* Use a **powered** hub, and give the Pi its proper power supply; a bus-powered hub with several bus-powered devices browns out the Pi.
* Only **class-compliant** USB MIDI devices work. A device that needs a vendor driver on Windows/macOS usually isn't one.
* On boards where the same port can be a USB device (Zero, 3, CM4), make sure no `dtoverlay=dwc2`/`g_*` gadget setup is active; the installer warns about this.

**Bluetooth MIDI won't pair.** EMM appears as a Bluetooth LE MIDI device named `EMM` (or your `ble_name`); no PIN is needed. Connect from the MIDI application, not only the system Bluetooth list: macOS: *Audio MIDI Setup → Window → Show MIDI Studio → Bluetooth*; iPhone/iPad: the Bluetooth MIDI settings inside apps such as GarageBand. Windows and Android need an app that supports Bluetooth LE MIDI. Bluetooth is for notes and controllers, not for SysEx dumps or samples (use USB or network MIDI for those). WiFi on 2.4 GHz and Bluetooth share the radio: use 5 GHz WiFi or Ethernet if connections are unstable.

**Sample/patch transfers are slow or stall.** Use Ethernet or 5 GHz WiFi. RTP-MIDI over WiFi was not built for bulk data; the USB side is not the bottleneck.

**Start over.** Create `reset-network` (see above). To clear the routing, presets and device names, delete `emm/state.json*` on the card (`/var/lib/emm/state.json*` on Raspberry Pi OS); to clear the network, web password and SSH settings, delete `emm/system.json*`; then reboot. (The `*` is for the `.new`, `.old` and `.corrupt-…` files a power cut can leave.)
