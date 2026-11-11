// Client-side state: topology (ports + connections), live activity counters,
// the SSE connection, and optimistic connection edits with rollback.

import { api, get } from './api.js';

// ---- tiny event bus --------------------------------------------------------
const listeners = {};
export function on(evt, fn) {
  (listeners[evt] = listeners[evt] || new Set()).add(fn);
  return () => listeners[evt].delete(fn);
}
export function emit(evt, data) {
  for (const fn of Array.from(listeners[evt] || [])) {
    try { fn(data); } catch (e) { console.error(`listener for ${evt} failed`, e); }
  }
}

// ---- state -----------------------------------------------------------------
export const S = {
  ports: [],
  connections: [],
  byId: new Map(),
  connMap: new Map(),
  hostname: 'emm',
  version: '',
  sse: 'connecting', // connecting | open | reconnecting | closed
  system: null,
  network: null,
};

export const ck = (from, to) => from + '\u0000' + to;
export const displayName = (p) => (p && (p.alias || p.name || p.id)) || '?';
export const portById = (id) => S.byId.get(id);
export const nameOfId = (id) => (S.byId.has(id) ? displayName(S.byId.get(id)) : id);
// How a port's kind is named for people (USB, Bluetooth, ...).
export const KIND_LABEL = { usb: 'USB', rtp: 'RTP-MIDI', ble: 'Bluetooth', virtual: 'Virtual', sim: 'Simulated' };
export const kindLabel = (kind) => KIND_LABEL[kind] || kind || '';
export const isNetworkPort = (p) => p.kind === 'rtp' || p.kind === 'ble';

// ---- filters ---------------------------------------------------------------
export const CATEGORIES = [
  ['note', 'Notes', 'Note on / off'],
  ['poly_at', 'Poly aftertouch', 'Per-note pressure'],
  ['cc', 'Control change', 'Knobs, faders, pedals'],
  ['program', 'Program change', 'Patch selection'],
  ['chan_at', 'Channel aftertouch', 'Channel pressure'],
  ['bend', 'Pitch bend', ''],
  ['sysex', 'SysEx', 'Patch and sample dumps'],
  ['clock', 'Clock', '24 ppqn timing'],
  ['transport', 'Start / Stop', 'Start, continue, stop'],
  ['active_sense', 'Active sensing', 'Keep-alive pings'],
  ['reset', 'System reset', ''],
  ['sys_common', 'System common', 'MTC, song position/select, tune'],
  ['other', 'Other', 'Undefined status bytes'],
];

export const DEFAULT_FILTER = () => ({ block: ['active_sense'], channels: [], map_channel: 0, transpose: 0 });

export function normFilter(f) {
  f = f || {};
  let channels = Array.from(new Set(f.channels || [])).filter((c) => c >= 1 && c <= 16).sort((a, b) => a - b);
  if (channels.length === 16) channels = [];
  return {
    block: Array.from(new Set(f.block || [])).sort(),
    channels,
    map_channel: f.map_channel | 0,
    transpose: f.transpose | 0,
  };
}

export function isDefaultFilter(f) {
  const n = normFilter(f);
  return n.block.length === 1 && n.block[0] === 'active_sense' && !n.channels.length && !n.map_channel && !n.transpose;
}

export function filterSummary(f) {
  const n = normFilter(f);
  const parts = [];
  if (n.block.length) parts.push('blocks ' + n.block.join(', '));
  if (n.channels.length) parts.push('channels ' + n.channels.join(','));
  if (n.map_channel) parts.push('→ ch ' + n.map_channel);
  if (n.transpose) parts.push((n.transpose > 0 ? '+' : '') + n.transpose + ' st');
  return parts.join(' · ') || 'passes everything';
}

// ---- topology --------------------------------------------------------------
function rebuild() {
  S.byId = new Map(S.ports.map((p) => [p.id, p]));
  S.connMap = new Map(S.connections.map((c) => [ck(c.from, c.to), c]));
}

export function applyTopology(t) {
  S.ports = (t.ports || []).map((p) => ({ ...p, stats: p.stats || {} }));
  S.connections = (t.connections || []).map((c) => ({ ...c, filter: normFilter(c.filter) }));
  if (t.hostname) S.hostname = t.hostname;
  if (t.version) S.version = t.version;
  rebuild();
  emit('topology', t);
}

export function applyActivity(a) {
  for (const [id, st] of Object.entries(a || {})) {
    const p = S.byId.get(id);
    if (!p) continue;
    const prev = p.stats || {};
    const rx = (st.rx_msgs ?? prev.rx_msgs ?? 0) > (prev.rx_msgs ?? 0);
    const tx = (st.tx_msgs ?? prev.tx_msgs ?? 0) > (prev.tx_msgs ?? 0);
    p.stats = { ...prev, ...st };
    emit('activity', { id, rx, tx, port: p });
  }
}

export async function refreshTopology() {
  applyTopology(await get('/api/state'));
}

// ---- optimistic connection edits ------------------------------------------
function setLocal(conn) {
  const i = S.connections.findIndex((c) => c.from === conn.from && c.to === conn.to);
  if (i >= 0) S.connections[i] = conn; else S.connections.push(conn);
  rebuild();
  emit('topology', { local: true });
}
function delLocal(from, to) {
  S.connections = S.connections.filter((c) => !(c.from === from && c.to === to));
  rebuild();
  emit('topology', { local: true });
}

export async function connect(from, to, filter) {
  const prev = S.connMap.get(ck(from, to));
  const f = normFilter(filter);
  const online = (S.byId.get(from) || {}).online !== false && (S.byId.get(to) || {}).online !== false;
  setLocal({ from, to, filter: f, online });
  try {
    const res = await api('PUT', '/api/connections', { from, to, filter: f });
    if (res && res.from === from && res.to === to) setLocal({ ...res, filter: normFilter(res.filter) });
  } catch (e) {
    if (prev) setLocal(prev); else delLocal(from, to);
    throw e;
  }
}

export async function disconnect(from, to) {
  const prev = S.connMap.get(ck(from, to));
  delLocal(from, to);
  try {
    await api('DELETE', `/api/connections?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`);
  } catch (e) {
    if (prev) setLocal(prev);
    throw e;
  }
}

// ---- server-sent events ----------------------------------------------------
let es = null;

function setSse(state) {
  if (S.sse !== state) { S.sse = state; emit('sse', state); }
}

export function startEvents() {
  if (es) es.close();
  if (typeof EventSource === 'undefined') { setSse('closed'); return; }
  let hadError = false;
  es = new EventSource('/api/events');
  es.addEventListener('open', () => {
    setSse('open');
    if (hadError) { hadError = false; resync(); }
  });
  es.addEventListener('error', () => {
    hadError = true;
    setSse(es.readyState === EventSource.CLOSED ? 'closed' : 'reconnecting');
    // Browsers retry on their own; if the stream was closed outright, try again.
    if (es.readyState === EventSource.CLOSED) setTimeout(startEvents, 3000);
  });
  es.addEventListener('topology', (e) => safe(() => applyTopology(JSON.parse(e.data))));
  es.addEventListener('activity', (e) => safe(() => applyActivity(JSON.parse(e.data))));
  es.addEventListener('settings', (e) => safe(() => emit('settings', parse(e.data))));
  es.addEventListener('presets', (e) => safe(() => emit('presets', parse(e.data))));
  es.addEventListener('network', (e) => safe(() => {
    const body = parse(e.data);
    if (body && body.status) S.network = body;
    emit('network', body);
  }));
}

function parse(s) { try { return JSON.parse(s); } catch (_) { return null; } }
function safe(fn) { try { fn(); } catch (e) { console.error(e); } }

async function resync() {
  try { await refreshTopology(); } catch (e) { console.warn('resync failed', e); }
  emit('resync');
}
