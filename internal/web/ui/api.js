// Thin fetch wrapper. Errors carry the daemon's {"error": "..."} message.

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// Default request timeout is short on purpose. These calls block on the
// device for longer: a WiFi join, forgetting the network or applying Ethernet
// settings take up to ~55 s on the server, and a forced WiFi scan up to 30 s.
export const DEFAULT_TIMEOUT = 15000;
export const LONG_TIMEOUT = 70000;
export const SCAN_TIMEOUT = 40000;

export async function api(method, path, body, opts = {}) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), opts.timeout ?? DEFAULT_TIMEOUT);
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : {},
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: ctl.signal,
      credentials: 'same-origin',
      cache: 'no-store',
    });
  } catch (e) {
    throw new ApiError(e && e.name === 'AbortError' ? 'The device took too long to answer' : 'Cannot reach the device', 0);
  } finally {
    clearTimeout(timer);
  }
  let data = null;
  const text = await res.text();
  if (text) {
    try { data = JSON.parse(text); } catch (_) { data = null; }
  }
  if (!res.ok) {
    throw new ApiError((data && data.error) || res.statusText || `HTTP ${res.status}`, res.status);
  }
  return data;
}

export const get = (p, o) => api('GET', p, undefined, o);
export const put = (p, b, o) => api('PUT', p, b, o);
export const post = (p, b, o) => api('POST', p, b === undefined ? {} : b, o);
export const patch = (p, b, o) => api('PATCH', p, b, o);
export const del = (p, o) => api('DELETE', p, undefined, o);
