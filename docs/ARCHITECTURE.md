# EMM architecture

Esben's MIDI Mainframe (EMM) is a Raspberry Pi appliance: a USB MIDI host,
patchbay, RTP-MIDI (AppleMIDI) server and Bluetooth LE MIDI peripheral, with a
web UI. One Go daemon, `emmd`, does everything; the OS underneath only provides
networking, mDNS, Bluetooth and USB.

```
                        ┌────────────────────────── emmd ───────────────────────────┐
 USB MIDI devices ──────▶ device.RawMIDI ──┐                                         │
 (via powered hub)      │   (hot-plug)     │      ┌─────────┐       ┌─────────────┐  │
                        │                  ├─────▶│ router  │◀─────▶│ web (HTTP,  │◀─── browser
 RTP-MIDI peers ◀──────▶ rtpmidi.Manager ──┤      │ matrix, │       │ SSE, UI)    │  │   emm.local
 (one session per port) │                  │      │ filters │       └─────────────┘  │
 BLE-MIDI centrals ◀───▶ blemidi.Server ───┘      └─────────┘       ┌─────────────┐  │
                        │                                           │ netcfg      │  │
                        │  config.Store (state.json) ◀──────────────│ (WiFi/AP/   │  │
                        │                                           │  captive)   │  │
                        └───────────────────────────────────────────┴─────────────┴──┘
```

## Principles

* **Raw MIDI, not the ALSA sequencer.** The sequencer cannot throttle senders
  and splits SysEx into 256-byte events, so it silently loses data when a
  device is slower than the source. EMM reads and writes `/dev/snd/midiC*D*`
  directly with blocking writes, which gives real back-pressure to the device.
* **A router with queues.** Every destination has one writer goroutine and a
  byte-bounded queue (4 MiB). Bursts (e.g. a network sample upload) are absorbed
  there. SysEx messages from different sources never interleave on one
  destination. Realtime bytes still pass.
* **Connections outlive devices.** A connection `from → to` is keyed by stable
  port IDs and persisted. Unplug a synth and the connection goes dormant; plug
  it back (same USB port, or same serial number) and it comes back. A device
  without a serial number that is plugged into a *different* port is recognised
  as the same one when that is unambiguous (see "Port IDs").
* **Declarative config.** `emm.txt` on the FAT32 partition (hand-edited) plus
  `state.json` (written by the web UI) are rendered into runtime config at
  every boot. The root filesystem of the appliance image is read-only / in RAM,
  so nothing else needs to persist.
* **No cgo on the Pi.** Everything that runs on Linux is pure Go (std lib +
  `godbus` for Bluetooth), so the daemon cross-compiles from a laptop to a single
  static arm64/armv7 binary. The only cgo is the optional macOS CoreMIDI backend
  (`internal/device/coremidi_darwin.go`), used to develop against real synths on a
  Mac; it is excluded from every Linux build.

## Packages

| Package | Role |
|---|---|
| `internal/midi` | MIDI 1.0 byte-stream parser (running status, chunked SysEx), categories, descriptions |
| `internal/router` | Ports, connections, filters, output queues, snapshots, monitor |
| `internal/device` | Port backends: Linux rawmidi (hot-plug via sysfs), simulator |
| `internal/rtpmidi` | AppleMIDI session protocol + RTP-MIDI payload, mDNS (Avahi service files), `Manager` |
| `internal/blemidi` | BLE-MIDI packet codec, BlueZ GATT server + advertisement over D-Bus |
| `internal/netcfg` | `emm.txt` parser, NetworkManager config, WiFi scan/connect, AP + captive portal logic |
| `internal/app` | Reconciler: network endpoints, default connections, moved-device adoption |
| `internal/config` | Persisted `state.json` (atomic writes) |
| `internal/web` | HTTP API, SSE, captive-portal middleware, embedded UI (`internal/web/ui`) |
| `cmd/emmd` | Wires it all together |

## Port IDs

IDs are stable across reboots and match `[A-Za-z0-9:._-]+`.

* USB: `usb:<vid>:<pid>:<serial-or-usb-path>:<cable>` (lower-case hex ids).
  The serial is used when the device reports a usable one; otherwise the
  physical USB port path in the kernel's `<bus>-<devpath>` form (e.g. `1-1.2`),
  so two identical synths on different hub ports stay distinct and keep their
  connections. If two devices ever report the same serial, both fall back to
  path IDs for the rest of the run. A rawmidi device with several cables adds
  the cable index; further rawmidi devices on one card add `<dev>-<sub>`.
  **Moving a device without a serial number to another hub port** would make it a
  new device, with an offline duplicate left behind. `internal/app/adopt.go` undoes
  that: when exactly one device of a model (vendor and product ID) is attached,
  and exactly one path-identified device of the same model with the same cables
  is remembered but absent, the two are the same device, and the old entry's
  connections (including its network endpoint's), saved presets, name, hidden
  flag and UDP port move to the new IDs. Nothing is guessed otherwise: with two
  identical devices attached, several absent candidates, or a candidate that was
  ever attached together with an identical device (`known[].twin`), the entries
  stay and the user forgets the leftovers (`DELETE /api/ports/{id}`).
* Other ALSA raw MIDI devices (only with `IncludeNonUSB`): `alsa:<cardid>:<cable>`.
* RTP network port bound to a device: `rtp:` + the device's port ID.
* Extra virtual network ports: `rtp:net-<slug>`.
* Bluetooth: `ble:0` (a single adapter in this version).
* Simulated: `sim:<slug>`.
* macOS CoreMIDI (development backend): hardware `usb:cm-<uniqueID>`, the IAC bus,
  network sessions and other apps' virtual ports `virtual:cm-<id>`.

By default every USB port gets a paired RTP network port (so each synth is a
separate network MIDI device) with a bidirectional connection between them.
Users can delete or filter those connections like any other.

## Persistent state

`config.State` (JSON) holds connections, per-port settings (alias, RTP on/off,
RTP UDP port), last-known port names (so dormant connections can still be shown
by name), RTP/BLE settings and saved WiFi. `emm.txt` overrides the keys it
contains on every boot.

The file is written atomically and rarely. A corrupt `state.json` is moved aside
(`state.json.corrupt-<time>`) and the daemon starts empty. A `state.json` that
cannot be *read* (I/O error, permissions) is left untouched: the daemon starts
with empty in-memory state, refuses to write until it is restarted, and the UI
shows its "read-only or unreadable" banner, so a flaky card cannot silently wipe
the saved settings. (While that state lasts the saved web password is not known,
so the web interface is open.)

## HTTP API

All JSON. Errors are `{"error": "message"}` with a 4xx/5xx status.

| Method & path | Purpose |
|---|---|
| `GET /api/state` | `{ports, connections, hostname, version}` — topology |
| `GET /api/events` | Server-Sent Events: `topology` (same body as `/api/state`), `activity`, `monitor` (only with `?monitor=1`), `settings`, `network` |
| `PUT /api/connections` | body `{from, to, filter}`; creates or updates a connection |
| `DELETE /api/connections?from=ID&to=ID` | removes a connection |
| `PATCH /api/ports/{id}` | body any of `{alias, rtp, hidden}` |
| `DELETE /api/ports/{id}` | forget an unplugged device: its connections (and its network endpoint's), name and settings. 409 while it is plugged in |
| `POST /api/ports/forget-offline` | forget every unplugged device; `{removed}` |
| `POST /api/ports/{id}/panic` | All Sound Off / All Notes Off / sustain off on every channel |
| `POST /api/ports/{id}/test-note` | middle C, ch1, for 300 ms |
| `GET /api/rtp` | network endpoints: `[{id, name, control_port, peers, device}]` |
| `POST /api/rtp/ports` | body `{name}` — add an extra virtual network port |
| `DELETE /api/rtp/ports/{id}` | remove such a port |
| `GET/PUT /api/settings` | `{rtp:{enabled}, ble:{enabled, name}, web:{password_set}}` |
| `GET /api/network` | status of WiFi/Ethernet/AP, hostname, IPs |
| `POST /api/network/scan` | rescan; returns networks |
| `POST /api/network/wifi` | body `{ssid, password}`; joins and remembers it |
| `DELETE /api/network/wifi` | forget the saved network |
| `PUT /api/network/ethernet` | body `{mode: "auto"\|"dhcp"\|"static"\|"off", address, gateway, dns}` |
| `PUT /api/network/hostname` | body `{hostname}` |
| `GET /api/system` | version, uptime, board model, kernel, CPU temperature, USB device count |
| `POST /api/system/reboot`, `POST /api/system/shutdown` | when enabled with `--allow-power` |

### Port JSON

```json
{
  "id": "usb:1234:abcd:1.2:0",
  "name": "Digitakt",          // default name from the device
  "alias": "",                  // user override; display = alias || name
  "kind": "usb",                // usb | rtp | ble | virtual | sim
  "group": "Elektron Digitakt",
  "input": true, "output": true,
  "detail": "USB 1.2 · 1234:abcd",
  "online": true,               // false = remembered, not currently present
  "rtp": true,                  // USB ports: exposed over the network
  "hidden": false,
  "stats": {"rx_msgs":0,"rx_bytes":0,"tx_msgs":0,"tx_bytes":0,"dropped":0,
            "errors":0,"queued_bytes":0,"last_rx":0,"last_tx":0}
}
```

### Connection JSON

```json
{"from": "usb:…", "to": "rtp:usb:…", "online": true,
 "filter": {"block": ["active_sense"], "channels": [], "map_channel": 0, "transpose": 0}}
```

Filter categories: `note poly_at cc program chan_at bend sysex clock transport
active_sense reset sys_common other`. `channels` is an allow-list (1–16, empty
= all). `map_channel` (1–16) forces channel messages onto one channel.
`transpose` is −48…48 semitones.

### SSE events

* `topology` — full `{ports, connections}` whenever it changes.
* `activity` — `{"<port id>": {rx_msgs, tx_msgs, last_rx, last_tx, dropped,
  queued_bytes, …}}` for ports whose counters changed, at most 20×/s.
* `monitor` — array of `{t, port, dir: "rx"|"tx", from, desc, hex}`.
* `settings`, `network` — payload is the new body of the matching GET; refetch.

## Captive portal

While the device is in setup mode (no usable network at all: that includes a
connection made outside EMM, which EMM adopts and never tears down) it runs a WiFi access
point `EMM-Setup-XXXX` with DHCP and a wildcard DNS answer pointing at itself.
The HTTP server answers the operating systems' connectivity probes
(`/hotspot-detect.html`, `/generate_204`, `/connecttest.txt`, `/ncsi.txt`, …)
and any foreign `Host:` with a redirect to the setup page, so phones pop the
"sign in to network" sheet. WiFi scan results are cached *before* the AP goes up
(a single radio cannot scan reliably while serving an AP).

## Boot-time config: `emm.txt`

`key = value` lines on the FAT32 partition; `#` starts a comment.

```
hostname      = emm           # applied only if set; otherwise the machine keeps its own name
wifi_country  = DK
wifi_ssid     = MyNetwork
wifi_password = secret
ethernet      = auto          # auto | dhcp | static | off
eth_address   = 192.168.1.50/24
eth_gateway   = 192.168.1.1
eth_dns       = 192.168.1.1
ap_ssid       = EMM-Setup
ap_password   =               # empty = open setup network
rtp           = on
ble           = on
ble_name      = EMM
web_password  =
allowed_hosts =               # extra names the web UI accepts, e.g. emm.fritz.box, .example.net
```

A file named `reset-network` next to `emm.txt` forces setup mode on the next
boot, ignoring saved WiFi *and* any WiFi in `emm.txt` for that boot (the daemon
deletes it afterwards, if the card is writable). Settings fixed by `emm.txt`
cannot be changed from the web UI: the API answers `409` with an explanation, and
the UI shows them read-only (`status.boot_managed`).

## Web server hardening

* **Host header allow-list** (DNS-rebinding defence). Requests are served only when
  `Host` is an IP literal, `localhost`, the bare hostname, any `*.local` name,
  `<hostname>.<lan|home|internal|localdomain|intranet|private>` or
  `<hostname>.home.arpa`. Anything else (including `emm.evil.example`) gets `421`
  with a hint. Extra names: `allowed_hosts = nas.example.net, .mydomain.example` in `emm.txt`
  (the only way on the appliance image) or `emmd --allowed-hosts=…` (a leading dot
  allows a whole domain).
* **Cross-origin writes** are refused (`Origin` must match `Host`), bodies must be
  `application/json`, and responses carry a strict CSP (no inline scripts).
* **Password** (optional, HTTP Basic, any user name): `web_password` in `emm.txt`
  or the System tab. While the device is in WiFi setup mode only the setup flow
  stays open without it (static UI files, `GET /api/network`,
  `POST /api/network/scan`, `POST /api/network/wifi`, `/healthz`).
* **Unknown ports**: `PUT /api/connections` answers `404` for an endpoint that is
  neither live nor remembered, so a typo cannot persist a junk connection.

## API response shapes

```jsonc
// GET /api/state  (and the body of the SSE "topology" event)
{ "version": "0.1.0", "hostname": "emm", "now": 1731000000000,
  "ports": [ /* Port JSON, see above; remembered-but-absent ports have online:false and zero stats */ ],
  "connections": [ /* Connection JSON */ ] }

// GET /api/rtp
[ { "id": "rtp:usb:1234:abcd:1.2:0", "name": "Digitakt", "control_port": 5004, "peers": 1, "device": "usb:1234:abcd:1.2:0" } ]
//   device is "" for extra virtual network ports

// GET /api/settings   PUT accepts any subset of the same shape
{ "rtp": { "enabled": true },
  "ble": { "enabled": true, "name": "EMM", "available": true, "advertising": true, "connected": 0, "error": "" },
  "web": { "password_set": false },
  "managed": { "rtp": false, "ble": false, "ble_name": false, "web_password": false } }
//   PUT may also carry {"web": {"password": "..."}} ("" clears it).
//   managed.* is true for a setting that emm.txt fixes: the UI shows it read-only and a PUT for it is refused with 409.

// GET /api/network
{ "status": { "mode": "client|ap|ethernet|connecting|offline|unmanaged", "setup_mode": false,
              "ssid": "MyWifi", "ip": "192.168.1.50", "hostname": "emm", "ap_ssid": "", "last_error": "",
              "ethernet": { "link": true, "ip": "192.168.1.51", "mode": "auto" }, "scan_age": 12,
              "joining": false, "adopted": false, "boot_managed": ["hostname"], "warnings": [] },
//   adopted:true = the device is online through a connection EMM did not create (e.g. a Raspberry Pi OS
//   install that was already on WiFi); EMM leaves it alone. hostname is the machine's actual name unless
//   emm.txt or the UI set one. warnings are problems found in emm.txt (never including secret values).
  "networks": [ { "ssid": "MyWifi", "signal": 82, "security": "WPA2", "in_use": true } ],
  "saved_ssid": "MyWifi",
  "ethernet": { "mode": "auto", "address": "", "gateway": "", "dns": [] },
  "boot_managed": ["hostname"] }   // settings emm.txt currently fixes: hostname, wifi, ethernet, wifi_country
// status.scan_age is -1 when no scan has happened yet; status.joining is true while a Connect() is in progress
// (during a join from setup mode the status still reports mode:"ap" with joining:true). Empty fields (ssid, last_error,
// ethernet.dns, zero filter fields) are omitted from the JSON, and networks[].in_use may lag status.ssid right after a join.
// POST /api/network/scan -> { "networks": [...], "stale": false }   (stale:true = cached list returned because the AP is up)
// POST /api/network/wifi {ssid,password} -> 202 { "status": {...} }  (in setup mode the device switches ~2 s later and the phone loses the AP)

// GET /api/system
{ "version": "0.1.0", "uptime_s": 12345, "model": "Raspberry Pi 4 Model B Rev 1.4", "kernel": "6.12.x",
  "cpu_temp_c": 51.2, "usb_devices": 3, "allow_power": true, "backend": "rawmidi", "state_writable": true }
```

`GET /api/events` sends an initial `topology` event immediately, a `:` comment
heartbeat every 15 s, and supports `?monitor=1`. Clients should reconnect on
error (the browser's `EventSource` does) and refetch `/api/state`.

### Daemon flags used by the UI during development

```
emmd --backend sim --netcfg fake --listen 127.0.0.1:8099 \
     --ui-dir internal/web/ui --state /tmp/x/state.json --boot-dir /tmp/x/boot
```
`--ui-dir` serves the UI from disk (no rebuild while editing); without it the
UI embedded in the binary is served. `--netcfg fake` uses a simulated network
stack so the WiFi onboarding pages can be tried on a laptop (the fake starts in
setup mode when no WiFi is saved; a password starting with `wrong`, e.g. `wrongpassword`, fails on purpose — the API itself requires a WPA passphrase of 8–63 characters or a 64-hex-digit key, or an empty password for an open network).
