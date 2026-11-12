// Console tab: the live MIDI log, plus what it takes to send MIDI to a wired
// device by hand: a two-octave keyboard, the common messages, and custom ones.

import { h, replace } from './dom.js';
import { icon } from './icons.js';
import { post } from './api.js';
import { on } from './state.js';
import { toast, confirmDialog } from './ui.js';
import { mountLog } from './monitor.js';
import {
  wiredPorts, portLabels, createSender, noteOn, noteOff, cc, program, chanAT, bend,
  hex, parseHex, checkMessages, noteName, isBlack,
} from './midisend.js';

// What the page remembers while you switch tabs.
const cs = {
  portId: '',
  channel: 0,        // 0-15
  velocity: 100,
  octave: 4,         // the keyboard starts at C(octave-1): 4 -> C3 .. C5
  cc: { 1: 0, 7: 100, 10: 64, 11: 127 },
  aftertouch: 0,
  bank: ['', ''],
  program: 0,
  custom: { type: 'cc', d1: 7, d2: 100 },
  raw: '',
};

const MAX_OCTAVE = 8; // C7 .. C9 would run past note 127; C7 + 24 = 120 is the highest start

export function mount(root) {
  let lastErr = 0;
  const sender = createSender(() => cs.portId, (e) => {
    if (Date.now() - lastErr > 2500) { lastErr = Date.now(); toast(`Could not send: ${e.message}`, 'error'); }
  });
  const cleanups = [];

  // ---- target and channel -------------------------------------------------------
  const targetSel = h('select', { id: 'con-target', 'aria-label': 'Send to', onchange: () => { releaseAll(); cs.portId = targetSel.value; } });
  const chanSel = h('select', { id: 'con-channel', 'aria-label': 'MIDI channel', onchange: () => { releaseAll(); cs.channel = Number(chanSel.value); } },
    Array.from({ length: 16 }, (_, i) => h('option', { value: i }, String(i + 1))));
  chanSel.value = String(cs.channel);
  const panicBtn = h('button', { type: 'button', class: 'btn btn-small', onclick: async () => {
    if (!cs.portId) return;
    try { await post(`/api/ports/${cs.portId}/panic`); toast('Sent all-notes-off on every channel'); } catch (e) { toast(e.message, 'error'); }
  } }, icon('panic', 14), 'Panic');
  const panicAllBtn = h('button', { type: 'button', class: 'btn btn-small btn-danger', title: 'All notes off on every channel of every USB device', onclick: async () => {
    const ports = wiredPorts('output');
    if (!ports.length) return;
    releaseAll();
    const results = await Promise.allSettled(ports.map((p) => post(`/api/ports/${p.id}/panic`)));
    const failed = results.filter((r) => r.status === 'rejected');
    if (failed.length) toast(`Panic failed on ${failed.length} of ${ports.length} devices: ${failed[0].reason.message}`, 'error');
    else toast(`Sent all-notes-off to ${ports.length} device${ports.length === 1 ? '' : 's'}`);
  } }, icon('panic', 14), 'Panic all');
  const noTarget = h('div', { class: 'notice notice-warn', role: 'note', hidden: true },
    icon('alert', 16), h('span', {}, 'No wired device that can receive MIDI is plugged in. The console sends to USB devices only, not to network or Bluetooth ports.'));
  const controls = h('div', { class: 'con-controls' });

  function fillTargets() {
    const ports = wiredPorts('output');
    const labels = portLabels(ports);
    replace(targetSel, ports.map((p) => h('option', { value: p.id }, labels.get(p.id))));
    if (!ports.some((p) => p.id === cs.portId)) { releaseAll(); cs.portId = ports.length ? ports[0].id : ''; }
    targetSel.value = cs.portId;
    noTarget.hidden = ports.length > 0;
    controls.hidden = ports.length === 0;
    panicAllBtn.disabled = ports.length === 0;
  }

  // ---- keyboard --------------------------------------------------------------------
  const held = new Map();       // note -> how many fingers
  const heldCh = new Map();     // note -> channel it went out on
  const keyEls = new Map();     // note -> element
  const pointers = new Map();   // pointer id -> note it is on

  function press(n) {
    held.set(n, (held.get(n) || 0) + 1);
    if (held.get(n) > 1) return;
    heldCh.set(n, cs.channel);
    sender.send(noteOn(cs.channel, n, cs.velocity));
    const el = keyEls.get(n);
    if (el) el.classList.add('down');
    status(`${noteName(n)} (${n}) on, velocity ${cs.velocity}`);
  }
  function release(n) {
    const c = (held.get(n) || 0) - 1;
    if (c > 0) { held.set(n, c); return; }
    if (c < 0) return;
    held.delete(n);
    sender.send(noteOff(heldCh.get(n) ?? cs.channel, n));
    heldCh.delete(n);
    const el = keyEls.get(n);
    if (el) el.classList.remove('down');
  }
  function releaseAll() {
    for (const n of Array.from(held.keys())) { held.set(n, 1); release(n); }
    pointers.clear();
  }

  const keyboard = h('div', { class: 'kb', role: 'group', 'aria-label': 'Keyboard' });
  const rangeLabel = h('span', { class: 'mono' });
  const velOut = h('output', { class: 'mono' }, String(cs.velocity));

  function buildKeyboard() {
    releaseAll();
    keyEls.clear();
    const start = cs.octave * 12;
    const notes = Array.from({ length: 25 }, (_, i) => start + i);
    const whites = notes.filter((n) => !isBlack(n));
    const w = 100 / whites.length;
    const keys = [];
    let whiteIdx = 0;
    for (const n of notes) {
      if (!isBlack(n)) {
        const el = keyEl(n, 'white', { left: `${whiteIdx * w}%`, width: `${w}%` });
        keys.push(el);
        whiteIdx++;
      }
    }
    whiteIdx = 0;
    for (const n of notes) {
      if (!isBlack(n)) { whiteIdx++; continue; }
      const bw = w * 0.62;
      keys.push(keyEl(n, 'black', { left: `${whiteIdx * w - bw / 2}%`, width: `${bw}%` }));
    }
    replace(keyboard, keys);
    rangeLabel.textContent = `${noteName(start)} – ${noteName(start + 24)}`;
  }
  function keyEl(n, kind, style) {
    const el = h('div', {
      class: `key ${kind}${n % 12 === 0 ? ' is-c' : ''}`, style, dataset: { note: n },
      role: 'button', tabindex: '0', 'aria-label': `${noteName(n)}`,
      onkeydown: (e) => { if ((e.key === ' ' || e.key === 'Enter') && !e.repeat) { e.preventDefault(); press(n); } },
      onkeyup: (e) => { if (e.key === ' ' || e.key === 'Enter') release(n); },
      onblur: () => { if (!pointers.size && held.has(n)) { held.set(n, 1); release(n); } },
    }, n % 12 === 0 ? h('span', { class: 'key-label' }, noteName(n)) : null);
    keyEls.set(n, el);
    return el;
  }
  const keyAt = (e) => {
    const k = document.elementFromPoint(e.clientX, e.clientY);
    const el = k && k.closest && k.closest('.key');
    return el && keyboard.contains(el) ? Number(el.dataset.note) : null;
  };
  keyboard.addEventListener('pointerdown', (e) => {
    const n = keyAt(e);
    if (n == null || !cs.portId) return;
    e.preventDefault();
    keyboard.setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, n);
    press(n);
  });
  keyboard.addEventListener('pointermove', (e) => {
    if (!pointers.has(e.pointerId)) return;
    const cur = pointers.get(e.pointerId), n = keyAt(e);
    if (n === cur) return;
    if (cur != null) release(cur);
    pointers.set(e.pointerId, n);
    if (n != null) press(n);
  });
  const lift = (e) => {
    if (!pointers.has(e.pointerId)) return;
    const cur = pointers.get(e.pointerId);
    pointers.delete(e.pointerId);
    if (cur != null) release(cur);
  };
  keyboard.addEventListener('pointerup', lift);
  keyboard.addEventListener('pointercancel', lift);
  keyboard.addEventListener('lostpointercapture', lift);
  keyboard.addEventListener('contextmenu', (e) => e.preventDefault());
  const onHide = () => { if (document.hidden) releaseAll(); };
  document.addEventListener('visibilitychange', onHide);
  window.addEventListener('blur', releaseAll);
  cleanups.push(() => { document.removeEventListener('visibilitychange', onHide); window.removeEventListener('blur', releaseAll); });

  function shift(d) {
    cs.octave = Math.max(0, Math.min(MAX_OCTAVE, cs.octave + d));
    buildKeyboard();
    octDown.disabled = cs.octave <= 0;
    octUp.disabled = cs.octave >= MAX_OCTAVE;
  }
  const octDown = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': 'Shift the keyboard down an octave', onclick: () => shift(-1) }, icon('minus', 14), 'Octave');
  const octUp = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': 'Shift the keyboard up an octave', onclick: () => shift(1) }, icon('plus', 14), 'Octave');
  const velocity = h('input', { type: 'range', min: 1, max: 127, value: cs.velocity, 'aria-label': 'Velocity', oninput: () => { cs.velocity = Number(velocity.value); velOut.textContent = String(cs.velocity); } });

  // ---- sliders and small helpers ----------------------------------------------------------
  const statusEl = h('p', { class: 'con-status muted small', role: 'status', 'aria-live': 'off' });
  function status(text) { statusEl.textContent = text; }

  function slider({ label, min, max, value, key, send, spring = false, show = (v) => String(v), title }) {
    const out = h('output', { class: 'mono' }, show(value));
    const input = h('input', { type: 'range', min, max, value, 'aria-label': label, title });
    const settle = () => { input.value = String(spring); out.textContent = show(spring); send(spring, false); };
    input.addEventListener('input', () => { const v = Number(input.value); out.textContent = show(v); send(v, true); });
    if (spring !== false) {
      for (const ev of ['pointerup', 'pointercancel', 'blur', 'keyup']) input.addEventListener(ev, () => { if (Number(input.value) !== spring) settle(); });
    }
    return h('div', { class: 'sl' }, h('label', {}, label), input, out);
  }
  const ccSlider = (label, num) => slider({
    label, min: 0, max: 127, value: cs.cc[num], title: `Control change ${num}`,
    send: (v) => { cs.cc[num] = v; sender.send(cc(cs.channel, num, v), `cc${num}`); status(`CC ${num} = ${v} on channel ${cs.channel + 1}`); },
  });

  function msg(bytes, what) {
    if (sender.send(bytes)) status(`${what}: ${hex(bytes)}`);
  }
  const btn = (label, onclick, cls = '') => h('button', { type: 'button', class: `btn btn-small ${cls}`.trim(), onclick }, label);

  // ---- program change ----------------------------------------------------------------------
  const bankMsb = numInput(cs.bank[0], 0, 127, 'Bank MSB', (v) => { cs.bank[0] = v; });
  const bankLsb = numInput(cs.bank[1], 0, 127, 'Bank LSB', (v) => { cs.bank[1] = v; });
  const prog = numInput(cs.program, 0, 127, 'Program', (v) => { cs.program = v === '' ? 0 : v; }, true);
  function numInput(value, min, max, label, set, required = false) {
    const el = h('input', { type: 'number', min, max, step: 1, value: value === '' ? '' : String(value), 'aria-label': label, inputmode: 'numeric' });
    el.addEventListener('input', () => set(el.value === '' ? '' : Number(el.value)));
    el.required = required;
    return el;
  }
  const inRange = (el) => el.value !== '' && Number(el.value) >= Number(el.min) && Number(el.value) <= Number(el.max) && Number.isInteger(Number(el.value));
  function sendProgram() {
    if (!inRange(prog)) { toast('Program must be a whole number from 0 to 127', 'error'); return; }
    const out = [];
    if (bankMsb.value !== '') { if (!inRange(bankMsb)) { toast('Bank MSB must be 0 to 127', 'error'); return; } out.push(...cc(cs.channel, 0, Number(bankMsb.value))); }
    if (bankLsb.value !== '') { if (!inRange(bankLsb)) { toast('Bank LSB must be 0 to 127', 'error'); return; } out.push(...cc(cs.channel, 32, Number(bankLsb.value))); }
    out.push(...program(cs.channel, Number(prog.value)));
    msg(out, 'Program change');
  }
  function stepProgram(d) {
    const v = Math.max(0, Math.min(127, (inRange(prog) ? Number(prog.value) : cs.program) + d));
    prog.value = String(v);
    cs.program = v;
    msg(program(cs.channel, v), 'Program change');
  }

  // ---- custom message ------------------------------------------------------------------------
  const TYPES = {
    noteon:  { label: 'Note On',            d1: ['Note', 0, 127, 'note'], d2: ['Velocity', 0, 127], start: 60, make: (c, a, b) => [0x90 | c, a, b] },
    noteoff: { label: 'Note Off',           d1: ['Note', 0, 127, 'note'], d2: ['Velocity', 0, 127], start: 60, make: (c, a, b) => [0x80 | c, a, b] },
    cc:      { label: 'Control Change',     d1: ['Controller', 0, 127, 'cc'], d2: ['Value', 0, 127], start: 7, make: (c, a, b) => [0xB0 | c, a, b] },
    polyat:  { label: 'Poly Aftertouch',    d1: ['Note', 0, 127, 'note'], d2: ['Pressure', 0, 127], start: 60, make: (c, a, b) => [0xA0 | c, a, b] },
    program: { label: 'Program Change',     d1: ['Program', 0, 127, 'list'], make: (c, a) => [0xC0 | c, a] },
    chanat:  { label: 'Channel Aftertouch', d1: ['Pressure', 0, 127, 'list'], make: (c, a) => [0xD0 | c, a] },
    bend:    { label: 'Pitch Bend',         d1: ['Bend', -8192, 8191], make: (c, a) => bend(c, a) },
    songpos: { label: 'Song Position',      d1: ['Position', 0, 16383], make: (c, a) => [0xF2, a & 127, a >> 7] },
    songsel: { label: 'Song Select',        d1: ['Song', 0, 127, 'list'], make: (c, a) => [0xF3, a] },
  };
  const typeSel = h('select', { 'aria-label': 'Message type', onchange: () => { cs.custom.type = typeSel.value; paintCustom(); } },
    Object.entries(TYPES).map(([k, t]) => h('option', { value: k }, t.label)));
  // The first value is a menu where the values are few (128 notes with their names, controllers with
  // theirs, plain 0-127) and a number field where they are many (pitch bend, song position).
  const d1Num = h('input', { type: 'number', step: 1, inputmode: 'numeric', oninput: () => { cs.custom.d1 = d1Num.value; paintCustom(); } });
  const d1Sel = h('select', { 'aria-label': 'First value', onchange: () => { cs.custom.d1 = d1Sel.value; paintCustom(); } });
  const d1Slot = h('span', { class: 'con-d1' }, d1Sel, d1Num);
  let d1 = d1Num; // whichever of the two is showing
  let d1Kind = null;
  const CC_NAMES = { 0: 'Bank select MSB', 1: 'Modulation', 2: 'Breath', 4: 'Foot pedal', 5: 'Portamento time', 6: 'Data entry MSB', 7: 'Volume', 8: 'Balance',
    10: 'Pan', 11: 'Expression', 32: 'Bank select LSB', 38: 'Data entry LSB', 64: 'Sustain pedal', 65: 'Portamento', 66: 'Sostenuto', 67: 'Soft pedal',
    71: 'Resonance', 72: 'Release time', 73: 'Attack time', 74: 'Cutoff / brightness', 91: 'Reverb', 93: 'Chorus', 98: 'NRPN LSB', 99: 'NRPN MSB',
    100: 'RPN LSB', 101: 'RPN MSB', 120: 'All sound off', 121: 'Reset all controllers', 123: 'All notes off' };
  const MENU_LABEL = {
    note: (n) => `${noteName(n)} (${n})`,
    cc: (n) => (CC_NAMES[n] ? `${n} – ${CC_NAMES[n]}` : String(n)),
    list: (n) => String(n),
  };
  function showD1(kind) {
    if (kind === d1Kind) return;
    d1Kind = kind;
    if (kind) {
      replace(d1Sel, Array.from({ length: 128 }, (_, n) => h('option', { value: n }, MENU_LABEL[kind](n))));
      d1 = d1Sel;
    } else {
      d1 = d1Num;
    }
    d1Sel.hidden = !kind;
    d1Num.hidden = !!kind;
  }
  const d2 = h('input', { type: 'number', step: 1, inputmode: 'numeric', oninput: () => { cs.custom.d2 = d2.value; paintCustom(); } });
  const d1Label = h('span', {}, ''), d2Label = h('span', {}, '');
  const preview = h('code', { class: 'con-preview' });
  const customErr = h('p', { class: 'form-error', role: 'alert' });
  const customSend = h('button', { type: 'button', class: 'btn btn-primary btn-small', onclick: () => { const b = customBytes(); if (b) msg(b, TYPES[cs.custom.type].label); } }, 'Send');
  function customBytes() {
    const t = TYPES[cs.custom.type];
    const vals = [[d1, t.d1]].concat(t.d2 ? [[d2, t.d2]] : []);
    const nums = [];
    for (const [el, [name, lo, hi]] of vals) {
      const n = Number(el.value);
      if (el.value === '' || !Number.isInteger(n) || n < lo || n > hi) { customErr.textContent = `${name} must be a whole number from ${lo} to ${hi}`; return null; }
      nums.push(n);
    }
    customErr.textContent = '';
    return t.make(cs.channel, ...nums);
  }
  function paintCustom() {
    const t = TYPES[cs.custom.type];
    typeSel.value = cs.custom.type;
    d1Label.textContent = t.d1[0];
    showD1(t.d1[3] || null);
    for (const [el, spec] of [[d1Num, t.d1], [d2, t.d2]]) if (spec) { el.min = spec[1]; el.max = spec[2]; }
    if (cs.custom.type !== lastType) { // a new type: keep the value if it means the same kind of thing, else a sensible start
      const prev = TYPES[lastType].d1[3] || null;
      if (!prev || prev !== (t.d1[3] || null)) cs.custom.d1 = t.start ?? 0;
      lastType = cs.custom.type;
    }
    d1.value = String(clampTo(cs.custom.d1, t.d1));
    cs.custom.d1 = d1.value;
    d2Label.hidden = d2.hidden = !t.d2;
    if (t.d2) { d2Label.textContent = t.d2[0]; d2.value = String(clampTo(cs.custom.d2, t.d2)); cs.custom.d2 = d2.value; }
    const b = customBytes();
    preview.textContent = b ? hex(b) : '';
    customSend.disabled = !b;
  }
  let lastType = cs.custom.type;
  const clampTo = (v, [, lo, hi]) => { const n = Number(v); return Number.isFinite(n) ? Math.max(lo, Math.min(hi, Math.round(n))) : lo; };

  const rawIn = h('input', { type: 'text', value: cs.raw, placeholder: '90 3C 64   or   F0 7D 01 F7', 'aria-label': 'Raw MIDI bytes in hex', spellcheck: 'false', autocomplete: 'off', class: 'mono',
    oninput: () => { cs.raw = rawIn.value; paintRaw(); }, onkeydown: (e) => { if (e.key === 'Enter') sendRaw(); } });
  const rawErr = h('p', { class: 'form-error', role: 'alert' });
  const rawSend = h('button', { type: 'button', class: 'btn btn-primary btn-small', onclick: sendRaw }, 'Send');
  function rawCheck() {
    const { bytes, error } = parseHex(rawIn.value);
    return { bytes, error: error || checkMessages(bytes) };
  }
  function paintRaw() {
    const { bytes, error } = rawCheck();
    rawErr.textContent = rawIn.value.trim() ? error : '';
    rawSend.disabled = !!error;
  }
  function sendRaw() {
    const { bytes, error } = rawCheck();
    if (error) { rawErr.textContent = error; return; }
    msg(bytes, 'Raw');
  }

  // ---- layout --------------------------------------------------------------------------------------------
  const keyboardCard = h('section', { class: 'card con-card' },
    h('div', { class: 'card-head' }, h('h3', {}, 'Keyboard'), h('div', { class: 'btn-row' }, octDown, rangeLabel, octUp)),
    keyboard,
    h('div', { class: 'sl' }, h('label', {}, 'Velocity'), velocity, velOut));

  const commonCard = h('section', { class: 'card con-card' },
    h('h3', {}, 'Common messages'),
    ccSlider('Modulation', 1), ccSlider('Volume', 7), ccSlider('Pan', 10), ccSlider('Expression', 11),
    slider({ label: 'Pitch bend', min: -8192, max: 8191, value: 0, spring: 0, show: (v) => (v > 0 ? '+' : '') + v, title: 'Springs back to the middle when let go',
      send: (v, live) => { sender.send(bend(cs.channel, v), 'bend'); if (!live) status('Pitch bend back to the middle'); } }),
    slider({ label: 'Aftertouch', min: 0, max: 127, value: cs.aftertouch, spring: 0, title: 'Channel pressure; returns to 0 when let go',
      send: (v) => { cs.aftertouch = v; sender.send(chanAT(cs.channel, v), 'at'); } }),
    h('div', { class: 'con-row' },
      h('label', { class: 'con-lab' }, 'Program'),
      h('div', { class: 'con-prog' },
        h('label', {}, 'Bank MSB', bankMsb), h('label', {}, 'Bank LSB', bankLsb), h('label', {}, 'Program', prog),
        btn(icon('minus', 14), () => stepProgram(-1)), btn(icon('plus', 14), () => stepProgram(1)),
        h('button', { type: 'button', class: 'btn btn-primary btn-small', onclick: sendProgram }, 'Send'))),
    h('div', { class: 'con-row' },
      h('span', { class: 'con-lab' }, 'Channel'),
      h('div', { class: 'btn-row' },
        btn('All notes off', () => msg(cc(cs.channel, 123, 0), 'All notes off')),
        btn('All sound off', () => msg(cc(cs.channel, 120, 0), 'All sound off')),
        btn('Reset controllers', () => msg(cc(cs.channel, 121, 0), 'Reset all controllers')),
        btn('Sustain on', () => msg(cc(cs.channel, 64, 127), 'Sustain on')),
        btn('Sustain off', () => msg(cc(cs.channel, 64, 0), 'Sustain off')))),
    h('div', { class: 'con-row' },
      h('span', { class: 'con-lab' }, 'Transport'),
      h('div', { class: 'btn-row' },
        btn('Start', () => msg([0xFA], 'Start')),
        btn('Continue', () => msg([0xFB], 'Continue')),
        btn('Stop', () => msg([0xFC], 'Stop')),
        btn('Tune request', () => msg([0xF6], 'Tune request')),
        btn('System reset', async () => {
          if (await confirmDialog({ title: 'Send System Reset?', body: 'Some devices restart or go back to their factory settings when they get a System Reset (FF).', confirmLabel: 'Send it', danger: true })) msg([0xFF], 'System reset');
        }, 'btn-danger'))));

  const customCard = h('section', { class: 'card con-card' },
    h('h3', {}, 'Custom message'),
    h('div', { class: 'con-custom' },
      h('label', {}, 'Type', typeSel),
      h('label', {}, d1Label, d1Slot),
      h('label', {}, d2Label, d2),
      h('div', { class: 'con-send' }, preview, customSend)),
    customErr,
    h('div', { class: 'con-raw' },
      h('label', {}, 'Raw bytes (hex)', rawIn),
      rawSend),
    rawErr,
    h('p', { class: 'muted small' }, 'Channel messages use the channel chosen above. Raw bytes may hold several messages and SysEx (F0 … F7); for whole dumps use the SysEx page.'));

  controls.append(keyboardCard, commonCard, customCard);
  const logSlot = h('div', { class: 'con-log' });

  root.append(h('section', { class: 'console' },
    h('div', { class: 'con-send-side' },
      h('div', { class: 'toolbar con-bar' },
        h('div', { class: 'field inline' }, h('label', {}, 'Send to', targetSel)),
        h('div', { class: 'field inline' }, h('label', {}, 'Channel', chanSel)),
        h('div', { class: 'btn-row' }, panicBtn, panicAllBtn)),
      noTarget, controls, statusEl),
    logSlot));

  cleanups.push(mountLog(logSlot));
  fillTargets();
  shift(0);
  paintCustom();
  paintRaw();
  const off = on('topology', () => { fillTargets(); panicBtn.disabled = !cs.portId; });
  panicBtn.disabled = !cs.portId;
  return () => { off(); releaseAll(); for (const c of cleanups) { try { c(); } catch (e) { console.error(e); } } };
}
