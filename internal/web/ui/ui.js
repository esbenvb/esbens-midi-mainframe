// Shared UI pieces: toasts, modal dialogs, popovers, switches, badges.

import { h, append } from './dom.js';
import { icon } from './icons.js';

// ---- toasts ----------------------------------------------------------------
let toastHost = null;

export function toast(message, kind = 'info') {
  if (!toastHost) {
    toastHost = h('div', { class: 'toasts', role: 'status', 'aria-live': 'polite' });
    document.body.appendChild(toastHost);
  }
  const el = h('div', { class: `toast toast-${kind}` },
    h('span', { class: 'toast-msg' }, message),
    h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Dismiss', onclick: () => el.remove() }, icon('x', 14)));
  toastHost.appendChild(el);
  while (toastHost.children.length > 4) toastHost.firstChild.remove();
  setTimeout(() => el.remove(), kind === 'error' ? 7000 : 3800);
}

// ---- small components ------------------------------------------------------
export function badge(text, kind = '') {
  return h('span', { class: 'badge' + (kind ? ' badge-' + kind : '') }, text);
}

export function spinner() {
  return h('span', { class: 'spinner', 'aria-hidden': 'true' });
}

// Marks a setting that emm.txt on the SD card fixes (and re-applies at boot).
export function emmTxtBadge() {
  return h('span', { class: 'badge badge-kind', title: 'Set in emm.txt on the SD card. It is re-applied at every boot.' }, 'emm.txt');
}

export function notice(kind, ...children) {
  return h('div', { class: `notice notice-${kind}`, role: kind === 'error' ? 'alert' : 'note' }, ...children);
}

// A labelled on/off switch. onChange(value) may be async; on failure the
// switch snaps back and a toast explains why. `disabled` makes it read-only
// (used for settings that emm.txt fixes). `id` / `fk` (see captureFocus) give
// the card's re-render a stable key to hand keyboard focus back to the new
// switch. While onChange runs the switch is only marked busy: setting
// `disabled` would blur it, and the keyboard user would lose their place.
export function toggleSwitch({ checked, label, onChange, hint, disabled = false, id = null, fk = null }) {
  const input = h('input', { type: 'checkbox', role: 'switch', checked: !!checked, disabled: !!disabled, id, dataset: fk ? { fk } : null });
  let busy = false;
  input.addEventListener('change', async () => {
    if (disabled || busy) { input.checked = !input.checked; return; }
    const v = input.checked;
    busy = true;
    input.setAttribute('aria-busy', 'true');
    try { await onChange(v); } catch (e) { input.checked = !v; toast(e.message, 'error'); } finally { busy = false; input.removeAttribute('aria-busy'); }
  });
  return h('label', { class: 'switch' + (disabled ? ' is-disabled' : '') },
    input,
    h('span', { class: 'track', 'aria-hidden': 'true' }, h('span', { class: 'thumb' })),
    h('span', { class: 'switch-label' }, label, hint ? h('small', {}, hint) : null));
}

// ---- keeping keyboard focus across a re-render -------------------------------
// The cards are rebuilt from scratch, so the control that had focus is replaced
// by a new element and focus would drop to <body>. A control is found again by
// a stable key: its id, or a data-fk attribute where an id would be awkward
// (several per page, or taken from data such as an SSID or a port id).
const keyOf = (el) => el.getAttribute('data-fk') || el.id || null;

export function findByKey(root, key) {
  if (!key) return null;
  for (const el of root.querySelectorAll('[data-fk],[id]')) if (keyOf(el) === key) return el;
  return null;
}

// A text field (or select): somebody may be typing in it, so a card is not rebuilt
// under it. A switch is not one: toggling it must not hold back the refresh.
export const isTextField = (el) => !!el && /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName) && el.type !== 'checkbox';

// Call before rebuilding `root`; null when focus is elsewhere.
export function captureFocus(root) {
  const a = document.activeElement;
  return a && a !== root && root.contains(a) ? { key: keyOf(a) } : null;
}

// Call after rebuilding. `successors` names, per key, the control to use when
// that one no longer exists (e.g. the Remove button after what it removed is
// gone). If nothing is found (or it is unavailable), focus goes to the tab's
// content area, so the next Tab continues from there rather than from the top
// of the page.
export function restoreFocus(root, saved, successors = {}) {
  if (!saved) return;
  let el = findByKey(root, saved.key) || findByKey(root, successors[saved.key]);
  if (el && el.disabled) el = null;
  if (!el) el = document.getElementById('view');
  if (el) el.focus({ preventScroll: true });
}

// ---- modal dialogs ---------------------------------------------------------
const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

export function openModal(build, opts = {}) {
  const prevFocus = document.activeElement;
  const prevKey = prevFocus && prevFocus.getAttribute ? keyOf(prevFocus) : null;
  const titleId = 'dlg-' + Math.random().toString(36).slice(2, 8);
  const overlay = h('div', { class: 'overlay' });
  const box = h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId });
  let closed = false;

  // Back to the control that opened the dialog. A re-render while the dialog was
  // open may have replaced it: then its successor (same key), else the content area.
  function restoreOpener() {
    if (!prevFocus || prevFocus === document.body || !prevFocus.focus) return;
    if (prevFocus.isConnected && !prevFocus.disabled) { prevFocus.focus(); return; }
    restoreFocus(document, { key: prevKey });
  }
  function close(result) {
    if (closed) return;
    closed = true;
    document.removeEventListener('keydown', onKey, true);
    overlay.remove();
    restoreOpener();
    if (opts.onClose) opts.onClose(result);
  }
  function onKey(e) {
    if (e.key === 'Escape') { e.stopPropagation(); close(undefined); return; }
    if (e.key !== 'Tab') return;
    const items = Array.from(box.querySelectorAll(FOCUSABLE)).filter((el) => el.offsetParent !== null);
    if (!items.length) return;
    const first = items[0], last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }

  const content = build(close, titleId);
  box.appendChild(content);
  overlay.appendChild(box);
  overlay.addEventListener('mousedown', (e) => { if (e.target === overlay) close(undefined); });
  document.body.appendChild(overlay);
  document.addEventListener('keydown', onKey, true);
  const auto = box.querySelector('[autofocus]') || box.querySelector('input:not([type=hidden]),select,textarea') || box.querySelector('button');
  if (auto) auto.focus();
  return { close };
}

export function confirmDialog({ title, body, confirmLabel = 'Confirm', danger = false }) {
  return new Promise((resolve) => {
    openModal((close, titleId) => h('div', { class: 'modal-body' },
      h('h2', { id: titleId }, title),
      h('p', {}, body),
      h('div', { class: 'modal-actions' },
        // Destructive actions default to Cancel so a stray Enter is harmless.
        h('button', { class: 'btn', type: 'button', autofocus: danger, onclick: () => close(false) }, 'Cancel'),
        h('button', { class: 'btn ' + (danger ? 'btn-danger' : 'btn-primary'), type: 'button', autofocus: !danger, onclick: () => close(true) }, confirmLabel))),
    { onClose: (r) => resolve(r === true) });
  });
}

// ---- popover (non-modal, anchored; bottom sheet on narrow screens) ---------
export function openPopover(anchor, content, { label = 'Settings', onClose } = {}) {
  const narrow = window.matchMedia('(max-width: 720px)').matches;
  const pop = h('div', { class: 'popover', role: 'dialog', 'aria-label': label, tabindex: '-1' }, content);
  const scrim = narrow ? h('div', { class: 'scrim' }) : null;
  if (scrim) document.body.appendChild(scrim);
  document.body.appendChild(pop);
  let closed = false;

  function place() {
    if (narrow) return;
    const r = anchor.getBoundingClientRect();
    const pw = pop.offsetWidth, ph = pop.offsetHeight;
    const vw = window.innerWidth, vh = window.innerHeight;
    let left = r.left + r.width / 2 - pw / 2;
    left = Math.max(8, Math.min(left, vw - pw - 8));
    let top = r.bottom + 8;
    if (top + ph > vh - 8) top = r.top - ph - 8;
    top = Math.max(8, Math.min(top, vh - ph - 8));
    pop.style.left = left + 'px';
    pop.style.top = top + 'px';
  }
  place();

  function close() {
    if (closed) return;
    closed = true;
    document.removeEventListener('keydown', onKey, true);
    document.removeEventListener('pointerdown', onDown, true);
    window.removeEventListener('resize', place);
    pop.remove();
    if (scrim) scrim.remove();
    if (onClose) onClose();
    if (anchor.isConnected && anchor.focus) anchor.focus({ preventScroll: true });
  }
  function onKey(e) {
    if (e.key === 'Escape') { e.stopPropagation(); close(); }
  }
  function onDown(e) {
    if (pop.contains(e.target) || anchor.contains(e.target)) return;
    close();
  }
  document.addEventListener('keydown', onKey, true);
  document.addEventListener('pointerdown', onDown, true);
  window.addEventListener('resize', place);

  const first = pop.querySelector('[autofocus]') || pop.querySelector(FOCUSABLE);
  (first || pop).focus({ preventScroll: true });
  return { close, el: pop, place };
}

// A two-line key/value list used on status cards.
export function kv(rows) {
  const dl = h('dl', { class: 'kv' });
  for (const [k, v] of rows) {
    if (v == null || v === '') continue;
    append(dl, [h('dt', {}, k), h('dd', {}, v)]);
  }
  return dl;
}
