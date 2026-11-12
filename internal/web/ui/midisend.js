// Sending MIDI from the browser: which devices can be used, building messages,
// and an ordered queue that posts them to the daemon.
//
// Only wired devices (USB) are offered: network and Bluetooth ports are fine
// for playing, but dumps and one-off sends must arrive complete.

import { post } from './api.js';
import { S, displayName } from './state.js';

export const isWired = (p) => p.kind === 'usb' || p.kind === 'sim';

// Wired ports that are plugged in and can send (dir 'input') or receive (dir 'output') MIDI.
export function wiredPorts(dir) {
  return S.ports.filter((p) => isWired(p) && p.online !== false && p[dir]);
}

// A label for a port in a list: its name, plus its detail when two share a name.
export function portLabels(ports) {
  const count = new Map();
  for (const p of ports) count.set(displayName(p), (count.get(displayName(p)) || 0) + 1);
  return new Map(ports.map((p) => [p.id, count.get(displayName(p)) > 1 && p.detail ? `${displayName(p)} (${p.detail})` : displayName(p)]));
}

// ---- messages ---------------------------------------------------------------
export const noteOn = (ch, n, v) => [0x90 | ch, n, v];
export const noteOff = (ch, n) => [0x80 | ch, n, 0];
export const cc = (ch, c, v) => [0xB0 | ch, c, v];
export const program = (ch, p) => [0xC0 | ch, p];
export const chanAT = (ch, v) => [0xD0 | ch, v];
export const bend = (ch, v) => { const u = Math.max(0, Math.min(16383, v + 8192)); return [0xE0 | ch, u & 127, u >> 7]; };

export const hex = (bytes) => Array.from(bytes, (b) => b.toString(16).toUpperCase().padStart(2, '0')).join(' ');

// "90 3C 64", "0x90, 0x3c, 0x64" and "903C64" all work.
export function parseHex(text) {
  const bytes = [];
  for (let tok of String(text).split(/[\s,;]+/)) {
    if (!tok) continue;
    tok = tok.replace(/^0x/i, '');
    if (!/^[0-9a-f]+$/i.test(tok)) return { bytes: [], error: `“${tok}” is not a hex number` };
    if (tok.length > 2) {
      if (tok.length % 2) return { bytes: [], error: `“${tok}” has an odd number of digits` };
      for (let i = 0; i < tok.length; i += 2) bytes.push(parseInt(tok.slice(i, i + 2), 16));
    } else {
      bytes.push(parseInt(tok, 16));
    }
  }
  return { bytes, error: bytes.length ? '' : 'Type some bytes in hex, for example 90 3C 64' };
}

// Same rules as the daemon: complete messages only, no running status.
export function checkMessages(bytes) {
  if (!bytes.length) return 'Nothing to send';
  for (let i = 0; i < bytes.length;) {
    const s = bytes[i];
    let n;
    if (s < 0x80) return `Byte ${i + 1} (${hex([s])}) is a data byte without a status byte before it`;
    if (s === 0xF0) {
      let e = i + 1;
      while (e < bytes.length && bytes[e] < 0x80) e++;
      if (e >= bytes.length || bytes[e] !== 0xF7) return `The SysEx starting at byte ${i + 1} does not end with F7`;
      i = e + 1;
      continue;
    }
    if (s < 0xC0 || (s >= 0xE0 && s < 0xF0)) n = 3;
    else if (s < 0xE0) n = 2;
    else if (s === 0xF1 || s === 0xF3) n = 2;
    else if (s === 0xF2) n = 3;
    else if (s === 0xF6 || (s >= 0xF8 && s !== 0xF9 && s !== 0xFD)) n = 1;
    else return `Byte ${i + 1} (${hex([s])}) is not a MIDI status byte that exists`;
    if (i + n > bytes.length) return `The message starting at byte ${i + 1} (${hex([s])}) is missing data bytes`;
    for (let j = 1; j < n; j++) if (bytes[i + j] >= 0x80) return `Byte ${i + j + 1} (${hex([bytes[i + j]])}) should be a data byte (0-127)`;
    i += n;
  }
  return '';
}

const NOTE_NAMES = ['C', 'C#', 'D', 'D#', 'E', 'F', 'F#', 'G', 'G#', 'A', 'A#', 'B'];
export const noteName = (n) => `${NOTE_NAMES[n % 12]}${Math.floor(n / 12) - 1}`; // middle C = C4 = 60
export const isBlack = (n) => [1, 3, 6, 8, 10].includes(n % 12);

// ---- the send queue -----------------------------------------------------------
// Messages go out in the order they were made (two parallel requests could
// overtake each other and leave a note hanging), several at a time when they
// pile up. A message made with a key replaces an older, unsent one with the same
// key: while a slider is dragged only its latest value matters.
export function createSender(target, onError) {
  const q = [];
  let busy = false;

  async function pump() {
    if (busy) return;
    busy = true;
    try {
      while (q.length) {
        const port = q[0].port;
        const data = [];
        while (q.length && q[0].port === port && data.length + q[0].bytes.length <= 4000) data.push(...q.shift().bytes);
        try { await post(`/api/ports/${port}/send`, { data }); } catch (e) { onError(e); }
      }
    } finally { busy = false; }
  }

  return {
    // Returns false when no device is chosen.
    send(bytes, key) {
      const port = target();
      if (!port) return false;
      const old = key ? q.find((it) => it.key === key && it.port === port) : null;
      if (old) old.bytes = bytes; else q.push({ port, bytes, key });
      pump();
      return true;
    },
  };
}
