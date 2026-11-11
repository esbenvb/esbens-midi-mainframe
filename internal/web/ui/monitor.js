// Live MIDI monitor. Uses its own EventSource with ?monitor=1, opened only
// while this tab is visible (the daemon only produces monitor entries while
// someone is listening).

import { h, clear, replace } from './dom.js';
import { icon } from './icons.js';
import { S, on, displayName, nameOfId, kindLabel } from './state.js';

const MAX_ROWS = 500;     // rows kept in the DOM
const MAX_RAW = 1500;     // entries kept for re-filtering
const MAX_PAUSED = 3000;  // entries buffered while paused

function fmtTime(t) {
  let d = typeof t === 'number' ? new Date(t) : new Date(String(t));
  if (isNaN(d)) d = new Date();
  const p = (n, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

export function mount(root) {
  let es = null;
  let paused = false;
  let auto = true;
  let portFilter = '';
  let dirFilter = '';
  let raw = [];
  let pausedBuf = [];
  let incoming = [];
  let raf = 0;
  let status = 'connecting';

  const tbody = h('tbody');
  const scroller = h('div', { class: 'mon-scroll', tabindex: '0', 'aria-label': 'MIDI message log' },
    h('table', { class: 'mon-table' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Time'), h('th', {}, 'Dir'), h('th', {}, 'Port'), h('th', {}, 'Message'), h('th', { class: 'col-hex' }, 'Hex'))),
      tbody));
  const emptyMsg = h('p', { class: 'mon-empty muted' }, 'Waiting for MIDI… play a note or wiggle a knob on a connected device.');
  const statusEl = h('span', { class: 'mon-status muted small', role: 'status' });
  const portSel = h('select', { 'aria-label': 'Filter by port', onchange: (e) => { portFilter = e.target.value; rebuild(); } });
  const dirSel = h('select', { 'aria-label': 'Filter by direction', onchange: (e) => { dirFilter = e.target.value; rebuild(); } },
    h('option', { value: '' }, 'rx + tx'), h('option', { value: 'rx' }, 'received (rx)'), h('option', { value: 'tx' }, 'sent (tx)'));
  const pauseBtn = h('button', { type: 'button', class: 'btn', onclick: () => setPaused(!paused) });
  const clearBtn = h('button', { type: 'button', class: 'btn', onclick: () => { raw = []; pausedBuf = []; incoming = []; clear(tbody); updateEmpty(); updatePause(); } }, icon('trash', 14), 'Clear');
  const jump = h('button', { type: 'button', class: 'btn btn-primary jump', hidden: true, onclick: () => { auto = true; scroller.scrollTop = scroller.scrollHeight; jump.hidden = true; } }, icon('arrowDown', 14), 'Jump to live');

  function fillPortSel() {
    const cur = portSel.value;
    replace(portSel, h('option', { value: '' }, 'All ports'),
      // The type after the name tells apart a device's USB port from its network endpoint, and
      // two devices with the same name.
      S.ports.map((p) => h('option', { value: p.id }, `${displayName(p)} (${kindLabel(p.kind)})`)));
    portSel.value = S.byId.has(cur) ? cur : '';
    portFilter = portSel.value;
  }

  function updatePause() {
    replace(pauseBtn, icon(paused ? 'play' : 'pause', 14), paused ? `Resume${pausedBuf.length ? ` (${pausedBuf.length} new)` : ''}` : 'Pause');
    pauseBtn.setAttribute('aria-pressed', String(paused));
  }
  function updateStatus() {
    statusEl.textContent = status === 'open' ? 'live' : status === 'connecting' ? 'connecting…' : 'reconnecting…';
    statusEl.classList.toggle('stat-warn', status !== 'open');
  }
  function updateEmpty() { emptyMsg.hidden = tbody.childElementCount > 0; }

  function pass(e) {
    if (dirFilter && e.dir !== dirFilter) return false;
    if (portFilter && e.port !== portFilter && e.from !== portFilter) return false;
    return true;
  }

  function rowEl(e) {
    const portName = nameOfId(e.port);
    return h('tr', { class: `dir-${e.dir}` },
      h('td', { class: 'mono t' }, fmtTime(e.t)),
      h('td', {}, h('span', { class: 'dir-tag' }, e.dir === 'rx' ? '← rx' : '→ tx')),
      h('td', { class: 'port-cell', title: e.port }, portName, e.dir === 'tx' && e.from ? h('small', { class: 'muted' }, ` ← ${nameOfId(e.from)}`) : null),
      h('td', {}, e.desc),
      h('td', { class: 'mono col-hex muted' }, e.hex));
  }

  function flush() {
    raf = 0;
    if (!incoming.length) return;
    const batch = incoming;
    incoming = [];
    const frag = document.createDocumentFragment();
    for (const e of batch) {
      raw.push(e);
      if (pass(e)) frag.appendChild(rowEl(e));
    }
    if (raw.length > MAX_RAW) raw.splice(0, raw.length - MAX_RAW);
    tbody.appendChild(frag);
    while (tbody.childElementCount > MAX_ROWS) tbody.removeChild(tbody.firstChild);
    updateEmpty();
    if (auto) scroller.scrollTop = scroller.scrollHeight;
  }

  function rebuild() {
    clear(tbody);
    const frag = document.createDocumentFragment();
    for (const e of raw.filter(pass).slice(-MAX_ROWS)) frag.appendChild(rowEl(e));
    tbody.appendChild(frag);
    updateEmpty();
    if (auto) scroller.scrollTop = scroller.scrollHeight;
  }

  function setPaused(v) {
    if (v && raf) { cancelAnimationFrame(raf); flush(); } // show what already arrived, then freeze
    paused = v;
    if (!paused && pausedBuf.length) { incoming.push(...pausedBuf); pausedBuf = []; schedule(); }
    updatePause();
  }

  function schedule() { if (!raf) raf = requestAnimationFrame(flush); }

  function onMonitor(ev) {
    let arr;
    try { arr = JSON.parse(ev.data); } catch (_) { return; }
    if (!Array.isArray(arr)) arr = [arr];
    if (paused) {
      pausedBuf.push(...arr);
      if (pausedBuf.length > MAX_PAUSED) pausedBuf.splice(0, pausedBuf.length - MAX_PAUSED);
      updatePause();
    } else {
      incoming.push(...arr);
      // A hidden tab never runs requestAnimationFrame; don't let the backlog grow without bound.
      if (incoming.length > MAX_RAW) incoming.splice(0, incoming.length - MAX_RAW);
      schedule();
    }
  }

  function open() {
    if (typeof EventSource === 'undefined') { status = 'closed'; updateStatus(); return; }
    es = new EventSource('/api/events?monitor=1');
    es.addEventListener('open', () => { status = 'open'; updateStatus(); });
    es.addEventListener('error', () => { status = 'reconnecting'; updateStatus(); });
    es.addEventListener('monitor', onMonitor);
  }

  scroller.addEventListener('scroll', () => {
    auto = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 28;
    jump.hidden = auto;
  });

  root.append(h('section', { class: 'monitor' },
    h('div', { class: 'toolbar' },
      h('div', { class: 'field inline' }, h('label', {}, 'Port', portSel)),
      h('div', { class: 'field inline' }, h('label', {}, 'Direction', dirSel)),
      pauseBtn, clearBtn, statusEl),
    h('div', { class: 'mon-wrap' }, scroller, emptyMsg, jump)));

  fillPortSel();
  updatePause();
  updateStatus();
  updateEmpty();
  open();
  const off = on('topology', () => { const v = portSel.value; fillPortSel(); if (portSel.value !== v) rebuild(); });
  return () => { off(); if (es) es.close(); if (raf) cancelAnimationFrame(raf); };
}
