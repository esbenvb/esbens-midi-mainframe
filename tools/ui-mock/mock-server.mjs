#!/usr/bin/env node
// Development mock of the emmd HTTP API (see docs/ARCHITECTURE.md), so the UI
// in internal/web/ui can be built and checked without the Go daemon.
//
//   node tools/ui-mock/mock-server.mjs [--port 8098]
//
// Environment:
//   MOCK_SETUP=1     start in setup (captive portal) mode
//   MOCK_PORTS=40    add that many extra simulated devices (performance testing)
//   MOCK_RO=1        report the settings storage as read-only
//   MOCK_MANAGED=0   no keys "managed by emm.txt"
//   MOCK_MANAGED=1   emm.txt fixes hostname and WiFi (network), RTP and the Bluetooth name (settings)
//   MOCK_MANAGED=all everything emm.txt can fix: hostname, wifi, ethernet, wifi_country; rtp, ble,
//                    ble_name, web_password. (Unset: only the hostname, as before.) Changing a fixed
//                    setting answers 409 like the daemon.
//   MOCK_SLOW_MS=20000  client-mode WiFi join, forget and Ethernet apply answer only after this long
//                    (the daemon blocks up to ~55 s on them). A join with a password starting with
//                    "wrong" fails after the delay; 8-63 characters or 64 hex digits are accepted.
//   MOCK_NO_BLE=1    Bluetooth MIDI on, but no adapter
//   MOCK_ADOPTED=1   client mode through a connection EMM did not create: status.adopted is true
//   MOCK_ODD_SSID=1  the scan also lists "Guest " (trailing space) and " Lounge" (leading space)
//   MOCK_SCAN_HANG=1 in setup mode a forced scan leaves the request hanging instead of dropping it
// A forced scan (POST /api/network/scan?force=1) in setup mode behaves like the daemon: the setup
// access point goes away for ~5 s (every request fails, the event stream drops), the cached list
// is refreshed meanwhile and gains a "Fresh-N" network so a fresh list can be told from the old one.
// Like the daemon, a join for a network that is secured in the scan but has an empty password is
// accepted (the daemon cannot tell) and then fails after the delay; so only the UI stops it early.
// Control endpoints (GET): /__mock/setup?on=1|0, /__mock/drop-sse, /__mock/wifi-posts (the bodies of
// the POST /api/network/wifi requests received so far, to check exactly what the UI sent)
//
// No dependencies; plain Node.

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const UI_DIR = path.resolve(here, '../../internal/web/ui');
const PORT = Number(process.argv.includes('--port') ? process.argv[process.argv.indexOf('--port') + 1] : process.env.PORT || 8098);

// ---------------------------------------------------------------- topology
const emptyStats = () => ({ rx_msgs: 0, rx_bytes: 0, tx_msgs: 0, tx_bytes: 0, dropped: 0, errors: 0, queued_bytes: 0, last_rx: 0, last_tx: 0 });
const ports = new Map();
let connections = [];
const DEFAULT_FILTER = () => ({ block: ['active_sense'], channels: [], map_channel: 0, transpose: 0 });

function addPort(p) {
  ports.set(p.id, { alias: '', kind: 'usb', group: '', input: true, output: true, detail: '', online: true, rtp: false, hidden: false, stats: emptyStats(), ...p });
  return ports.get(p.id);
}
function addUsb(id, name, group, { input = true, output = true, online = true, detail } = {}) {
  const p = addPort({ id, name, group, input, output, online, rtp: true, detail: detail || `USB ${id.split(':')[3]} · ${id.split(':')[1]}:${id.split(':')[2]}` });
  if (online) addRtpFor(p);
  return p;
}
function addRtpFor(p) {
  const id = 'rtp:' + p.id;
  if (ports.has(id)) return;
  addPort({ id, name: p.name, kind: 'rtp', group: '', input: true, output: true, detail: 'network' });
  if (!connections.some((c) => c.from === p.id && c.to === id) && p.input) connections.push({ from: p.id, to: id, filter: DEFAULT_FILTER(), online: true });
  if (!connections.some((c) => c.from === id && c.to === p.id) && p.output) connections.push({ from: id, to: p.id, filter: DEFAULT_FILTER(), online: true });
}

addUsb('usb:1235:0a01:1.1:0', 'Digitakt', 'Elektron Digitakt');
addUsb('usb:1235:0a02:1.2:0', 'Syntakt', 'Elektron Syntakt');
addUsb('usb:1235:0020:1.3:0', 'Launchpad Mini MK3 MIDI', 'Novation Launchpad Mini MK3');
addUsb('usb:1235:0020:1.3:1', 'Launchpad Mini MK3 DAW', 'Novation Launchpad Mini MK3');
addUsb('usb:0582:0007:1.4:0', 'JV-1080', 'Roland JV-1080');
addUsb('usb:1c75:0284:1.5:0', 'KeyStep 37', 'Arturia KeyStep 37', { output: false });
addUsb('usb:0000:0001:1.6:0', 'MIDI Thru Box', 'DIY MIDI Thru Box', { input: false });
addUsb('usb:0944:0115:1.7:0', 'Minilogue', 'Korg Minilogue', { online: false });
addPort({ id: 'rtp:net-daw-bus', name: 'DAW bus', kind: 'rtp', input: true, output: true, detail: 'network' });
addPort({ id: 'ble:0', name: 'EMM', kind: 'ble', input: true, output: true, detail: 'Bluetooth LE MIDI' });
ports.get('usb:0944:0115:1.7:0').rtp = true;

for (let i = 1; i <= Number(process.env.MOCK_PORTS || 0); i++) {
  const n = String(i).padStart(2, '0');
  addUsb(`usb:ffff:${n}00:2.${i}:0`, `Synth ${n}`, `Test Synth ${n}`);
}

const conn = (from, to, filter) => connections.push({ from, to, filter: filter || DEFAULT_FILTER(), online: true });
conn('usb:1c75:0284:1.5:0', 'usb:1235:0a01:1.1:0');
conn('usb:1c75:0284:1.5:0', 'usb:1235:0a02:1.2:0', { block: ['active_sense', 'sysex'], channels: [1, 2], map_channel: 5, transpose: 12 });
conn('usb:1235:0a01:1.1:0', 'usb:0582:0007:1.4:0', { block: ['active_sense', 'clock'], channels: [], map_channel: 0, transpose: 0 });
conn('usb:1235:0a01:1.1:0', 'usb:0000:0001:1.6:0');
conn('usb:0944:0115:1.7:0', 'usb:1235:0a01:1.1:0');
conn('rtp:net-daw-bus', 'usb:1235:0a02:1.2:0');
connections.forEach((c) => { c.online = !!(ports.get(c.from)?.online && ports.get(c.to)?.online); });

const portList = () => Array.from(ports.values()).sort((a, b) => (a.kind > b.kind ? 1 : a.kind < b.kind ? -1 : a.name.localeCompare(b.name)));
const topology = () => ({ version: '0.1.0-mock', hostname: net.hostname, now: Date.now(), ports: portList(), connections });

// ---------------------------------------------------------------- network
const MANAGED = process.env.MOCK_MANAGED;
const SLOW_MS = Number(process.env.MOCK_SLOW_MS || 0);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const BOOT_MANAGED_MSG = 'this setting is controlled by emm.txt on the SD card; edit or remove it there';
const bootManaged = () => (MANAGED === '0' ? [] : MANAGED === 'all' ? ['hostname', 'wifi', 'ethernet', 'wifi_country'] : MANAGED === '1' ? ['hostname', 'wifi'] : ['hostname']);
const settingsManaged = () => ({
  rtp: MANAGED === '1' || MANAGED === 'all',
  ble: MANAGED === 'all',
  ble_name: MANAGED === '1' || MANAGED === 'all',
  web_password: MANAGED === 'all',
});
let apDownUntil = 0; // the setup access point is off (a forced scan) until this time
let freshScans = 0;
const net = {
  joining: false,
  hostname: 'emm',
  setup_mode: process.env.MOCK_SETUP === '1',
  mode: process.env.MOCK_SETUP === '1' ? 'ap' : 'client',
  ssid: process.env.MOCK_SETUP === '1' ? '' : 'HomeWiFi-5G',
  ip: process.env.MOCK_SETUP === '1' ? '' : '192.168.1.50',
  ap_ssid: 'EMM-Setup-3F2A',
  last_error: '',
  ethernet: { link: false, ip: '', mode: 'auto' },
  saved_ssid: process.env.MOCK_SETUP === '1' ? '' : 'HomeWiFi-5G',
  scan_ts: Date.now() - 12000,
};
let ethSettings = { mode: 'auto', address: '', gateway: '', dns: [] };
const networks = [
  { ssid: 'HomeWiFi-5G', signal: 82, security: 'WPA2' },
  { ssid: 'HomeWiFi', signal: 74, security: 'WPA2' },
  { ssid: 'Studio-Guest', signal: 58, security: 'WPA2' },
  { ssid: 'Café Lounge', signal: 41, security: '' },
  { ssid: 'NETGEAR-7F', signal: 33, security: 'WPA2 WPA3' },
  { ssid: 'TeliaGateway-C9D2', signal: 22, security: 'WPA2' },
  { ssid: 'DIRECT-roku-514', signal: 12, security: 'WPA2' },
  { ssid: 'Hotel Wi-Fi 🎹', signal: 9, security: '' },
];
if (process.env.MOCK_ODD_SSID === '1') networks.push({ ssid: 'Guest ', signal: 66, security: 'WPA2' }, { ssid: ' Lounge', signal: 63, security: 'WPA2' });
const wifiPosts = [];
const netStatus = () => ({
  mode: net.mode, setup_mode: net.setup_mode, ssid: net.ssid, ip: net.ip, hostname: net.hostname,
  ap_ssid: net.setup_mode ? net.ap_ssid : '', last_error: net.last_error, ethernet: net.ethernet,
  scan_age: Math.max(0, Math.floor((Date.now() - net.scan_ts) / 1000)), joining: net.joining, adopted: process.env.MOCK_ADOPTED === '1' && net.mode === 'client', boot_managed: bootManaged(),
});
const netBody = () => ({
  status: netStatus(),
  networks: networks.map((n) => ({ ...n, in_use: n.ssid === net.ssid && net.mode === 'client' })),
  saved_ssid: net.saved_ssid,
  ethernet: ethSettings,
  boot_managed: bootManaged(),
});

let settings = {
  rtp: { enabled: true },
  ble: { enabled: true, name: 'EMM', available: process.env.MOCK_NO_BLE !== '1', advertising: process.env.MOCK_NO_BLE !== '1', connected: process.env.MOCK_NO_BLE !== '1' ? 1 : 0, error: '' },
  web: { password_set: MANAGED === 'all' },
};
// What GET /api/settings answers: like the daemon, Bluetooth is "unavailable" while it is switched off,
// and `managed` says which settings emm.txt fixes.
const settingsBody = () => ({ ...settings, managed: settingsManaged() });
const validWifiPassword = (p) => p === '' || (Buffer.byteLength(p) >= 8 && Buffer.byteLength(p) <= 63) || /^[0-9a-fA-F]{64}$/.test(p);

// ---------------------------------------------------------------- SSE
const clients = new Set();
function sse(res, event, data) { res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`); }
function broadcast(event, data, monitorOnly) {
  for (const c of clients) {
    if (monitorOnly && !c.monitor) continue;
    sse(c.res, event, data);
  }
}
setInterval(() => { for (const c of clients) c.res.write(': heartbeat\n\n'); }, 15000);

// ---------------------------------------------------------------- simulated MIDI traffic
const NOTE = ['C', 'C#', 'D', 'D#', 'E', 'F', 'F#', 'G', 'G#', 'A', 'A#', 'B'];
const noteName = (n) => `${NOTE[n % 12]}${Math.floor(n / 12) - 1}`;
const hex = (...b) => b.map((x) => x.toString(16).toUpperCase().padStart(2, '0')).join(' ');
function randomMessage() {
  const r = Math.random();
  const ch = Math.floor(Math.random() * 4);
  if (r < 0.45) { const n = 36 + Math.floor(Math.random() * 40), v = 20 + Math.floor(Math.random() * 100); return { desc: `Note On ch${ch + 1} ${noteName(n)} vel ${v}`, hex: hex(0x90 | ch, n, v), bytes: 3 }; }
  if (r < 0.8) { const n = 36 + Math.floor(Math.random() * 40); return { desc: `Note Off ch${ch + 1} ${noteName(n)}`, hex: hex(0x90 | ch, n, 0), bytes: 3 }; }
  if (r < 0.92) { const c = [1, 7, 10, 74][Math.floor(Math.random() * 4)], v = Math.floor(Math.random() * 128); return { desc: `CC ch${ch + 1} #${c} = ${v}`, hex: hex(0xB0 | ch, c, v), bytes: 3 }; }
  if (r < 0.97) return { desc: 'Clock', hex: 'F8', bytes: 1 };
  return { desc: `Pitch Bend ch${ch + 1} ${Math.floor(Math.random() * 16384) - 8192}`, hex: hex(0xE0 | ch, 0, 64), bytes: 3 };
}

const dirty = new Set();
function bump(id, dir, msgs, bytes) {
  const p = ports.get(id);
  if (!p || !p.online) return;
  p.stats[dir + '_msgs'] += msgs;
  p.stats[dir + '_bytes'] += bytes;
  p.stats['last_' + dir] = Date.now();
  dirty.add(id);
}
setInterval(() => {
  const sources = portList().filter((p) => p.online && p.input && p.kind !== 'ble');
  if (!sources.length) return;
  const picks = Math.random() < 0.7 ? 1 : 2;
  for (let i = 0; i < picks; i++) {
    const src = sources[Math.floor(Math.random() * sources.length)];
    const m = randomMessage();
    bump(src.id, 'rx', 1, m.bytes);
    broadcast('monitor', [{ t: new Date().toISOString(), port: src.id, dir: 'rx', desc: m.desc, hex: m.hex }], true);
    for (const c of connections) {
      if (c.from !== src.id || !c.online) continue;
      bump(c.to, 'tx', 1, m.bytes);
      broadcast('monitor', [{ t: new Date().toISOString(), port: c.to, dir: 'tx', from: src.id, desc: m.desc, hex: m.hex }], true);
    }
  }
}, 140);
setInterval(() => {
  if (!dirty.size) return;
  const out = {};
  for (const id of dirty) out[id] = ports.get(id).stats;
  dirty.clear();
  broadcast('activity', out);
}, 100);

// ---------------------------------------------------------------- helpers
const CATS = ['note', 'poly_at', 'cc', 'program', 'chan_at', 'bend', 'sysex', 'clock', 'transport', 'active_sense', 'reset', 'sys_common', 'other'];
function validateFilter(f) {
  f = f || {};
  for (const b of f.block || []) if (!CATS.includes(b)) return `unknown category "${b}"`;
  for (const c of f.channels || []) if (!(c >= 1 && c <= 16)) return `channel ${c} out of range 1..16`;
  if (f.map_channel && !(f.map_channel >= 1 && f.map_channel <= 16)) return `map_channel ${f.map_channel} out of range 1..16`;
  if (f.transpose && !(f.transpose >= -48 && f.transpose <= 48)) return `transpose ${f.transpose} out of range -48..48`;
  return '';
}
const norm = (f) => {
  let ch = Array.from(new Set(f.channels || [])).sort((a, b) => a - b);
  if (ch.length === 16) ch = [];
  return { block: Array.from(new Set(f.block || [])), channels: ch, map_channel: f.map_channel || 0, transpose: f.transpose || 0 };
};

function json(res, status, body) {
  const s = body === undefined ? '' : JSON.stringify(body);
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(s);
}
const err = (res, status, message) => json(res, status, { error: message });
function readBody(req) {
  return new Promise((resolve) => {
    let s = '';
    req.on('data', (c) => { s += c; });
    req.on('end', () => { try { resolve(s ? JSON.parse(s) : {}); } catch { resolve(null); } });
  });
}
function topoChanged() { broadcast('topology', topology()); }

const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml', '.json': 'application/json', '.ico': 'image/x-icon' };

// ---------------------------------------------------------------- server
const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  const p = url.pathname;
  const m = req.method;

  // The setup access point is off while a forced scan runs: nothing answers.
  if (Date.now() < apDownUntil && p.startsWith('/api/')) { req.socket.destroy(); return; }

  if (p.startsWith('/__mock/')) {
    if (p === '/__mock/setup') {
      const on = url.searchParams.get('on') !== '0';
      net.setup_mode = on; net.mode = on ? 'ap' : 'client'; net.ssid = on ? '' : 'HomeWiFi-5G'; net.ip = on ? '' : '192.168.1.50'; net.last_error = '';
      broadcast('network', netBody());
      return json(res, 200, { setup_mode: on });
    }
    if (p === '/__mock/wifi-posts') return json(res, 200, wifiPosts);
    if (p === '/__mock/drop-sse') { for (const c of clients) c.res.end(); clients.clear(); return json(res, 200, { dropped: true }); }
    return err(res, 404, 'unknown mock control');
  }

  if (p.startsWith('/api/')) {
    // ---- events
    if (p === '/api/events' && m === 'GET') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', Connection: 'keep-alive', 'X-Accel-Buffering': 'no' });
      const c = { res, monitor: url.searchParams.get('monitor') === '1' };
      clients.add(c);
      req.on('close', () => clients.delete(c));
      sse(res, 'topology', topology());
      return;
    }
    if (p === '/api/state' && m === 'GET') return json(res, 200, topology());

    // ---- connections
    if (p === '/api/connections' && m === 'PUT') {
      const b = await readBody(req);
      if (!b || !b.from || !b.to) return err(res, 400, 'from and to are required');
      if (b.from === b.to) return err(res, 400, 'source and destination are the same port');
      const a = ports.get(b.from), z = ports.get(b.to);
      if (a && !a.input) return err(res, 400, 'source port cannot send MIDI');
      if (z && !z.output) return err(res, 400, 'destination port cannot receive MIDI');
      const bad = validateFilter(b.filter);
      if (bad) return err(res, 400, `filter: ${bad}`);
      const f = norm(b.filter || DEFAULT_FILTER());
      let c = connections.find((x) => x.from === b.from && x.to === b.to);
      if (!c) { c = { from: b.from, to: b.to }; connections.push(c); }
      c.filter = f;
      c.online = !!(a?.online && z?.online);
      topoChanged();
      return json(res, 200, c);
    }
    if (p === '/api/connections' && m === 'DELETE') {
      const from = url.searchParams.get('from'), to = url.searchParams.get('to');
      const n = connections.length;
      connections = connections.filter((c) => !(c.from === from && c.to === to));
      if (connections.length === n) return err(res, 404, 'no such connection');
      topoChanged();
      return json(res, 200, { ok: true });
    }

    // ---- ports
    let mm;
    if ((mm = p.match(/^\/api\/ports\/([^/]+)$/)) && m === 'PATCH') {
      const port = ports.get(decodeURIComponent(mm[1]));
      if (!port) return err(res, 404, 'unknown port');
      const b = await readBody(req);
      if (!b) return err(res, 400, 'bad JSON');
      if (typeof b.alias === 'string') {
        if ([...b.alias.trim()].length > 60) return err(res, 400, 'alias is too long (max 60 characters)');
        port.alias = b.alias.trim();
      }
      if (typeof b.hidden === 'boolean') port.hidden = b.hidden;
      if (typeof b.rtp === 'boolean' && (port.kind === 'usb')) {
        port.rtp = b.rtp;
        if (b.rtp) addRtpFor(port);
        else { ports.delete('rtp:' + port.id); connections = connections.filter((c) => c.from !== 'rtp:' + port.id && c.to !== 'rtp:' + port.id); }
      }
      topoChanged();
      return json(res, 200, port);
    }
    if ((mm = p.match(/^\/api\/ports\/([^/]+)\/(panic|test-note)$/)) && m === 'POST') {
      const port = ports.get(decodeURIComponent(mm[1]));
      if (!port) return err(res, 404, 'unknown port');
      if (!port.output) return err(res, 400, 'port cannot receive MIDI');
      bump(port.id, 'tx', mm[2] === 'panic' ? 48 : 2, mm[2] === 'panic' ? 144 : 6);
      return json(res, 200, { ok: true });
    }

    // ---- rtp
    if (p === '/api/rtp' && m === 'GET') {
      let port = 5004;
      const list = portList().filter((x) => x.kind === 'rtp').map((x) => ({
        id: x.id, name: x.alias || x.name, control_port: (port += 2) - 2,
        peers: x.id.length % 3 === 0 ? 1 : 0, device: x.id === 'rtp:net-daw-bus' || x.id.startsWith('rtp:net-') ? '' : x.id.slice(4),
      }));
      return json(res, 200, list);
    }
    if (p === '/api/rtp/ports' && m === 'POST') {
      const b = await readBody(req);
      const name = b && typeof b.name === 'string' ? b.name.trim() : '';
      if (!name || [...name].length > 40) return err(res, 400, 'name is required (max 40 characters)');
      const id = 'rtp:net-' + name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
      if (ports.has(id)) return err(res, 409, 'a network port with that name exists');
      addPort({ id, name, kind: 'rtp', detail: 'network' });
      topoChanged();
      return json(res, 201, { id });
    }
    if ((mm = p.match(/^\/api\/rtp\/ports\/([^/]+)$/)) && m === 'DELETE') {
      const id = decodeURIComponent(mm[1]);
      if (!id.startsWith('rtp:net-') || !ports.has(id)) return err(res, 404, 'no such network port');
      ports.delete(id);
      connections = connections.filter((c) => c.from !== id && c.to !== id);
      topoChanged();
      return json(res, 200, { ok: true });
    }

    // ---- settings
    if (p === '/api/settings' && m === 'GET') return json(res, 200, settingsBody());
    if (p === '/api/settings' && m === 'PUT') {
      const b = await readBody(req);
      if (!b) return err(res, 400, 'bad JSON');
      const fixed = settingsManaged();
      if ((b.rtp && typeof b.rtp.enabled === 'boolean' && fixed.rtp)
        || (b.ble && typeof b.ble.enabled === 'boolean' && fixed.ble)
        || (b.ble && typeof b.ble.name === 'string' && fixed.ble_name)
        || (b.web && typeof b.web.password === 'string' && fixed.web_password)) return err(res, 409, BOOT_MANAGED_MSG);
      if (b.ble && typeof b.ble.name === 'string' && (!b.ble.name.trim() || [...b.ble.name.trim()].length > 20)) return err(res, 400, 'Bluetooth name must be 1–20 characters');
      if (b.rtp && typeof b.rtp.enabled === 'boolean') settings.rtp.enabled = b.rtp.enabled;
      if (b.ble) {
        if (typeof b.ble.enabled === 'boolean') {
          settings.ble.enabled = b.ble.enabled;
          const adapter = process.env.MOCK_NO_BLE !== '1';
          settings.ble.available = b.ble.enabled && adapter;
          settings.ble.advertising = b.ble.enabled && adapter;
        }
        if (typeof b.ble.name === 'string') settings.ble.name = b.ble.name.trim();
      }
      if (b.web && typeof b.web.password === 'string') settings.web.password_set = b.web.password !== '';
      broadcast('settings', settingsBody());
      return json(res, 200, settingsBody());
    }

    // ---- network
    if (p === '/api/network' && m === 'GET') return json(res, 200, netBody());
    if (p === '/api/network/scan' && m === 'POST') {
      const force = url.searchParams.get('force') === '1';
      const freshen = () => { net.scan_ts = Date.now(); freshScans++; networks.push({ ssid: `Fresh-${freshScans}`, signal: 60, security: 'WPA2' }); };
      if (force && net.setup_mode) {
        // The radio leaves AP mode to scan: the setup network is gone for ~5 s.
        apDownUntil = Date.now() + 5000;
        for (const c of clients) c.res.end();
        clients.clear();
        setTimeout(freshen, 1500);
        if (process.env.MOCK_SCAN_HANG !== '1') req.socket.destroy();
        return; // hang: never answered
      }
      await sleep(900);
      if (force) { freshen(); return json(res, 200, { networks: netBody().networks, stale: false }); }
      if (!net.setup_mode) net.scan_ts = Date.now();
      return json(res, 200, { networks: netBody().networks, stale: net.setup_mode });
    }
    if (p === '/api/network/wifi' && m === 'POST') {
      const b = await readBody(req);
      if (b) wifiPosts.push({ ssid: b.ssid, password: b.password });
      if (!b || !b.ssid) return err(res, 400, 'ssid is required');
      if (bootManaged().includes('wifi')) return err(res, 409, BOOT_MANAGED_MSG);
      if (!validWifiPassword(b.password || '')) return err(res, 400, 'WPA password must be 8–63 characters or 64 hex digits (leave empty for an open network)');
      // The daemon accepts an empty password for any SSID; a secured network just fails to join.
      const known = networks.find((n) => n.ssid === b.ssid);
      const doomed = (b.password || '').startsWith('wrong') || !!(known && known.security && !b.password);
      if (!net.setup_mode) {
        // Client mode: the daemon joins synchronously and answers when it is done (or has given up).
        net.joining = true;
        await sleep(SLOW_MS || 1500);
        net.joining = false;
        if (doomed) {
          net.last_error = `Couldn't join “${b.ssid}”: authentication failed, wrong password?`;
          return err(res, 502, net.last_error);
        }
        net.mode = 'client'; net.ssid = b.ssid; net.ip = '192.168.1.50'; net.saved_ssid = b.ssid; net.last_error = '';
        broadcast('network', netBody());
        return json(res, 202, { status: netStatus() });
      }
      const wasSetup = net.setup_mode;
      const prev = { mode: net.mode, ssid: net.ssid, ip: net.ip };
      net.last_error = '';
      setTimeout(() => { net.mode = 'connecting'; broadcast('network', netBody()); }, 300);
      setTimeout(() => {
        if (doomed) {
          net.last_error = 'Authentication failed: wrong password?';
          net.mode = wasSetup ? 'ap' : prev.mode; net.ssid = prev.ssid; net.ip = prev.ip;
        } else {
          net.mode = 'client'; net.ssid = b.ssid; net.ip = '192.168.1.50'; net.setup_mode = false; net.saved_ssid = b.ssid; net.last_error = '';
        }
        broadcast('network', netBody());
      }, 2600);
      return json(res, 202, { status: netStatus() });
    }
    if (p === '/api/network/wifi' && m === 'DELETE') {
      if (bootManaged().includes('wifi')) return err(res, 409, BOOT_MANAGED_MSG);
      await sleep(SLOW_MS);
      net.saved_ssid = '';
      return json(res, 200, { ok: true });
    }
    if (p === '/api/network/ethernet' && m === 'PUT') {
      const b = await readBody(req);
      if (!b || !['auto', 'dhcp', 'static', 'off'].includes(b.mode)) return err(res, 400, 'mode must be auto, dhcp, static or off');
      if (bootManaged().includes('ethernet')) return err(res, 409, BOOT_MANAGED_MSG);
      await sleep(SLOW_MS);
      ethSettings = { mode: b.mode, address: b.address || '', gateway: b.gateway || '', dns: b.dns || [] };
      net.ethernet.mode = b.mode;
      return json(res, 200, ethSettings);
    }
    if (p === '/api/network/hostname' && m === 'PUT') {
      const b = await readBody(req);
      if (!b || !/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(b.hostname || '')) return err(res, 400, 'invalid hostname');
      if (bootManaged().includes('hostname')) return err(res, 409, BOOT_MANAGED_MSG);
      net.hostname = b.hostname;
      broadcast('network', netBody());
      topoChanged();
      return json(res, 200, { hostname: net.hostname });
    }

    // ---- system
    if (p === '/api/system' && m === 'GET') {
      return json(res, 200, {
        version: '0.1.0-mock', uptime_s: 3 * 86400 + Math.round(process.uptime()), model: 'Raspberry Pi 4 Model B Rev 1.4 (mock)', kernel: '6.12.25-v8+',
        cpu_temp_c: 48 + Math.round(Math.random() * 80) / 10, usb_devices: portList().filter((x) => x.kind === 'usb' && x.online).length, allow_power: true,
        backend: 'sim', state_writable: process.env.MOCK_RO !== '1',
      });
    }
    if ((p === '/api/system/reboot' || p === '/api/system/shutdown') && m === 'POST') return json(res, 200, { ok: true });

    return err(res, 404, `no such endpoint: ${m} ${p}`);
  }

  // ---- static files
  let file = p === '/' || p === '/setup' || p === '/setup/' ? '/index.html' : p;
  const full = path.join(UI_DIR, path.normalize(file));
  if (!full.startsWith(UI_DIR)) { res.writeHead(403); return res.end(); }
  fs.readFile(full, (e, data) => {
    if (e) { res.writeHead(404, { 'Content-Type': 'text/plain' }); return res.end('not found'); }
    res.writeHead(200, { 'Content-Type': MIME[path.extname(full)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
    res.end(data);
  });
});

server.listen(PORT, '127.0.0.1', () => console.log(`EMM UI mock on http://127.0.0.1:${PORT}  (setup mode: ${net.setup_mode})`));
