# Esben's MIDI Mainframe (EMM)

A Raspberry Pi appliance that turns a USB hub full of synths into one patchbay:

* **USB MIDI host** — plug synths, controllers and interfaces into a hub on the Pi.
* **Routing matrix** — connect any input to any output, with per-connection
  filters (message types, channels, channel remap, transpose). Connections
  survive unplugging and reboots. Save the whole routing as a named **preset**
  and bring it back with one click.
* **RTP-MIDI (AppleMIDI)** — every USB device also appears as its own network
  MIDI device, so a Mac, iPhone, Windows PC or Linux box can talk to any synth
  wirelessly: remote control, patch dumps, sample uploads.
* **Bluetooth LE MIDI** — the Pi shows up as a Bluetooth MIDI device.
* **Web UI** at `http://emm.local` — first boot opens a setup WiFi with a
  captive portal; after that it joins your network (or use Ethernet, or put the
  WiFi details in `emm.txt` on the SD card).
* **Install by copying files** to a FAT32 SD card (appliance image, see
  [docs/SDCARD-IMAGE.md](docs/SDCARD-IMAGE.md)) or run the installer on
  Raspberry Pi OS Lite (see [docs/INSTALL.md](docs/INSTALL.md)).

One Go daemon, `emmd`, does all of it. No cgo, no libasound: it reads and writes
ALSA raw MIDI nodes directly so SysEx back-pressure is real (the ALSA sequencer
silently drops data when a device is slower than its source — see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)).

## Try it on your laptop

```sh
make run          # simulated synths + simulated WiFi stack, UI on http://127.0.0.1:8099
make test         # unit tests, race detector on
make check        # fmt, vet, tests, and that the Linux/ARM targets still build
```

On a Mac, `./bin/emmd --backend coremidi --netcfg off --listen 127.0.0.1:8099 --state /tmp/emm.json`
drives your real USB synths through CoreMIDI, and the network MIDI endpoints are
announced with Bonjour — handy for testing the router and RTP-MIDI against real
gear before the Pi arrives. `--backend sim,coremidi` mixes both.

## On a Pi

```sh
make dist                       # static arm64 + armv7 binaries in dist/
                                # (64-bit OS: automatic; a 32-bit OS needs the installer's --binary flag)
make deploy HOST=pi@raspberrypi.local   # copy and install on Raspberry Pi OS Lite
```

Then open `http://emm.local`. Hardware advice (which Pi, which hub, and a
theoretical, untested caveat about wired Ethernet on a Pi 3) is in
[docs/HARDWARE.md](docs/HARDWARE.md).

## Layout

| Path | What |
|---|---|
| `cmd/emmd` | the daemon |
| `internal/midi` | MIDI byte-stream parser |
| `internal/router` | ports, connections, filters, queues |
| `internal/device` | Linux raw MIDI backend, macOS CoreMIDI backend, simulator |
| `internal/rtpmidi` | RTP-MIDI / AppleMIDI sessions + mDNS announcements |
| `internal/blemidi` | BLE-MIDI codec + BlueZ GATT server |
| `internal/netcfg` | `emm.txt`, NetworkManager, setup AP, captive portal |
| `internal/config` | persisted state |
| `internal/app` | reconciliation: network endpoints, default connections, carrying a moved device's settings to its new USB port |
| `internal/sshaccess` | the appliance image's SSH switch (Dropbear, root password and keys) |
| `internal/web` | HTTP API, live events, embedded UI (`internal/web/ui`) |
| `packaging/`, `scripts/`, `image/` | systemd units, installers, Buildroot appliance image |

## Status

What has been verified, and how, is tracked honestly because most of this
cannot be exercised without a Pi:

* **Verified by automated tests on a development machine** (race detector on):
  the MIDI parser, router (routing, filters, held-note release, SysEx
  serialisation between sources, lossless back-pressure for large dumps),
  persistence, the HTTP API and event stream, the reconciler, RTP-MIDI
  protocol over loopback, BLE-MIDI packet codec, `emm.txt` parsing, NetworkManager
  keyfile generation and the setup/captive-portal state machine (against a fake
  `nmcli`), device discovery against fake sysfs trees.
* **Compiled for linux/arm64 and armv7 but never run on hardware**: the raw MIDI
  device I/O (ioctls, thread-pinned subdevice selection), the BlueZ GATT server
  and advertisement, the real `nmcli` calls and the Avahi announcements.
* **Not verified against third-party implementations** unless noted in the
  package docs: RTP-MIDI against Apple's, BLE-MIDI against macOS/iOS/Android.
* **Not built**: the Buildroot appliance image has never been built (that needs a
  Linux machine, 15–30 GB of disk and an hour or two).

Expect a first-hardware shakedown. `emmd --list-devices` and `--log-level debug`
are the first tools to reach for.

## Security model (read this before putting it on a shared network)

* The web UI and the API have **no password by default**. Anyone on the LAN can
  reroute MIDI and send SysEx to your synths. Set a password in the System tab or
  with `web_password` in `emm.txt` (HTTP Basic, so only worthwhile on a trusted LAN).
* **RTP-MIDI (AppleMIDI) has no authentication at all.** Anyone on the network can
  connect to a network MIDI endpoint and send SysEx to the synth behind it.
* On the appliance image the System tab can switch **SSH** on (a root shell, with a
  password and/or public keys you set there). It is off at every boot unless you
  saved it on, and it cannot be switched on until the web interface has a password.
* The **setup WiFi is open** unless `ap_password` is set in `emm.txt`; the home
  WiFi password you type there crosses the air once. Set `ap_password` if that
  matters to you.
* WiFi credentials are stored **in clear text** in `state.json` / `emm.txt` on the
  FAT32 partition (the appliance has no encrypted storage), so treat the SD card
  like the password it contains.
* The server rejects requests whose `Host` header is not an IP, `localhost`, the
  device's own name (`emm`, `emm.local`, `emm.lan`, ...) or a `*.local` name
  (DNS-rebinding protection; add names with `allowed_hosts` in `emm.txt` or `--allowed-hosts`), and
  state-changing requests from another origin. While in setup mode with a web
  password set, only the WiFi setup endpoints are open.

## Not (yet) included

* No `LICENSE` file has been chosen.
* No DIN-5 MIDI (a UART HAT would fit the same backend interface).
* No RTP-MIDI recovery journal (RFC 6295). Instead, when a UDP packet is lost or
  a network session ends or times out, EMM releases the notes that peer was
  holding: no stuck notes (a held sustain pedal is the exception: only the Panic
  button releases it), but a lost packet can cut a note the player is still
  holding. Use Ethernet for anything critical.
* USB MIDI 2.0 / UMP devices are not handled (legacy MIDI 1.0 endpoints only).
