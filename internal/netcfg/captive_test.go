package netcfg

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

var errTest = errors.New("exit status 1")

type fakePortal struct {
	setup bool
	url   string
}

func (f *fakePortal) InSetupMode() bool { return f.setup }
func (f *fakePortal) PortalURL() string { return f.url }

func serve(t *testing.T, p *fakePortal, host, path string) (code int, location string, passed bool, cc string) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passed = true
		w.WriteHeader(http.StatusOK)
	})
	h := CaptiveMiddleware(p)(next)
	req := httptest.NewRequest("GET", "http://"+host+path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Location"), passed, rec.Header().Get("Cache-Control")
}

func TestCaptiveProbesRedirectInSetupMode(t *testing.T) {
	p := &fakePortal{setup: true, url: "http://10.42.0.1/"}
	probes := []struct{ host, path string }{
		{"captive.apple.com", "/hotspot-detect.html"},
		{"captive.apple.com", "/library/test/success.html"},
		{"www.apple.com", "/library/test/success.html"},
		{"connectivitycheck.gstatic.com", "/generate_204"},
		{"connectivitycheck.android.com", "/generate_204"},
		{"clients3.google.com", "/generate_204"},
		{"www.google.com", "/gen_204"},
		{"www.msftconnecttest.com", "/connecttest.txt"},
		{"www.msftconnecttest.com", "/redirect"},
		{"www.msftncsi.com", "/ncsi.txt"},
		{"detectportal.firefox.com", "/canonical.html"},
		{"detectportal.firefox.com", "/success.txt"},
		{"connectivity-check.ubuntu.com", "/"},
		{"nmcheck.gnome.org", "/check_network_status.txt"},
	}
	for _, pr := range probes {
		code, loc, passed, cc := serve(t, p, pr.host, pr.path)
		if code != http.StatusFound || loc != "http://10.42.0.1/" || passed {
			t.Errorf("%s%s: code=%d location=%q passed=%v", pr.host, pr.path, code, loc, passed)
		}
		if cc != "no-store" {
			t.Errorf("%s%s: Cache-Control %q", pr.host, pr.path, cc)
		}
	}
}

func TestCaptiveForeignHostsRedirect(t *testing.T) {
	p := &fakePortal{setup: true, url: "http://10.42.0.1/"}
	for _, host := range []string{"example.com", "www.google.com:80", "EXAMPLE.org", "facebook.com.", "emm.local", ""} {
		code, loc, passed, _ := serve(t, p, host, "/some/page")
		if code != http.StatusFound || loc != p.url || passed {
			t.Errorf("host %q: code=%d location=%q passed=%v", host, code, loc, passed)
		}
	}
}

func TestCaptivePortalHostPassesThrough(t *testing.T) {
	p := &fakePortal{setup: true, url: "http://10.42.0.1/"}
	for _, host := range []string{"10.42.0.1", "10.42.0.1:80", "10.42.0.1:8080", "192.168.4.1", "localhost", "localhost:8080", "[fe80::1]:80", "[::1]"} {
		for _, path := range []string{"/", "/index.html", "/api/state", "/setup"} {
			code, _, passed, _ := serve(t, p, host, path)
			if code != http.StatusOK || !passed {
				t.Errorf("%s%s: code=%d passed=%v", host, path, code, passed)
			}
		}
	}
}

func TestCaptiveAPIAndSetupPathsPassThroughForAnyHost(t *testing.T) {
	p := &fakePortal{setup: true, url: "http://10.42.0.1/"}
	for _, path := range []string{"/api/network", "/api/network/wifi", "/setup", "/setup/wifi", "/api"} {
		code, _, passed, _ := serve(t, p, "captive.example.net", path)
		if code != http.StatusOK || !passed {
			t.Errorf("%s: code=%d passed=%v", path, code, passed)
		}
	}
	// but a look-alike prefix must not
	code, _, passed, _ := serve(t, p, "captive.example.net", "/apiary")
	if code != http.StatusFound || passed {
		t.Errorf("/apiary: code=%d passed=%v", code, passed)
	}
	code, _, passed, _ = serve(t, p, "captive.example.net", "/setupx")
	if code != http.StatusFound || passed {
		t.Errorf("/setupx: code=%d passed=%v", code, passed)
	}
}

func TestCaptiveProbeOnPortalHostIsServedNotRedirected(t *testing.T) {
	// A browser on the portal address must never loop.
	p := &fakePortal{setup: true, url: "http://10.42.0.1/"}
	code, _, passed, _ := serve(t, p, "10.42.0.1", "/generate_204")
	if code != http.StatusOK || !passed {
		t.Errorf("code=%d passed=%v", code, passed)
	}
}

func TestCaptivePassThroughOutsideSetupMode(t *testing.T) {
	p := &fakePortal{setup: false, url: "http://emm.local/"}
	for _, c := range []struct{ host, path string }{
		{"captive.apple.com", "/hotspot-detect.html"},
		{"example.com", "/"},
		{"emm.local", "/api/state"},
	} {
		code, loc, passed, _ := serve(t, p, c.host, c.path)
		if code != http.StatusOK || !passed || loc != "" {
			t.Errorf("%s%s: code=%d loc=%q passed=%v", c.host, c.path, code, loc, passed)
		}
	}
}

func TestCaptiveFollowsModeChanges(t *testing.T) {
	p := &fakePortal{setup: true, url: "http://10.42.0.1/"}
	if code, _, _, _ := serve(t, p, "example.com", "/"); code != http.StatusFound {
		t.Fatal("should redirect in setup mode")
	}
	p.setup = false
	if code, _, _, _ := serve(t, p, "example.com", "/"); code != http.StatusOK {
		t.Fatal("should pass through after setup")
	}
}

func TestCaptiveRedirectBodyIsEscaped(t *testing.T) {
	p := &fakePortal{setup: true, url: `http://10.42.0.1/?a="><script>`}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	CaptiveMiddleware(p)(next).ServeHTTP(rec, req)
	if body := rec.Body.String(); contains(body, "<script>") {
		t.Fatalf("unescaped portal URL in body: %s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
