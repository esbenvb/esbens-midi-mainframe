# Changelog

What changed in each release of Esben's MIDI Mainframe. Releases are listed on the
[Releases page](https://github.com/esbenvb/esbens-midi-mainframe/releases).

## Unreleased

### Added

- **Console** (the Monitor tab, renamed): the live MIDI log now sits beside controls for
  sending MIDI by hand to a USB device. A two-octave keyboard with octave shift and velocity,
  sliders and buttons for the common messages (modulation, volume, pan, expression, pitch bend,
  aftertouch, program and bank change, all notes/sound off, sustain, transport, system reset),
  and custom messages: a message type with menus for notes (by name), controllers (with their
  usual names) and 0-127 values, or raw bytes in hex. **Panic** for the chosen device and
  **Panic all** for every USB device.
- **SysEx** tab: receive a SysEx dump from a USB device into the browser (optionally after a
  request such as an identity request), save it as a `.syx` file, load `.syx` files, and send
  the selected messages to a USB device with a pause between messages, a progress bar and Cancel.
- Network and Bluetooth ports are not offered by either page: dumps and one-off sends must
  arrive complete, which only a wired connection can promise.
- API: `POST /api/ports/{id}/send`, `POST /api/ports/{id}/sysex` and
  `GET /api/ports/{id}/sysex/listen` (see `docs/ARCHITECTURE.md`).
- The simulated Sim Synth answers an identity request and a dump request
  (`F0 7D 01 F7`), so both pages can be tried without hardware.
- Manual: Console and SysEx sections with new screenshots.

### Changed

- The old `#monitor` address still opens the Console.

### Fixed

- `docs/manual/build.sh` no longer fails when the version contains a `/` (a `release/v…` tag).

## v0.1-alpha1

First public pre-release: the appliance SD-card images, the over-the-air update and
application-update packages, the Raspberry Pi OS installer bundle, the daemon binaries and
the manual.
