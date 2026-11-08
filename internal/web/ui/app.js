// Entry point: decides between the setup flow and the dashboard, draws the
// header and tabs, and mounts one tab at a time.

import { h, clear, replace } from './dom.js';
import { icon } from './icons.js';
import { get } from './api.js';
import { S, on, startEvents, refreshTopology } from './state.js';
import { toast } from './ui.js';
import { initTheme, toggleTheme, currentTheme } from './theme.js';
import './leds.js';
import * as matrix from './matrix.js';
import * as devices from './devices.js';
import * as monitor from './monitor.js';
import * as network from './network.js';
import * as system from './system.js';
import { mountSetup } from './setup.js';

const TABS = [
  ['matrix', 'Matrix', matrix],
  ['devices', 'Devices', devices],
  ['monitor', 'Monitor', monitor],
  ['network', 'Network', network],
  ['system', 'System', system],
];

const root = document.getElementById('app');
initTheme();
boot();

async function boot() {
  let net = null;
  try { net = await get('/api/network', { timeout: 6000 }); } catch (_) { /* dashboard will show the connection problem */ }
  const path = location.pathname.replace(/\/+$/, '');
  if (path === '/setup' || (net && net.status && net.status.setup_mode)) {
    mountSetup(root, net);
    return;
  }
  S.network = net;
  mountApp();
}

function mountApp() {
  const hostEl = h('span', { class: 'host mono' }, `${S.hostname}.local`);
  const conn = h('span', { class: 'conn', role: 'status' });
  const themeBtn = h('button', { type: 'button', class: 'icon-btn', onclick: () => { toggleTheme(); paintTheme(); } });
  const banners = h('div', { class: 'banners' });
  const view = h('main', { id: 'view', tabindex: '-1' });
  const nav = h('nav', { class: 'tabs', 'aria-label': 'Sections' },
    TABS.map(([id, label]) => h('a', { href: '#' + id, dataset: { tab: id } }, label)));

  function paintTheme() {
    const light = currentTheme() === 'light';
    replace(themeBtn, icon(light ? 'moon' : 'sun', 18));
    themeBtn.setAttribute('aria-label', light ? 'Switch to dark theme' : 'Switch to light theme');
    themeBtn.title = themeBtn.getAttribute('aria-label');
  }
  function paintConn() {
    const labels = { open: 'live', connecting: 'connecting…', reconnecting: 'reconnecting…', closed: 'offline' };
    conn.dataset.state = S.sse;
    replace(conn, h('span', { class: 'conn-dot', 'aria-hidden': 'true' }), labels[S.sse] || S.sse);
  }
  function paintBanners() {
    const items = [];
    if (S.sse === 'reconnecting' || S.sse === 'closed') {
      items.push(h('div', { class: 'notice notice-warn', role: 'alert' }, icon('alert', 16), h('span', {}, 'Lost contact with the device. Trying to reconnect… what you see may be out of date.')));
    }
    if (S.system && S.system.state_writable === false) {
      items.push(h('div', { class: 'notice notice-warn', role: 'note' }, icon('alert', 16), h('span', {}, 'The settings storage is read-only or unreadable: changes won’t survive a restart.')));
    }
    replace(banners, items);
  }

  root.append(
    h('div', { class: 'app' },
      h('header', { class: 'topbar' },
        h('div', { class: 'brand' },
          h('span', { class: 'mark', 'aria-hidden': 'true' }, 'EMM'),
          h('div', { class: 'brand-text' }, h('h1', {}, 'Esben’s MIDI Mainframe'), hostEl)),
        h('div', { class: 'top-right' }, conn, themeBtn)),
      nav, banners, view));

  // ---- tab routing
  let cleanup = null;
  let first = true;
  function show() {
    const id = (location.hash || '#matrix').slice(1);
    const tab = TABS.find((t) => t[0] === id) || TABS[0];
    if (cleanup) { try { cleanup(); } catch (e) { console.error(e); } cleanup = null; }
    clear(view);
    for (const a of nav.children) {
      if (a.dataset.tab === tab[0]) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
    document.title = `${tab[1]} · Esben’s MIDI Mainframe`;
    view.dataset.tab = tab[0];
    cleanup = tab[2].mount(view) || null;
    if (!first) view.focus({ preventScroll: true });
    first = false;
  }
  window.addEventListener('hashchange', show);

  paintTheme();
  paintConn();
  on('sse', () => { paintConn(); paintBanners(); });
  on('system', () => paintBanners());
  on('topology', () => { const t = `${S.hostname}.local`; if (hostEl.textContent !== t) hostEl.textContent = t; });
  on('network', (body) => {
    if (body && body.status && body.status.setup_mode && location.pathname.replace(/\/+$/, '') !== '/setup') location.assign('/setup');
  });
  on('resync', () => { /* tabs keep their own data fresh via topology/settings events */ });

  startEvents();
  refreshTopology().catch((e) => toast(`Can't read the device state: ${e.message}`, 'error'));
  get('/api/system').then((s) => { S.system = s; paintBanners(); }).catch(() => {});
  show();
}
