package netcfg

import (
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Hosts and paths that operating systems request to find out whether they are
// behind a captive portal. In setup mode every host name resolves to the Pi
// (wildcard DNS), so these requests reach us; answering anything other than
// the "all is well" response they expect makes the OS open its sign-in sheet.
var (
	probeHosts = map[string]bool{
		"captive.apple.com":                      true, // iOS, macOS
		"connectivitycheck.gstatic.com":          true, // Android
		"connectivitycheck.android.com":          true,
		"clients3.google.com":                    true,
		"clients.l.google.com":                   true,
		"www.msftconnecttest.com":                true, // Windows
		"dns.msftncsi.com":                       true,
		"www.msftncsi.com":                       true,
		"detectportal.firefox.com":               true, // Firefox
		"connectivity-check.ubuntu.com":          true, // Ubuntu / NetworkManager
		"nmcheck.gnome.org":                      true, // GNOME / NetworkManager
		"network-test.debian.org":                true,
		"www.gstatic.com":                        true,
		"play.googleapis.com":                    true,
		"www.apple.com":                          true,
		"www.appleiphonecell.com":                true,
		"www.itools.info":                        true,
		"www.ibook.info":                         true,
		"www.airport.us":                         true,
		"www.thinkdifferent.us":                  true,
		"android.clients.google.com":             true,
		"connectivitycheck.platform.hicloud.com": true,
	}
	probePaths = map[string]bool{
		"/hotspot-detect.html":       true, // Apple
		"/library/test/success.html": true,
		"/generate_204":              true, // Android, Chrome
		"/gen_204":                   true,
		"/connecttest.txt":           true, // Windows
		"/ncsi.txt":                  true,
		"/redirect":                  true,
		"/canonical.html":            true, // Firefox
		"/success.txt":               true,
		"/check_network_status.txt":  true, // Ubuntu
		"/connectivity-check.html":   true,
		"/nm-check.txt":              true,
	}
)

// CaptiveMiddleware turns the web server into a captive portal while the
// manager is in setup mode, and is a transparent pass-through otherwise.
//
// In setup mode:
//   - requests for the OS connectivity probes get a 302 to the portal URL;
//   - requests for the portal host itself (or a bare IP address, or localhost)
//     pass through, as do paths under /api/ and /setup;
//   - anything else (any other Host) gets a 302 to the portal URL.
//
// Redirects carry Cache-Control: no-store so the OS re-probes after setup.
func CaptiveMiddleware(m interface {
	InSetupMode() bool
	PortalURL() string
}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !m.InSetupMode() {
				next.ServeHTTP(w, r)
				return
			}
			portal := m.PortalURL()
			host := requestHost(r)
			portalHost := ""
			if u, err := url.Parse(portal); err == nil {
				portalHost = strings.ToLower(u.Hostname())
			}

			isProbe := probeHosts[host] || probePaths[r.URL.Path]
			onPortal := host == portalHost || host == "localhost" || net.ParseIP(host) != nil

			switch {
			case onPortal:
				next.ServeHTTP(w, r)
			case isProbe:
				redirect(w, portal)
			case r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") ||
				r.URL.Path == "/setup" || strings.HasPrefix(r.URL.Path, "/setup/"):
				next.ServeHTTP(w, r)
			default:
				redirect(w, portal)
			}
		})
	}
}

// requestHost returns the lower-case host name of the request without port,
// brackets or trailing dot.
func requestHost(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func redirect(w http.ResponseWriter, to string) {
	h := w.Header()
	h.Set("Location", to)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusFound)
	// A body for clients that ignore Location.
	esc := html.EscapeString(to)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0; url=` +
		esc + `"><title>EMM setup</title><a href="` + esc + `">Open EMM setup</a>`))
}
