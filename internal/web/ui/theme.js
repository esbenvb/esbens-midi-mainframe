// Theme: dark-first, follows the OS unless the user picked one explicitly.
// localStorage may be unavailable (private mode, captive-portal browsers),
// so every access is wrapped.

const KEY = 'emm-theme';

function stored() {
  try { const v = localStorage.getItem(KEY); return v === 'light' || v === 'dark' ? v : null; } catch (_) { return null; }
}

export function currentTheme() {
  return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
}

function apply(t) {
  document.documentElement.dataset.theme = t;
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', t === 'light' ? '#f3f1ec' : '#0d0e11');
}

export function initTheme() {
  const mq = window.matchMedia('(prefers-color-scheme: light)');
  apply(stored() || (mq.matches ? 'light' : 'dark'));
  const onChange = () => { if (!stored()) apply(mq.matches ? 'light' : 'dark'); };
  if (mq.addEventListener) mq.addEventListener('change', onChange);
}

export function toggleTheme() {
  const next = currentTheme() === 'light' ? 'dark' : 'light';
  apply(next);
  try { localStorage.setItem(KEY, next); } catch (_) { /* fine: just not remembered */ }
  return next;
}
