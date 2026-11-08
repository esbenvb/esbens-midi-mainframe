// Package blemidi makes the Pi appear as a Bluetooth LE MIDI peripheral that
// macOS (Audio MIDI Setup → Bluetooth), iOS apps, Android and Windows can
// connect to, and bridges it into the router as one port ("ble:0").
//
// # Pieces
//
//   - codec.go: the BLE-MIDI 1.0 packet format (MMA/AMEI RP-052). Decoder turns
//     packets into a plain MIDI byte stream; Encoder turns router chunks into
//     packets that fit the link's MTU. Portable, no dependencies.
//   - queue.go: a small bounded packet queue (drop-oldest, optional wait).
//   - gatt.go: the D-Bus object tree BlueZ talks to (GATT service and
//     characteristic, LE advertisement, pairing agent, ObjectManager,
//     Properties, introspection), with no live connection.
//   - bridge.go: decode → router and router → encode → queue → pace → notify.
//   - server_linux.go: the BlueZ session (connect, register, watch, retry).
//   - server_other.go: a stub so the daemon builds on every OS.
//
// # What is verified (unit tests, run on a laptop without Bluetooth)
//
//   - The codec: spec vectors (header/timestamp split, running status with and
//     without timestamps, system common/realtime not cancelling running status,
//     SysEx continuation packets, realtime inside SysEx, EOX timestamp), random
//     stream round trips at MTU 23/100/185/517, a 200 KB SysEx round trip,
//     malformed-input tolerance and fuzz targets for decoder and encoder.
//   - The queue semantics.
//   - The D-Bus shapes: GetManagedObjects signature and content, property
//     types, characteristic flags, introspection XML, consistency between the
//     exported method tables, their Go signatures and the introspection data,
//     PropertiesChanged signal shape, WriteValue ordering via the connection
//     interceptor, MTU tracking, signal handling.
//   - The bridge against a real router: central → device, device → central,
//     large SysEx in both MTU regimes, port renaming.
//
// # What is UNVERIFIED
//
// Everything that needs a real BlueZ and a real central has never run:
//
//   - registering the GATT application, the advertisement and the pairing agent
//     with a live bluetoothd, and the exact way BlueZ reacts to them (the API
//     shapes follow BlueZ's doc/*.rst, but nobody has run it);
//   - that macOS, iOS, Android and Windows centrals discover, connect, read the
//     (empty) characteristic, enable notifications and exchange data;
//   - "Just Works" pairing through the NoInputNoOutput agent, and the optional
//     Options.Encrypt mode;
//   - notification delivery through PropertiesChanged on "Value" (BlueZ may
//     coalesce or queue rapid updates; the sender paces packets 3 ms apart);
//   - the negotiated MTU: it is learned from the "mtu" option of ReadValue and
//     WriteValue calls and defaults to 23 (20-byte packets) until then;
//   - recovery when bluetoothd restarts (the session ends and is retried with
//     backoff), and advertisement re-registration after Release;
//   - the advertised name fitting in the 31-byte advertising packet next to
//     the 128-bit service UUID (about eight characters; BlueZ shortens longer
//     names).
//
// AcquireNotify is deliberately not implemented: BlueZ falls back to
// StartNotify and property-based notifications, which keeps the unverifiable
// surface small.
//
// # Implementation notes
//
// godbus v5 runs every incoming method call in its own goroutine, which would
// reorder WriteValue packets and corrupt SysEx. WriteValue payloads are
// therefore captured by the connection's incoming-message interceptor, which
// runs on the reader goroutine in arrival order; the exported WriteValue
// handler only learns the central and its MTU.
//
// # Hardware caveats
//
//   - The Pi's onboard CYW43455 shares one 2.4 GHz antenna between WiFi and
//     Bluetooth. Heavy 2.4 GHz WiFi traffic adds BLE latency and jitter; prefer
//     5 GHz WiFi or Ethernet.
//   - BLE-MIDI has no cable number, so one BLE-MIDI device is one port pair. One
//     port per adapter ("ble:0", "ble:1", ...); extra named devices need extra
//     Bluetooth adapters. Several centrals may be connected to one adapter: their
//     input is merged and output goes to all of them.
//   - BLE throughput (tens of kB/s at best, with 7.5–30 ms connection intervals)
//     is fine for notes and controllers but not suited to bulk SysEx or sample
//     transfers; use USB or RTP-MIDI for those.
//   - A pairing agent that accepts everything means any nearby central can
//     connect and send MIDI. Set Options.Encrypt to require an encrypted link,
//     but note that "Just Works" pairing is still not authenticated.
package blemidi
