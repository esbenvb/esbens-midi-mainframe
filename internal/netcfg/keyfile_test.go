package netcfg

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func TestRenderWifiClientExact(t *testing.T) {
	got, err := renderWifiClient("HomeNet", "secret123")
	if err != nil {
		t.Fatal(err)
	}
	want := "[connection]\n" +
		"id=emm-wifi\n" +
		"uuid=" + uuidFor("emm-wifi") + "\n" +
		"type=wifi\n" +
		"autoconnect=true\n" +
		"autoconnect-priority=10\n" +
		"autoconnect-retries=0\n" +
		"\n[wifi]\n" +
		"mode=infrastructure\n" +
		"ssid=HomeNet\n" +
		"powersave=2\n" +
		"\n[wifi-security]\n" +
		"key-mgmt=wpa-psk\n" +
		"psk=secret123\n" +
		"\n[ipv4]\nmethod=auto\n" +
		"\n[ipv6]\nmethod=auto\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderWifiClientOpenNetwork(t *testing.T) {
	got, err := renderWifiClient("CoffeeShop", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "wifi-security") || strings.Contains(got, "psk") {
		t.Fatalf("open network must have no security section:\n%s", got)
	}
}

func TestRenderWifiClientEscaping(t *testing.T) {
	got, err := renderWifiClient(`My "Net"; ok`, ` pa\ss word `)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ssid=My \"Net\"; ok\n") {
		t.Errorf("ssid line wrong:\n%s", got)
	}
	// Leading and trailing spaces become \s (they would be trimmed otherwise),
	// the backslash is doubled, the inner space is kept.
	if !strings.Contains(got, `psk=\spa\\ss word\s`+"\n") {
		t.Errorf("psk line wrong:\n%s", got)
	}
}

func TestKfEscapeRoundTripsThroughFakeParser(t *testing.T) {
	for _, s := range []string{"plain", " lead", "trail ", "a\\b", "multi\nline", "tab\there", " ", "", "ünï©ode"} {
		if got := kfUnescape(kfEscape(s), false); got != s {
			t.Errorf("round trip %q -> %q -> %q", s, kfEscape(s), got)
		}
	}
}

func TestSSIDValueForms(t *testing.T) {
	cases := []struct{ ssid, want string }{
		{"HomeNet", "HomeNet"},
		{"Café", "Café"},
		{"semi;colon", "semi;colon"},
		{"12345678", "49;50;51;52;53;54;55;56;"}, // digits only would be read as a byte list
		{"1;2", "49;59;50;"},
		{"bad\x01ssid", "98;97;100;1;115;115;105;100;"}, // unprintable -> byte list
		{"\xff\xfe", "255;254;"},                        // invalid UTF-8 -> byte list
	}
	for _, c := range cases {
		if got := ssidValue(c.ssid); got != c.want {
			t.Errorf("ssidValue(%q) = %q, want %q", c.ssid, got, c.want)
		}
		// whatever form was chosen must decode back to the original bytes
		if back := kfUnescape(ssidValue(c.ssid), true); back != c.ssid {
			t.Errorf("ssid %q did not round trip: %q", c.ssid, back)
		}
	}
}

func TestRenderAPExactOpen(t *testing.T) {
	got, err := renderAP("EMM-Setup-A1C2", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "[connection]\n" +
		"id=emm-setup-ap\n" +
		"uuid=" + uuidFor("emm-setup-ap") + "\n" +
		"type=wifi\n" +
		"autoconnect=false\n" +
		"\n[wifi]\n" +
		"mode=ap\n" +
		"band=bg\n" +
		"channel=6\n" +
		"ssid=EMM-Setup-A1C2\n" +
		"\n[ipv4]\n" +
		"address1=10.42.0.1/24\n" +
		"method=shared\n" +
		"\n[ipv6]\nmethod=ignore\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderAPWithPassword(t *testing.T) {
	got, err := renderAP("EMM-Setup", "mainframe1")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"key-mgmt=wpa-psk", "proto=rsn;", "pairwise=ccmp;", "group=ccmp;", "psk=mainframe1"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	if !strings.Contains(got, "method=shared") || !strings.Contains(got, "address1=10.42.0.1/24") {
		t.Errorf("AP must use shared mode on 10.42.0.1/24:\n%s", got)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	if _, err := renderWifiClient("", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty ssid: %v", err)
	}
	if _, err := renderWifiClient(strings.Repeat("s", 33), ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("long ssid: %v", err)
	}
	if _, err := renderWifiClient("ok", "short"); !errors.Is(err, ErrInvalid) {
		t.Errorf("short psk: %v", err)
	}
	if _, err := renderAP("ok", strings.Repeat("p", 64)+"z"); !errors.Is(err, ErrInvalid) {
		t.Errorf("long psk: %v", err)
	}
	if _, err := renderWifiClient("ok", "pass\nword1"); !errors.Is(err, ErrInvalid) {
		t.Errorf("control chars in psk: %v", err)
	}
}

func TestValidatePSK(t *testing.T) {
	ok := []string{"", "12345678", strings.Repeat("a", 63), strings.Repeat("ab12", 16), "pässwörd1"}
	bad := []string{"1234567", strings.Repeat("a", 63) + "g", strings.Repeat("a", 65)}
	for _, p := range ok {
		if err := validatePSK(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := validatePSK(p); err == nil {
			t.Errorf("%q should be invalid", p)
		}
	}
}

func TestUUIDStableAndWellFormed(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, id := range []string{wifiConnID, apConnID, ethConnID} {
		u := uuidFor(id)
		if !re.MatchString(u) {
			t.Errorf("uuid %q for %s is malformed", u, id)
		}
		if u != uuidFor(id) {
			t.Error("uuid not stable")
		}
	}
	if uuidFor(wifiConnID) == uuidFor(apConnID) {
		t.Error("distinct profiles must have distinct uuids")
	}
}

func TestRenderEthernetModes(t *testing.T) {
	auto, err := renderEthernet(EthernetSettings{})
	if err != nil {
		t.Fatal(err)
	}
	wantAuto := "[connection]\nid=emm-eth\nuuid=" + uuidFor("emm-eth") + "\ntype=ethernet\nautoconnect=true\nautoconnect-priority=100\n" +
		"\n[ethernet]\n\n[ipv4]\nmethod=auto\n\n[ipv6]\nmethod=auto\n"
	if auto != wantAuto {
		t.Fatalf("auto:\n%s\nwant:\n%s", auto, wantAuto)
	}

	dhcp, _ := renderEthernet(EthernetSettings{Mode: "dhcp"})
	if !strings.Contains(dhcp, "may-fail=false") || !strings.Contains(dhcp, "[ipv6]\nmethod=ignore") {
		t.Errorf("dhcp:\n%s", dhcp)
	}

	static, err := renderEthernet(EthernetSettings{Mode: "static", Address: "192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1", "8.8.8.8"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"address1=192.168.1.50/24,192.168.1.1", "dns=1.1.1.1;8.8.8.8;", "method=manual"} {
		if !strings.Contains(static, line+"\n") {
			t.Errorf("static missing %q:\n%s", line, static)
		}
	}

	noGW, _ := renderEthernet(EthernetSettings{Mode: "static", Address: "10.0.0.2/8"})
	if !strings.Contains(noGW, "address1=10.0.0.2/8\n") || strings.Contains(noGW, "dns=") {
		t.Errorf("static without gateway/dns:\n%s", noGW)
	}

	off, _ := renderEthernet(EthernetSettings{Mode: "off"})
	if !strings.Contains(off, "autoconnect=false") || !strings.Contains(off, "[ipv4]\nmethod=disabled") {
		t.Errorf("off:\n%s", off)
	}
}

func TestNormalizeEthernetValidation(t *testing.T) {
	bad := []EthernetSettings{
		{Mode: "turbo"},
		{Mode: "static"},
		{Mode: "static", Address: "192.168.1.50"},
		{Mode: "static", Address: "fe80::1/64"},
		{Mode: "static", Address: "192.168.1.50/24", Gateway: "10.0.0.1"},
		{Mode: "static", Address: "192.168.1.50/24", Gateway: "nope"},
		{Mode: "static", Address: "192.168.1.50/24", DNS: []string{"nope"}},
	}
	for _, e := range bad {
		if _, err := normalizeEthernet(e); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v should be rejected, got %v", e, err)
		}
	}
	// Non-static modes drop stale static fields.
	e, err := normalizeEthernet(EthernetSettings{Mode: "DHCP", Address: "1.2.3.4/24", DNS: []string{"1.1.1.1"}})
	if err != nil || e.Mode != "dhcp" || e.Address != "" || e.DNS != nil {
		t.Errorf("got %+v %v", e, err)
	}
}

func TestDNSMasqWildcard(t *testing.T) {
	if !strings.Contains(dnsmasqWildcard(), "\naddress=/#/10.42.0.1\n") {
		t.Fatalf("wildcard snippet wrong:\n%s", dnsmasqWildcard())
	}
}

// R2: the profile of a network that does not broadcast its name asks
// NetworkManager to probe for it; one that does is left alone.
func TestRenderWifiProfileHiddenFlag(t *testing.T) {
	hid, err := renderWifiProfile("Secret Studio", "secret123", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hid, "[wifi]\nmode=infrastructure\nssid=Secret Studio\nhidden=true\npowersave=2\n") {
		t.Fatalf("hidden profile:\n%s", hid)
	}
	vis, err := renderWifiProfile("Secret Studio", "secret123", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(vis, "hidden") {
		t.Fatalf("visible profile must not say hidden:\n%s", vis)
	}
	if want, _ := renderWifiClient("Secret Studio", "secret123"); vis != want {
		t.Fatal("renderWifiClient must stay the visible form")
	}
	if hid2, _ := renderWifiProfile("Secret Studio", "secret123", false, true); hid2 != hid {
		t.Fatal("rendering must be deterministic")
	}
}

// R10: NetworkManager's keyfile reader (nm-keyfile.c get_bytes/ssid_parser)
// turns `\;` in a string SSID into ';' and may read a value of only digits,
// ';' and white space as a byte list. Whatever form ssidValue picks has to come
// back byte for byte through that reader (modelled by kfUnescape).
func TestSSIDValueSurvivesNetworkManagerReader(t *testing.T) {
	for _, ssid := range []string{
		`a\;b`, `\;`, `;\`, `trailing\`, `\`, `a\\;b`, `back\slash`, `win\dows;`,
		";", ";;", "abc;", ";abc", "semi;colon", "a;", "a ; b",
		"1;2;3", "1;2;3;", "123", "007", "100;200;300;", "256;1;", "1; ", " 1;2", "12 34", "1 ;2 ;",
		"HomeNet", "Café", "日本語", "with space", " lead", "trail ", "Studio:5G", `My "Net"; ok`,
		"bad\x01ssid", "\xff\xfe", "tab\there",
	} {
		v := ssidValue(ssid)
		if strings.ContainsAny(v, "\n\r") {
			t.Errorf("ssidValue(%q) = %q contains a line break", ssid, v)
		}
		if back := kfUnescape(v, true); back != ssid {
			t.Errorf("ssid %q written as %q is read back as %q", ssid, v, back)
		}
	}
}

// The model above must actually catch the problem: the plain string form of an
// SSID containing a backslash and ';' is misread.
func TestPlainSSIDWithBackslashSemicolonIsMisread(t *testing.T) {
	ssid := `a\;b`
	if back := kfUnescape(kfEscape(ssid), true); back == ssid {
		t.Fatalf("the NetworkManager reader model no longer shows the trap (read %q)", back)
	}
	if got := ssidValue(ssid); got != "97;92;59;98;" {
		t.Fatalf("ssidValue(%q) = %q, want the byte list", ssid, got)
	}
}
