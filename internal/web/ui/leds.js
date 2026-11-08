// Activity LEDs. Every LED element is registered under a port id and
// direction; `activity` events flash the matching ones.

import { h } from './dom.js';
import { on } from './state.js';

const registry = new Map(); // id -> { rx: Set<Element>, tx: Set<Element> }

export function led(portId, dir) {
  const el = h('span', { class: `led led-${dir}`, 'aria-hidden': 'true' });
  let e = registry.get(portId);
  if (!e) registry.set(portId, (e = { rx: new Set(), tx: new Set() }));
  e[dir].add(el);
  return el;
}

function flash(set) {
  for (const el of set) {
    if (!el.isConnected) { set.delete(el); continue; }
    el.classList.add('on');
    clearTimeout(el._t);
    el._t = setTimeout(() => el.classList.remove('on'), 110);
  }
}

on('activity', ({ id, rx, tx }) => {
  const e = registry.get(id);
  if (!e) return;
  if (rx) flash(e.rx);
  if (tx) flash(e.tx);
});
