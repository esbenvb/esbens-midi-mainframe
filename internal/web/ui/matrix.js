// The routing matrix: rows are sources, columns are destinations.
// Wide screens get the classic grid; narrow screens get "pick a destination,
// then toggle every source" which is far easier to use with a thumb.

import { h, clear } from './dom.js';
import { icon } from './icons.js';
import {
  S, on, ck, displayName, isDefaultFilter, filterSummary, connect, disconnect,
  DEFAULT_FILTER, isNetworkPort,
} from './state.js';
import { led } from './leds.js';
import { openFilterEditor } from './filter-editor.js';
import { toast } from './ui.js';
import { presetsButton, closePresets } from './presets.js';

const NET = '__net__';
let netExpanded = false; // module level: survives switching tabs
let narrowDest = null;

// ---- model -----------------------------------------------------------------
function groupPorts(ports) {
  const order = [];
  const map = new Map();
  for (const p of ports) {
    const net = isNetworkPort(p);
    const key = net ? NET : 'g:' + (p.group || p.name || p.id);
    let g = map.get(key);
    if (!g) {
      g = { key, net, label: net ? 'Network' : (p.group || p.name || p.id), ports: [] };
      map.set(key, g);
      order.push(g);
    }
    g.ports.push(p);
  }
  order.sort((a, b) => Number(a.net) - Number(b.net)); // network group last, otherwise stable
  return order;
}

function model() {
  const vis = S.ports.filter((p) => !p.hidden);
  const sources = vis.filter((p) => p.input);
  const dests = vis.filter((p) => p.output);
  return {
    sources: groupPorts(sources),
    dests: groupPorts(dests),
    nSrc: sources.length,
    nDst: dests.length,
    hidden: S.ports.length - vis.length,
    hasHardware: S.ports.some((p) => !isNetworkPort(p)),
  };
}

function signature(m, narrow) {
  const g = (groups) => groups.map((x) => x.key + ':' + x.ports.map((p) => `${p.id}|${displayName(p)}|${p.online === false ? 0 : 1}`).join(',')).join(';');
  return [netExpanded, narrow, narrow ? narrowDest : '', g(m.sources), '//', g(m.dests)].join('#');
}

function netLinkCount() {
  let n = 0;
  for (const c of S.connections) {
    const a = S.byId.get(c.from), b = S.byId.get(c.to);
    if ((a && isNetworkPort(a)) || (b && isNetworkPort(b))) n++;
  }
  return n;
}

async function toggle(src, dst) {
  const existing = S.connMap.get(ck(src.id, dst.id));
  try {
    if (existing) await disconnect(src.id, dst.id);
    else await connect(src.id, dst.id, DEFAULT_FILTER());
  } catch (e) {
    toast(`${displayName(src)} → ${displayName(dst)}: ${e.message}`, 'error');
  }
}

function stateOf(src, dst) {
  const conn = S.connMap.get(ck(src.id, dst.id));
  if (!conn) return { conn: null, state: 'off', filtered: false };
  const offline = src.online === false || dst.online === false || conn.online === false;
  return { conn, state: offline ? 'dormant' : 'on', filtered: !isDefaultFilter(conn.filter) };
}

function stateText(s) {
  const t = s.state === 'off' ? 'not connected' : s.state === 'dormant' ? 'connected, waiting for device' : 'connected';
  return t + (s.filtered ? ', filtered' : '');
}

function tooltipOf(p) {
  return [displayName(p), p.id, p.detail, p.online === false ? 'offline (remembered)' : ''].filter(Boolean).join('\n');
}

function legend() {
  const item = (cls, text) => h('li', {}, h('span', { class: 'lg ' + cls, 'aria-hidden': 'true' }), text);
  return h('ul', { class: 'legend' },
    item('lg-on', 'Connected'),
    item('lg-filter', 'Filtered'),
    item('lg-dormant', 'Waiting for device'),
    item('lg-off', 'Not connected'),
    h('li', { class: 'legend-hint' }, 'Click to toggle · F or right-click: filters · arrow keys move'));
}

function emptyState(m) {
  if (!m.hasHardware) {
    return h('div', { class: 'empty' },
      icon('plug', 36),
      h('h2', {}, 'No MIDI devices yet'),
      h('p', {}, 'Plug a USB MIDI device into the hub. It shows up here within a second or two, and every device also gets its own network (RTP-MIDI) port.'));
  }
  return h('div', { class: 'empty' },
    h('h2', {}, 'Nothing to route'),
    h('p', {}, m.hidden ? `${m.hidden} port${m.hidden === 1 ? ' is' : 's are'} hidden. Un-hide them on the Devices tab.` : 'There are no ports that can send or receive MIDI.'));
}

// ---- mount -----------------------------------------------------------------
export function mount(root) {
  const mq = window.matchMedia('(max-width: 720px)');
  const host = h('section', { class: 'matrix', 'aria-label': 'MIDI routing matrix' });
  root.append(h('div', { class: 'mx-toolbar' }, presetsButton()), host);

  let sig = null;
  let view = null;

  function render(force) {
    const m = model();
    if (!m.nSrc || !m.nDst) {
      if (sig !== 'empty') { sig = 'empty'; view = null; clear(host); host.append(emptyState(m)); }
      return;
    }
    const s = signature(m, mq.matches);
    if (!force && s === sig && view) { view.paint(); return; }
    sig = s;
    const a = document.activeElement;
    const keep = host.contains(a) && a.dataset && a.dataset.from ? [a.dataset.from, a.dataset.to] : null;
    clear(host);
    view = mq.matches ? renderNarrow(m, host, () => render(true)) : renderWide(m, host, () => render(true));
    host.append(legend());
    if (m.hidden) host.append(h('p', { class: 'muted small' }, `${m.hidden} hidden port${m.hidden === 1 ? '' : 's'} not shown (see the Devices tab).`));
    view.paint();
    if (keep && view.focus) view.focus(keep[0], keep[1]);
  }

  const offs = [on('topology', () => render(false))];
  const onMq = () => render(true);
  mq.addEventListener('change', onMq);
  render(true);
  return () => { offs.forEach((f) => f()); mq.removeEventListener('change', onMq); closePresets(); };
}

// ---- wide: the grid --------------------------------------------------------
function renderWide(m, host, rerender) {
  const grid = h('div', { class: 'mx', role: 'grid', 'aria-label': 'Sources down the side, destinations along the top' });
  const scroller = h('div', { class: 'mx-scroll' }, grid);
  host.append(scroller);

  const tmpl = ['var(--rowhead)'];
  const colSpecs = [];
  const gheads = [];
  let col = 2;
  let gi = 0;
  for (const g of m.dests) {
    gi++;
    if (g.net && !netExpanded) {
      tmpl.push('var(--stub)');
      gheads.push({ g, col, span: 1, stub: true, gi });
      colSpecs.push({ stub: true, col, start: true, g });
      col++;
      continue;
    }
    const start = col;
    g.ports.forEach((p, i) => { tmpl.push('var(--cell)'); colSpecs.push({ port: p, col, start: i === 0 }); col++; });
    gheads.push({ g, col: start, span: g.ports.length, gi });
  }
  grid.style.gridTemplateColumns = tmpl.join(' ');

  const colHeads = new Map(); // grid column -> element
  const rowHeads = new Map(); // grid row -> element
  const cells = [];           // flat list of cell objects
  const navRows = [];         // rows of cell|null for keyboard navigation
  let current = null;

  const netToggle = (label) => h('button', {
    type: 'button', class: 'net-toggle', 'aria-expanded': String(netExpanded),
    title: netExpanded ? 'Collapse network ports' : 'Show network ports',
    onclick: () => { netExpanded = !netExpanded; rerender(); },
  }, icon(netExpanded ? 'chevronDown' : 'chevronRight', 14), label);

  // header rows
  grid.append(h('div', { class: 'mx-corner', style: { gridRow: '1 / span 2', gridColumn: '1' } },
    h('span', { class: 'corner-from' }, 'from ↓'), h('span', { class: 'corner-to' }, 'to →')));
  for (const gh of gheads) {
    // Only label a group over its columns when there is room for the text.
    let content = null;
    if (gh.g.net && !gh.stub) content = netToggle('Network');
    else if (!gh.g.net && gh.span >= 3) content = h('span', { class: 'ghead-label' }, gh.g.label);
    grid.append(h('div', {
      class: 'mx-ghead g-start' + (gh.gi % 2 ? '' : ' alt') + (gh.stub ? ' is-stub' : ''), title: gh.g.label,
      style: { gridRow: '1', gridColumn: `${gh.col} / span ${gh.span}` },
    }, content));
  }
  for (const cs of colSpecs) {
    if (cs.stub) {
      const el = h('div', { class: 'mx-chead is-stub g-start' },
        h('button', { type: 'button', class: 'net-toggle net-stub', 'aria-expanded': 'false', title: 'Show network ports',
          onclick: () => { netExpanded = true; rerender(); } },
        icon('chevronRight', 14), h('span', { class: 'net-stub-text' }, 'Network')));
      el.style.gridRow = '2';
      el.style.gridColumn = String(cs.col);
      grid.append(el);
      continue;
    }
    const p = cs.port;
    const el = h('div', { class: 'mx-chead' + (cs.start ? ' g-start' : '') + (p.online === false ? ' is-offline' : ''), title: tooltipOf(p) },
      h('span', { class: 'mx-cname' }, displayName(p)),
      p.online === false ? h('span', { class: 'tag-offline', 'aria-label': 'offline' }, '⏻') : null,
      led(p.id, 'tx'));
    el.style.gridRow = '2';
    el.style.gridColumn = String(cs.col);
    colHeads.set(cs.col, el);
    grid.append(el);
  }

  // body
  let row = 3;
  for (const g of m.sources) {
    const links = g.net ? netLinkCount() : 0;
    const label = g.net
      ? netToggle(`Network · ${g.ports.length} port${g.ports.length === 1 ? '' : 's'}${links ? ` · ${links} link${links === 1 ? '' : 's'}` : ''}`)
      : h('span', { class: 'glabel-text' }, g.label);
    grid.append(h('div', { class: 'mx-glabel', style: { gridRow: String(row), gridColumn: '1 / -1' } }, label));
    row++;
    if (g.net && !netExpanded) continue;
    for (const p of g.ports) {
      const rh = h('div', { class: 'mx-rhead' + (p.online === false ? ' is-offline' : ''), title: tooltipOf(p), role: 'rowheader' },
        led(p.id, 'rx'),
        h('span', { class: 'mx-name' }, displayName(p)),
        p.online === false ? h('span', { class: 'tag-offline-text' }, 'offline') : (p.kind === 'rtp' ? h('span', { class: 'kind-tag' }, 'RTP') : p.kind === 'ble' ? h('span', { class: 'kind-tag' }, 'BLE') : null));
      rh.style.gridRow = String(row);
      rh.style.gridColumn = '1';
      grid.append(rh);
      rowHeads.set(row, rh);
      const nav = [];
      for (const cs of colSpecs) {
        if (cs.stub) {
          const s = h('div', { class: 'mx-stub-cell g-start' });
          s.style.gridRow = String(row);
          s.style.gridColumn = String(cs.col);
          grid.append(s);
          nav.push(null);
          continue;
        }
        const cell = makeCell(p, cs.port, row, cs.col, cs.start);
        grid.append(cell.wrap);
        if (cell.btn) cells.push(cell);
        nav.push(cell.btn ? cell : null);
      }
      navRows.push(nav);
      row++;
    }
  }

  // ---- interaction helpers
  function makeCell(src, dst, r, c, start) {
    const wrap = h('div', { class: 'mx-cell' + (start ? ' g-start' : ''), role: 'gridcell', dataset: { r: String(r), c: String(c) } });
    wrap.style.gridRow = String(r);
    wrap.style.gridColumn = String(c);
    if (src.id === dst.id) {
      wrap.classList.add('self');
      wrap.append(h('span', { class: 'xp-self', role: 'img', 'aria-label': `${displayName(src)} to itself, not available`, title: 'A port cannot feed itself' }));
      return { wrap };
    }
    const cell = { src, dst, wrap, r, c, last: '' };
    let pressTimer = null, sx = 0, sy = 0;
    cell.btn = h('button', {
      type: 'button', class: 'xp', tabindex: '-1', dataset: { from: src.id, to: dst.id },
      onclick: () => { if (cell.suppress) { cell.suppress = false; return; } toggle(src, dst); },
      onkeydown: (e) => onKey(e, cell),
      oncontextmenu: (e) => { e.preventDefault(); openEditor(cell, cell.btn); },
      onfocus: () => setCurrent(cell),
      onpointerdown: (e) => {
        if (e.pointerType === 'mouse') return;
        sx = e.clientX; sy = e.clientY;
        clearTimeout(pressTimer);
        pressTimer = setTimeout(() => { pressTimer = null; cell.suppress = true; openEditor(cell, cell.btn); }, 550);
      },
      onpointermove: (e) => { if (pressTimer && Math.hypot(e.clientX - sx, e.clientY - sy) > 8) { clearTimeout(pressTimer); pressTimer = null; } },
      onpointerup: () => { clearTimeout(pressTimer); pressTimer = null; },
      onpointercancel: () => { clearTimeout(pressTimer); pressTimer = null; },
    });
    // The gear is created lazily (see paint): most crosspoints never have a connection.
    cell.gear = null;
    wrap.append(cell.btn);
    return cell;
  }

  function openEditor(cell, anchor) {
    if (!S.connMap.get(ck(cell.src.id, cell.dst.id))) { toast('Connect the two ports first, then adjust the filter.'); return; }
    openFilterEditor(anchor, cell.src.id, cell.dst.id);
  }

  function setCurrent(cell) {
    if (current === cell) return;
    if (current) current.btn.tabIndex = -1;
    current = cell;
    cell.btn.tabIndex = 0;
  }

  function onKey(e, cell) {
    const key = e.key;
    if (key === 'f' || key === 'F') { e.preventDefault(); openEditor(cell, cell.gear && !cell.gear.hidden ? cell.gear : cell.btn); return; }
    let dr = 0, dc = 0;
    if (key === 'ArrowUp') dr = -1; else if (key === 'ArrowDown') dr = 1;
    else if (key === 'ArrowLeft') dc = -1; else if (key === 'ArrowRight') dc = 1;
    else if (key === 'Home' || key === 'End') {
      e.preventDefault();
      const ri = navRows.findIndex((r) => r.includes(cell));
      const list = navRows[ri].filter(Boolean);
      (key === 'Home' ? list[0] : list[list.length - 1]).btn.focus();
      return;
    } else return;
    e.preventDefault();
    let ri = navRows.findIndex((r) => r.includes(cell));
    let ci = navRows[ri].indexOf(cell);
    for (;;) {
      ri += dr; ci += dc;
      if (ri < 0 || ri >= navRows.length || ci < 0 || ci >= navRows[ri].length) return;
      const next = navRows[ri][ci];
      if (next) { next.btn.focus(); next.btn.scrollIntoView({ block: 'nearest', inline: 'nearest' }); return; }
    }
  }

  // crosshair: highlight the headers of the hovered / focused cell
  let hl = [];
  function highlight(e) {
    const wrap = e.target.closest && e.target.closest('.mx-cell');
    for (const el of hl) el.classList.remove('hl');
    hl = [];
    if (!wrap) return;
    const rh = rowHeads.get(Number(wrap.dataset.r)), ch = colHeads.get(Number(wrap.dataset.c));
    for (const el of [rh, ch]) if (el) { el.classList.add('hl'); hl.push(el); }
  }
  grid.addEventListener('mouseover', highlight);
  grid.addEventListener('focusin', highlight);
  grid.addEventListener('mouseleave', () => { for (const el of hl) el.classList.remove('hl'); hl = []; });

  function paint() {
    for (const cell of cells) {
      const s = stateOf(cell.src, cell.dst);
      const key = s.state + (s.filtered ? 'f' : '') + (s.conn ? filterSummary(s.conn.filter) : '');
      if (key === cell.last) continue;
      cell.last = key;
      cell.wrap.dataset.state = s.state;
      cell.wrap.dataset.filtered = s.filtered ? '1' : '0';
      const name = `${displayName(cell.src)} → ${displayName(cell.dst)}`;
      cell.btn.setAttribute('aria-label', `${name}, ${stateText(s)}`);
      cell.btn.title = s.conn ? `${name}\n${filterSummary(s.conn.filter)}\nClick to disconnect · F: filter` : `${name}\nClick to connect`;
      if (s.conn && !cell.gear) {
        cell.gear = h('button', {
          type: 'button', class: 'xp-gear', tabindex: '-1',
          'aria-label': `Filter settings for ${name}`,
          onclick: (e) => { e.stopPropagation(); openEditor(cell, cell.gear); },
        }, icon('sliders', 12));
        cell.wrap.append(cell.gear);
      }
      if (cell.gear) cell.gear.hidden = !s.conn;
    }
    if (!current && cells.length) setCurrent(cells[0]);
  }

  function focus(from, to) {
    const cell = cells.find((c) => c.src.id === from && c.dst.id === to);
    if (cell) cell.btn.focus({ preventScroll: true });
  }

  return { paint, focus };
}

// ---- narrow: destination picker + source toggles ---------------------------
function renderNarrow(m, host, rerender) {
  const dests = m.dests.flatMap((g) => g.ports);
  if (!dests.some((p) => p.id === narrowDest)) narrowDest = dests[0].id;
  const dest = dests.find((p) => p.id === narrowDest);

  const select = h('select', { id: 'nd', onchange: (e) => { narrowDest = e.target.value; rerender(); } },
    m.dests.map((g) => h('optgroup', { label: g.label }, g.ports.map((p) =>
      h('option', { value: p.id, selected: p.id === narrowDest }, displayName(p) + (p.online === false ? ' (offline)' : ''))))));

  host.append(h('div', { class: 'field narrow-dest' },
    h('label', { for: 'nd' }, 'Send to'),
    select));

  const list = h('ul', { class: 'chips', 'aria-label': `Sources that feed ${displayName(dest)}` });
  const rows = [];
  for (const g of m.sources) {
    if (g.net) {
      const links = netLinkCount();
      list.append(h('li', { class: 'chips-group' }, h('button', {
        type: 'button', class: 'net-toggle', 'aria-expanded': String(netExpanded),
        onclick: () => { netExpanded = !netExpanded; rerender(); },
      }, icon(netExpanded ? 'chevronDown' : 'chevronRight', 14), `Network · ${g.ports.length} port${g.ports.length === 1 ? '' : 's'}${links ? ` · ${links} link${links === 1 ? '' : 's'}` : ''}`)));
      if (!netExpanded) continue;
    } else {
      list.append(h('li', { class: 'chips-group' }, h('span', { class: 'glabel-text' }, g.label)));
    }
    for (const p of g.ports) {
      const self = p.id === dest.id;
      const stateEl = h('span', { class: 'chip-state', 'aria-hidden': 'true' });
      const chip = h('button', {
        type: 'button', class: 'chip', role: 'switch', 'aria-checked': 'false', disabled: self,
        dataset: { from: p.id, to: dest.id },
        onclick: () => toggle(p, dest),
      },
        led(p.id, 'rx'),
        h('span', { class: 'chip-name' },
          h('span', { class: 'chip-title' }, displayName(p)),
          p.online === false ? h('small', {}, 'offline') : (p.detail ? h('small', {}, p.detail) : null)),
        stateEl);
      const gear = h('button', {
        type: 'button', class: 'icon-btn chip-gear', hidden: true,
        'aria-label': `Filter settings for ${displayName(p)} to ${displayName(dest)}`,
        onclick: (e) => openFilterEditor(e.currentTarget, p.id, dest.id),
      }, icon('sliders', 18));
      list.append(h('li', { class: 'chip-row' }, chip, gear));
      rows.push({ p, chip, gear, stateEl, self });
    }
  }
  host.append(list);

  function paint() {
    for (const r of rows) {
      if (r.self) { r.chip.title = 'A port cannot feed itself'; continue; }
      const s = stateOf(r.p, dest);
      r.chip.setAttribute('aria-checked', String(s.state !== 'off'));
      r.chip.dataset.state = s.state;
      r.chip.dataset.filtered = s.filtered ? '1' : '0';
      r.chip.setAttribute('aria-label', `${displayName(r.p)} → ${displayName(dest)}, ${stateText(s)}`);
      r.stateEl.textContent = s.state === 'off' ? 'Off' : s.state === 'dormant' ? 'Waiting' : s.filtered ? 'On · filtered' : 'On';
      r.gear.hidden = !s.conn;
    }
  }
  function focus(from, to) {
    const r = rows.find((x) => x.p.id === from && dest.id === to);
    if (r) r.chip.focus({ preventScroll: true });
  }
  return { paint, focus };
}
