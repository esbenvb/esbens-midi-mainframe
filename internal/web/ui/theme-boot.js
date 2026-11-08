// Runs synchronously in <head> so the theme is set before first paint (no
// flash). Kept as a separate classic script because the daemon's
// Content-Security-Policy forbids inline scripts. Mirrors theme.js.
(function () {
  try {
    var t = localStorage.getItem('emm-theme');
    if (t !== 'light' && t !== 'dark') t = matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', t);
  } catch (e) { /* default (dark) stays */ }
})();
