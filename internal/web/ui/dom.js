// Tiny DOM builder.
//
// Strings passed as children always become text nodes, never HTML, so names
// that come from hardware (USB product strings, WiFi SSIDs) cannot inject
// markup. There is deliberately no innerHTML anywhere in this UI.

const PROPS = new Set(['value', 'checked', 'disabled', 'selected', 'hidden', 'open', 'readOnly', 'indeterminate']);

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (PROPS.has(k)) el[k] = v;
      else el.setAttribute(k, v === true ? '' : String(v));
    }
  }
  append(el, children);
  return el;
}

export function append(el, children) {
  for (const c of children) {
    if (c == null || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function replace(el, ...children) {
  clear(el);
  return append(el, children);
}

// Debounce helper with flush().
export function debounce(fn, ms) {
  let t = null, args = null;
  const d = (...a) => { args = a; clearTimeout(t); t = setTimeout(run, ms); };
  function run() { t = null; const a = args; args = null; fn(...a); }
  d.flush = () => { if (t !== null) { clearTimeout(t); run(); } };
  d.cancel = () => { clearTimeout(t); t = null; args = null; };
  return d;
}

export function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KB';
  return (n / 1048576).toFixed(1) + ' MB';
}

export function fmtNum(n) {
  return (Number(n) || 0).toLocaleString('en-US');
}

export function fmtUptime(s) {
  s = Math.floor(Number(s) || 0);
  const d = Math.floor(s / 86400), hh = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${hh}h ${m}m`;
  if (hh) return `${hh}h ${m}m`;
  if (m) return `${m}m ${s % 60}s`;
  return `${s}s`;
}
