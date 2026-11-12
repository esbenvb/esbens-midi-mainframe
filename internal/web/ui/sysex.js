// SysEx tab: receive dumps from a wired device into the browser, save them as
// .syx files, load .syx files, and send them to a wired device.
//
// Dumps are kept in this page (in memory) while the browser tab is open, also
// when you switch to another tab of the interface; a reload empties the list.
// Wired devices only: a dump has to arrive complete, which network and
// Bluetooth ports cannot promise.

import { h, replace, fmtBytes, fmtNum } from './dom.js';
import { icon } from './icons.js';
import { post } from './api.js';
import { S, on, displayName } from './state.js';
import { toast, badge } from './ui.js';
import { wiredPorts, portLabels, parseHex, checkMessages, hex } from './midisend.js';

const MAX_FILE = 16 * 1024 * 1024;

const sx = {
  messages: [],   // { id, bytes: Uint8Array, source, complete, selected }
  nextId: 1,
  fromId: '',
  toId: '',
  request: '',
  gap: 20,
};

// ---- what a message is ---------------------------------------------------------
const MAKERS = {
  0x01: 'Sequential', 0x04: 'Moog', 0x06: 'Lexicon', 0x07: 'Kurzweil', 0x0F: 'Ensoniq', 0x10: 'Oberheim',
  0x18: 'E-mu', 0x3E: 'Waldorf', 0x40: 'Kawai', 0x41: 'Roland', 0x42: 'Korg', 0x43: 'Yamaha', 0x44: 'Casio',
  0x47: 'Akai', 0x4C: 'Sony', 0x7D: 'Non-commercial / development',
};
const MAKERS3 = {
  '00 00 0E': 'Alesis', '00 20 29': 'Novation', '00 20 32': 'Behringer', '00 20 33': 'Access Music',
  '00 20 3C': 'Elektron', '00 20 6B': 'Arturia', '00 21 09': 'Native Instruments',
};

export function describe(b) {
  if (b.length < 3) return 'SysEx';
  const id = b[1];
  if (id === 0x7E || id === 0x7F) {
    const sub = b[3], sub2 = b[4];
    if (id === 0x7E) {
      if (sub === 0x06 && sub2 === 0x01) return 'Universal: identity request';
      if (sub === 0x06 && sub2 === 0x02) {
        const three = b[5] === 0;
        const maker = three ? MAKERS3[hex(b.slice(5, 8))] : MAKERS[b[5]];
        return `Universal: identity reply${maker ? ` from ${maker}` : ''}`;
      }
      if (sub === 0x09) return `Universal: General MIDI ${sub2 === 1 ? 'on' : sub2 === 2 ? 'off' : 'mode'}`;
      if (sub === 0x01 || sub === 0x02 || sub === 0x03) return 'Universal: sample dump';
      return 'Universal, non-real-time';
    }
    if (sub === 0x04) return `Universal: ${sub2 === 1 ? 'master volume' : sub2 === 2 ? 'master balance' : 'device control'}`;
    return 'Universal, real-time';
  }
  if (id === 0) {
    const m = MAKERS3[hex(b.slice(1, 4))];
    return m ? m : `Manufacturer 00 ${hex(b.slice(2, 4))}`;
  }
  return MAKERS[id] || `Manufacturer ${hex([id])}`;
}

// Cut a file into its SysEx messages; an error text if it holds anything else.
export function splitSysex(bytes) {
  if (!bytes.length) return { error: 'the file is empty' };
  const out = [];
  for (let i = 0; i < bytes.length;) {
    if (bytes[i] !== 0xF0) return { error: `byte ${i + 1} (${hex([bytes[i]])}): a SysEx file must consist of messages that start with F0` };
    let e = i + 1;
    while (e < bytes.length && bytes[e] < 0x80) e++;
    if (e >= bytes.length || bytes[e] !== 0xF7) return { error: `the message at byte ${i + 1} does not end with F7 (the file may be cut off)` };
    out.push(bytes.slice(i, e + 1));
    i = e + 1;
  }
  return { messages: out };
}

function stamp() {
  const d = new Date(), p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

function saveBlob(parts, name) {
  const url = URL.createObjectURL(new Blob(parts, { type: 'application/octet-stream' }));
  const a = h('a', { href: url, download: name });
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

const b64ToBytes = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

export function mount(root) {
  const cleanups = [];

  // ================= receive =================
  let es = null;
  let listening = false;
  const fromSel = h('select', { id: 'sx-from', 'aria-label': 'Receive from', onchange: () => { const v = fromSel.value; stopListening(); sx.fromId = v; paintReceive(); } });
  const listenBtn = h('button', { type: 'button', class: 'btn btn-primary', onclick: () => (listening ? stopListening('Stopped listening.') : startListening()) });
  const recvStatus = h('p', { class: 'muted', role: 'status' });
  const reqIn = h('input', { type: 'text', class: 'mono', value: sx.request, placeholder: 'F0 7E 7F 06 01 F7', spellcheck: 'false', autocomplete: 'off', 'aria-label': 'Request to send first, in hex',
    oninput: () => { sx.request = reqIn.value; paintReceive(); }, onkeydown: (e) => { if (e.key === 'Enter') sendRequest(); } });
  const reqBtn = h('button', { type: 'button', class: 'btn', onclick: sendRequest }, 'Send request');
  const reqErr = h('p', { class: 'form-error', role: 'alert' });
  const noInput = h('div', { class: 'notice notice-warn', role: 'note', hidden: true },
    icon('alert', 16), h('span', {}, 'No wired device that sends MIDI is plugged in. Only USB devices can be used here, not network or Bluetooth ports.'));

  function fromPort() { return S.byId.get(sx.fromId) || null; }

  function startListening() {
    if (!sx.fromId || typeof EventSource === 'undefined') return;
    stopListening();
    listening = true;
    paintReceive();
    recvStatus.textContent = 'Connecting…';
    es = new EventSource(`/api/ports/${sx.fromId}/sysex/listen`);
    const src = displayName(fromPort());
    es.addEventListener('ready', () => { recvStatus.textContent = 'Listening. Start the dump on the device now, or send it a request.'; });
    es.addEventListener('progress', (e) => { const d = parse(e); if (d) recvStatus.textContent = `Receiving a message… ${fmtBytes(d.bytes)} so far`; });
    es.addEventListener('sysex', (e) => {
      const d = parse(e);
      if (!d) return;
      let bytes;
      try { bytes = b64ToBytes(d.data); } catch (_) { return; }
      add(bytes, src, d.complete);
      recvStatus.textContent = `Received a message of ${fmtBytes(bytes.length)}${d.complete ? '' : ', cut short (no F7 at the end)'}. Still listening.`;
      if (d.overrun) toast('Some messages were lost: the browser could not keep up. Receive the dump again.', 'error');
    });
    es.addEventListener('toolong', () => toast(`A message was larger than ${MAX_FILE >> 20} MB and was not kept.`, 'error'));
    es.addEventListener('closed', () => stopListening('The device was unplugged. Listening stopped.'));
    es.addEventListener('error', () => {
      if (es && es.readyState === EventSource.CLOSED) stopListening('Could not listen to that device (is it still plugged in?).');
      else if (es) recvStatus.textContent = 'Connection to the device was lost. Reconnecting…';
    });
  }
  function parse(e) { try { return JSON.parse(e.data); } catch (_) { return null; } }
  function stopListening(msg) {
    if (es) { es.close(); es = null; }
    const was = listening;
    listening = false;
    if (msg || was) recvStatus.textContent = msg || '';
    paintReceive();
  }
  async function sendRequest() {
    const { bytes, error } = parseHex(reqIn.value);
    const err = error || checkMessages(bytes);
    if (err) { reqErr.textContent = err; return; }
    reqErr.textContent = '';
    try { await post(`/api/ports/${sx.fromId}/send`, { data: bytes }); } catch (e) { toast(`Could not send the request: ${e.message}`, 'error'); }
  }
  function paintReceive() {
    const ports = wiredPorts('input');
    const labels = portLabels(ports);
    replace(fromSel, ports.map((p) => h('option', { value: p.id }, labels.get(p.id))));
    if (!ports.some((p) => p.id === sx.fromId)) {
      if (listening) stopListening('The device is gone. Listening stopped.');
      sx.fromId = ports.length ? ports[0].id : '';
    }
    fromSel.value = sx.fromId;
    fromSel.disabled = listening;
    noInput.hidden = ports.length > 0;
    const p = fromPort();
    replace(listenBtn, icon(listening ? 'pause' : 'play', 14), listening ? 'Stop listening' : 'Start listening');
    listenBtn.disabled = !sx.fromId;
    const canAsk = !!p && p.output;
    reqIn.disabled = !canAsk;
    const bad = reqIn.value.trim() ? (parseHex(reqIn.value).error || checkMessages(parseHex(reqIn.value).bytes)) : 'empty';
    reqBtn.disabled = !canAsk || !!bad;
    if (!reqIn.value.trim()) reqErr.textContent = '';
  }

  const receiveCard = h('section', { class: 'card' },
    h('h3', {}, 'Receive from a device'),
    h('p', { class: 'muted small' }, 'Press Start listening, then start the dump on the device (many have a “send” or “dump” function). Some devices only answer a request: type it below.'),
    noInput,
    h('div', { class: 'sx-row' },
      h('div', { class: 'field' }, h('label', { for: 'sx-from' }, 'Device'), fromSel),
      listenBtn),
    h('div', { class: 'sx-req' },
      h('div', { class: 'field' }, h('label', {}, 'Request to send first (optional, hex)', reqIn)),
      reqBtn,
      h('button', { type: 'button', class: 'btn btn-small', title: 'Ask any device that speaks the MIDI identity protocol who it is', onclick: () => { reqIn.value = 'F0 7E 7F 06 01 F7'; sx.request = reqIn.value; paintReceive(); } }, 'Identity request')),
    reqErr,
    recvStatus);

  // ================= the messages =================
  const listSlot = h('div', { class: 'sx-list' });
  const summary = h('p', { class: 'muted small', role: 'status' });
  const saveBtn = h('button', { type: 'button', class: 'btn', onclick: saveSelected }, icon('download', 14), 'Save as .syx');
  const clearBtn = h('button', { type: 'button', class: 'btn', onclick: () => { sx.messages = []; paintList(); paintSend(); } }, icon('trash', 14), 'Clear');
  const picker = h('input', { type: 'file', accept: '.syx,.sysex,.bin,application/octet-stream', multiple: true, hidden: true, 'aria-label': 'SysEx files to load', onchange: () => loadFiles(picker.files).then(() => { picker.value = ''; }) });
  const loadBtn = h('button', { type: 'button', class: 'btn', onclick: () => picker.click() }, icon('upload', 14), 'Load file…');

  function add(bytes, source, complete) {
    sx.messages.push({ id: sx.nextId++, bytes, source, complete, selected: complete });
    paintList();
    paintSend();
  }
  async function loadFiles(files) {
    for (const f of Array.from(files)) {
      if (f.size > MAX_FILE) { toast(`${f.name} is larger than ${MAX_FILE >> 20} MB.`, 'error'); continue; }
      const { messages, error } = splitSysex(new Uint8Array(await f.arrayBuffer()));
      if (error) { toast(`${f.name}: ${error}`, 'error'); continue; }
      for (const m of messages) sx.messages.push({ id: sx.nextId++, bytes: m, source: f.name, complete: true, selected: true });
    }
    paintList();
    paintSend();
  }
  const selected = () => sx.messages.filter((m) => m.selected && m.complete);
  function saveSelected() {
    const sel = selected();
    if (!sel.length) return;
    saveBlob(sel.map((m) => m.bytes), `emm-sysex-${stamp()}.syx`);
  }

  function paintList() {
    if (!sx.messages.length) {
      replace(listSlot, h('p', { class: 'muted sx-empty' }, 'Nothing here yet. Receive a dump from a device, or load a .syx file from your computer.'));
    } else {
      replace(listSlot, h('div', { class: 'sx-scroll' }, h('table', { class: 'mon-table sx-table' },
        h('thead', {}, h('tr', {},
          h('th', {}, h('input', { type: 'checkbox', 'aria-label': 'Select all', checked: sx.messages.every((m) => m.selected || !m.complete), onchange: (e) => { for (const m of sx.messages) m.selected = e.target.checked && m.complete; paintList(); paintSend(); } })),
          h('th', {}, '#'), h('th', {}, 'From'), h('th', {}, 'Size'), h('th', {}, 'What'), h('th', { class: 'col-hex' }, 'Starts with'), h('th', {}, ''))),
        h('tbody', {}, sx.messages.map((m, i) => h('tr', {},
          h('td', {}, h('input', { type: 'checkbox', checked: m.selected, disabled: !m.complete, 'aria-label': `Message ${i + 1}`, onchange: (e) => { m.selected = e.target.checked; paintSummary(); paintSend(); } })),
          h('td', { class: 'mono' }, String(i + 1)),
          h('td', { class: 'port-cell', title: m.source }, m.source),
          h('td', { class: 'mono' }, fmtBytes(m.bytes.length)),
          h('td', {}, describe(m.bytes), m.complete ? null : [' ', badge('cut short', 'warn')]),
          h('td', { class: 'mono col-hex muted' }, hex(m.bytes.slice(0, 12)) + (m.bytes.length > 12 ? ' …' : '')),
          h('td', {}, h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Remove message ${i + 1}`, title: 'Remove', onclick: () => { sx.messages = sx.messages.filter((x) => x !== m); paintList(); paintSend(); } }, icon('x', 14)))))))));
    }
    paintSummary();
  }
  function paintSummary() {
    const all = sx.messages.length, sel = selected();
    const bytes = sel.reduce((n, m) => n + m.bytes.length, 0);
    summary.textContent = all ? `${fmtNum(all)} message${all === 1 ? '' : 's'}, ${fmtNum(sel.length)} selected (${fmtBytes(bytes)})` : '';
    saveBtn.disabled = !sel.length;
    clearBtn.disabled = !all;
  }

  const listCard = h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h3', {}, 'Messages'), h('div', { class: 'btn-row' }, loadBtn, saveBtn, clearBtn)),
    picker, summary, listSlot,
    h('p', { class: 'muted small' }, 'The list stays while this page is open in your browser; reloading the page empties it, so save what you want to keep.'));
  listCard.addEventListener('dragover', (e) => { if (e.dataTransfer && Array.from(e.dataTransfer.types).includes('Files')) e.preventDefault(); });
  listCard.addEventListener('drop', (e) => { if (e.dataTransfer && e.dataTransfer.files.length) { e.preventDefault(); loadFiles(e.dataTransfer.files); } });

  // ================= send =================
  let sending = null; // { ctl, port, base, total }
  const toSel = h('select', { id: 'sx-to', 'aria-label': 'Send to', onchange: () => { sx.toId = toSel.value; paintSend(); } });
  const gapSel = h('select', { id: 'sx-gap', 'aria-label': 'Pause between messages', onchange: () => { sx.gap = Number(gapSel.value); } },
    [0, 20, 50, 100, 250, 500, 1000].map((ms) => h('option', { value: ms }, ms ? `${ms} ms` : 'none')));
  gapSel.value = String(sx.gap);
  const sendBtn = h('button', { type: 'button', class: 'btn btn-primary', onclick: () => (sending ? sending.ctl.abort() : upload()) });
  const bar = h('progress', { max: 1, value: 0, hidden: true, 'aria-label': 'Progress of the transfer' });
  const sendStatus = h('p', { class: 'muted', role: 'status' });
  const noOutput = h('div', { class: 'notice notice-warn', role: 'note', hidden: true },
    icon('alert', 16), h('span', {}, 'No wired device that can receive MIDI is plugged in. Only USB devices can be used here, not network or Bluetooth ports.'));

  async function upload() {
    const sel = selected();
    if (!sel.length || !sx.toId) return;
    const total = sel.reduce((n, m) => n + m.bytes.length, 0);
    const body = new Uint8Array(total);
    let o = 0;
    for (const m of sel) { body.set(m.bytes, o); o += m.bytes.length; }
    const ctl = new AbortController();
    const port = S.byId.get(sx.toId);
    sending = { ctl, port: sx.toId, base: (port && port.stats && port.stats.tx_bytes) || 0, total };
    paintSend();
    try {
      const res = await fetch(`/api/ports/${sx.toId}/sysex?gap_ms=${sx.gap}`, {
        method: 'POST', body, signal: ctl.signal, credentials: 'same-origin', cache: 'no-store',
        headers: { 'Content-Type': 'application/octet-stream' },
      });
      let data = null;
      try { data = await res.json(); } catch (_) { /* no body */ }
      if (!res.ok) throw new Error((data && data.error) || res.statusText || `HTTP ${res.status}`);
      sendStatus.textContent = `Sent ${fmtNum(sel.length)} message${sel.length === 1 ? '' : 's'} (${fmtBytes(total)}) to ${displayName(S.byId.get(sending.port))}.`;
      toast('SysEx sent.');
    } catch (e) {
      if (e && e.name === 'AbortError') sendStatus.textContent = 'Cancelled. The device may have received part of it.';
      else { sendStatus.textContent = `Not sent: ${e.message}`; toast(`SysEx not sent: ${e.message}`, 'error'); }
    } finally {
      sending = null;
      paintSend();
    }
  }
  function paintProgress() {
    if (!sending) return;
    const p = S.byId.get(sending.port);
    const sent = Math.max(0, Math.min(sending.total, ((p && p.stats && p.stats.tx_bytes) || 0) - sending.base));
    bar.max = sending.total;
    bar.value = sent;
    sendStatus.textContent = `Sending… ${fmtBytes(sent)} of ${fmtBytes(sending.total)}`;
  }
  function paintSend() {
    const ports = wiredPorts('output');
    const labels = portLabels(ports);
    replace(toSel, ports.map((p) => h('option', { value: p.id }, labels.get(p.id))));
    if (!ports.some((p) => p.id === sx.toId)) sx.toId = ports.length ? ports[0].id : '';
    toSel.value = sx.toId;
    toSel.disabled = !!sending;
    gapSel.disabled = !!sending;
    noOutput.hidden = ports.length > 0;
    const sel = selected();
    replace(sendBtn, sending ? [icon('x', 14), 'Cancel'] : [icon('upload', 14), sel.length ? `Send ${fmtNum(sel.length)} selected` : 'Send selected']);
    sendBtn.disabled = !sending && (!sel.length || !sx.toId);
    bar.hidden = !sending;
    if (sending) paintProgress();
  }

  const sendCard = h('section', { class: 'card' },
    h('h3', {}, 'Send to a device'),
    h('p', { class: 'muted small' }, 'Sends the selected messages above. A dump can overwrite what is stored in the device, so check the device and the list first. Some devices need a pause between messages.'),
    noOutput,
    h('div', { class: 'sx-send' },
      h('div', { class: 'field' }, h('label', { for: 'sx-to' }, 'Device'), toSel),
      h('div', { class: 'field' }, h('label', { for: 'sx-gap' }, 'Pause between messages'), gapSel),
      sendBtn),
    bar, sendStatus);

  root.append(h('section', { class: 'sysex' },
    h('div', { class: 'sx-cols' }, receiveCard, sendCard), listCard));

  paintReceive();
  paintSend();
  paintList();
  const offTop = on('topology', () => { paintReceive(); if (!sending) paintSend(); });
  const offAct = on('activity', (a) => { if (sending && a.id === sending.port) paintProgress(); });
  cleanups.push(offTop, offAct);
  return () => {
    for (const c of cleanups) c();
    if (es) { es.close(); es = null; }
    if (sending) sending.ctl.abort();
  };
}
