package netcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) BootConfig {
	t.Helper()
	c, err := ParseBootConfig(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func hasWarning(c BootConfig, sub string) bool {
	for _, w := range c.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestParseBootConfigFull(t *testing.T) {
	c := parse(t, `# EMM boot config
hostname      = emm
wifi_country  = DK
wifi_ssid     = MyNetwork
wifi_password = secret123
ethernet      = static     # use a fixed address
eth_address   = 192.168.1.50/24
eth_gateway   = 192.168.1.1
eth_dns       = 192.168.1.1, 1.1.1.1
ap_ssid       = EMM-Setup
ap_password   = mainframe1
rtp           = on
ble           = off
ble_name      = EMM Studio
web_password  = hunter2
`)
	if len(c.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", c.Warnings)
	}
	if c.Hostname != "emm" || c.WifiCountry != "DK" || c.WifiSSID != "MyNetwork" || c.WifiPassword != "secret123" ||
		c.Ethernet != "static" || c.EthAddress != "192.168.1.50/24" || c.EthGateway != "192.168.1.1" ||
		!reflect.DeepEqual(c.EthDNS, []string{"192.168.1.1", "1.1.1.1"}) ||
		c.APSSID != "EMM-Setup" || c.APPassword != "mainframe1" ||
		c.BLEName != "EMM Studio" || c.WebPassword != "hunter2" {
		t.Fatalf("bad parse: %+v", c)
	}
	if c.RTP == nil || !*c.RTP || c.BLE == nil || *c.BLE {
		t.Fatalf("booleans: rtp=%v ble=%v", c.RTP, c.BLE)
	}
}

func TestParseBootConfigCRLFAndBOM(t *testing.T) {
	c := parse(t, "\xef\xbb\xbfhostname = studio\r\nwifi_ssid = Net\r\nwifi_password = abcdefgh\r\n")
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings: %v", c.Warnings)
	}
	if c.Hostname != "studio" || c.WifiSSID != "Net" || c.WifiPassword != "abcdefgh" {
		t.Fatalf("got %+v", c)
	}
}

func TestParseBootConfigEmptyAndMissingEquals(t *testing.T) {
	c := parse(t, "\n\n   \n# only comments\nnonsense line\n")
	if !hasWarning(c, "expected key = value") {
		t.Fatalf("warnings: %v", c.Warnings)
	}
	if c.Hostname != "" {
		t.Fatal("hostname should be unset")
	}
}

func TestParseBootConfigNoSpacesAndCaseInsensitiveKeys(t *testing.T) {
	c := parse(t, "HOSTNAME=Studio-1\nWiFi_SSID=Net\n")
	if c.Hostname != "studio-1" { // lower-cased silently
		t.Fatalf("hostname %q", c.Hostname)
	}
	if c.WifiSSID != "Net" || len(c.Warnings) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestParseBootConfigQuotes(t *testing.T) {
	cases := []struct{ line, want string }{
		{`wifi_password = "pa ss#word=1"`, "pa ss#word=1"},
		{`wifi_password = 'it"s a #secret'`, `it"s a #secret`},
		{`wifi_password = "  spaced  "`, "  spaced  "},
		{`wifi_password = "quoted" # trailing comment`, "quoted"},
		{`wifi_password = ""`, ""},
	}
	for _, c := range cases {
		got := parse(t, c.line)
		if got.WifiPassword != c.want {
			t.Errorf("%s: got %q want %q (warnings %v)", c.line, got.WifiPassword, c.want, got.Warnings)
		}
	}
	// Text after the closing quote is dropped with a warning.
	got := parse(t, `wifi_password = "abcdefgh" junk`)
	if got.WifiPassword != "abcdefgh" || !hasWarning(got, "after the closing quote") {
		t.Errorf("trailing junk: %q %v", got.WifiPassword, got.Warnings)
	}
	// An unterminated quote is taken literally, with a warning.
	got = parse(t, `wifi_password = "abcdefgh`)
	if got.WifiPassword != `"abcdefgh` || !hasWarning(got, "unterminated") {
		t.Errorf("unterminated: %q %v", got.WifiPassword, got.Warnings)
	}
}

func TestParseBootConfigHashHandling(t *testing.T) {
	cases := []struct{ line, want string }{
		{"wifi_password = abc #comment", "abc"},      // " #" starts a comment
		{"wifi_password = abc\t#comment", "abc"},     // tab too
		{"wifi_password = pa#ssword1", "pa#ssword1"}, // '#' inside a word is data
		{"wifi_password=#abcdefgh", "#abcdefgh"},     // directly after '=' is data
		{"wifi_password = # nothing", ""},            // whitespace then '#': comment, empty value
		{"wifi_password = abc#def ghi #x", "abc#def ghi"},
	}
	for _, c := range cases {
		got := parse(t, c.line)
		if got.WifiPassword != c.want {
			t.Errorf("%q: got %q want %q", c.line, got.WifiPassword, c.want)
		}
	}
}

func TestParseBootConfigEqualsInValues(t *testing.T) {
	c := parse(t, "wifi_ssid = my=net=2\nwifi_password = a=b=c1234\n")
	if c.WifiSSID != "my=net=2" || c.WifiPassword != "a=b=c1234" {
		t.Fatalf("got %q / %q", c.WifiSSID, c.WifiPassword)
	}
}

func TestParseBootConfigDuplicatesLastWins(t *testing.T) {
	c := parse(t, "hostname = one\nhostname = two\n")
	if c.Hostname != "two" || !hasWarning(c, "more than once") {
		t.Fatalf("%+v", c)
	}
}

func TestParseBootConfigUnknownKeys(t *testing.T) {
	c := parse(t, "colour = blue\nhostname = emm\n")
	if c.Hostname != "emm" || !hasWarning(c, `unknown key "colour"`) {
		t.Fatalf("%+v", c)
	}
}

func TestParseBootConfigBadValues(t *testing.T) {
	c := parse(t, `rtp = maybe
ble = 2
hostname = -bad-
wifi_country = Denmark
ethernet = turbo
eth_address = 10.0.0.1
eth_gateway = nope
eth_dns = 1.1.1.1, nope, ::1
ap_ssid = `+strings.Repeat("x", 40)+`
ap_password = short
wifi_ssid =
`)
	if c.RTP != nil || c.BLE != nil {
		t.Error("bad booleans must stay unset")
	}
	if c.Hostname != "" || c.WifiCountry != "" || c.Ethernet != "" || c.EthAddress != "" || c.EthGateway != "" {
		t.Errorf("bad values leaked through: %+v", c)
	}
	if !reflect.DeepEqual(c.EthDNS, []string{"1.1.1.1"}) {
		t.Errorf("dns %v", c.EthDNS)
	}
	if c.APSSID != "" || c.APPassword != "" || c.WifiSSID != "" {
		t.Errorf("ap/wifi leaked: %+v", c)
	}
	for _, sub := range []string{"rtp must be", "ble must be", "not a valid host name", "two-letter country", "ethernet must be",
		"eth_address must be", "eth_gateway must be", "not an IPv4 address", "ap_ssid ignored", "will be OPEN", "wifi_ssid ignored"} {
		if !hasWarning(c, sub) {
			t.Errorf("missing warning containing %q in %v", sub, c.Warnings)
		}
	}
}

func TestParseBootConfigBooleanSpellings(t *testing.T) {
	for _, v := range []string{"on", "ON", "true", "Yes", "1"} {
		c := parse(t, "rtp = "+v)
		if c.RTP == nil || !*c.RTP {
			t.Errorf("%s should be true", v)
		}
	}
	for _, v := range []string{"off", "False", "no", "0"} {
		c := parse(t, "ble = "+v)
		if c.BLE == nil || *c.BLE {
			t.Errorf("%s should be false", v)
		}
	}
}

func TestParseBootConfigShortWifiPasswordKeptWithWarning(t *testing.T) {
	c := parse(t, "wifi_ssid = Net\nwifi_password = abc\n")
	if c.WifiPassword != "abc" || !hasWarning(c, "wifi_password looks wrong") {
		t.Fatalf("%+v", c)
	}
}

func TestParseBootConfigSecretsNotInJSON(t *testing.T) {
	c := parse(t, "wifi_password = secretsecret\nap_password = apsecretsecret\nweb_password = websecret\n")
	b, _ := jsonMarshal(c)
	for _, s := range []string{"secretsecret", "apsecretsecret", "websecret"} {
		if strings.Contains(string(b), s) {
			t.Errorf("secret %q leaked into JSON: %s", s, b)
		}
	}
}

func TestLoadBootConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadBootConfig(dir)
	if err != nil || c.Hostname != "" || len(c.Warnings) != 0 {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	if err := os.WriteFile(filepath.Join(dir, BootFileName), []byte("hostname = kitchen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = LoadBootConfig(dir)
	if err != nil || c.Hostname != "kitchen" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestParseBootConfigHugeFileIsCapped(t *testing.T) {
	big := "hostname = emm\n" + strings.Repeat("# filler\n", 20000)
	c := parse(t, big)
	if c.Hostname != "emm" || !hasWarning(c, "larger than") {
		t.Fatalf("hostname %q warnings %v", c.Hostname, c.Warnings)
	}
}

func TestValidHostname(t *testing.T) {
	good := []string{"emm", "a", "studio-1", "0abc", strings.Repeat("a", 63)}
	bad := []string{"", "-emm", "emm-", "Emm", "em_m", "em.m", "em m", strings.Repeat("a", 64), "émm"}
	for _, h := range good {
		if !ValidHostname(h) {
			t.Errorf("%q should be valid", h)
		}
	}
	for _, h := range bad {
		if ValidHostname(h) {
			t.Errorf("%q should be invalid", h)
		}
	}
}

// R4: a malformed line must never put its value (or part of it) into a
// warning: warnings reach the journal and the unauthenticated setup-mode API.
func TestParseBootConfigMalformedLinesNeverEchoSecrets(t *testing.T) {
	cases := []struct {
		line    string
		secrets []string
		want    string // substring that must be in the warning
	}{
		{"wifi_password: MyHomePass99", []string{"MyHomePass99"}, "no '=' after wifi_password"},
		{"web_password hunter2hunter2", []string{"hunter2hunter2"}, "no '=' after web_password"},
		{"Wifi_Password   MyHomePass99", []string{"MyHomePass99"}, "no '=' after wifi_password"},
		{"supersecretvalue1234", []string{"supersecretvalue1234"}, "expected key = value"},
		{"wifi_password: Zq7pass=Xk9word", []string{"Zq7pass", "Xk9word"}, "unknown key"},
		{"my secret Zq7pass = Xk9word", []string{"secret", "Zq7pass", "Xk9word"}, "unknown key"},
		{`ble_name: "Zq7pass=Xk9word`, []string{"Zq7pass", "Xk9word"}, "unknown key"},
		{`wifi_password = "Xk9word-abc" Zq7pass`, []string{"Xk9word-abc", "Zq7pass"}, "after the closing quote"},
		{`wifi_pasword = Xk9word-typo`, []string{"Xk9word-typo"}, "wifi_pasword"}, // a mistyped key is shown, its value is not
	}
	for _, c := range cases {
		got := parse(t, c.line)
		if len(got.Warnings) == 0 {
			t.Errorf("%q: no warning at all", c.line)
			continue
		}
		all := strings.Join(got.Warnings, "\n")
		for _, s := range c.secrets {
			if strings.Contains(all, s) {
				t.Errorf("%q: warning leaks %q: %v", c.line, s, got.Warnings)
			}
		}
		if !strings.Contains(all, c.want) || !strings.Contains(all, "line 1") {
			t.Errorf("%q: warnings %v lack %q and the line number", c.line, got.Warnings, c.want)
		}
	}
}

func TestBootWarningsDoNotReachStatus(t *testing.T) {
	e := newEnv(t, withBootFile("wifi_password: MyHomePass99\nweb_password hunter2hunter2\n"), withCable())
	e.start()
	st := e.m.Status()
	if len(st.Warnings) != 2 {
		t.Fatalf("warnings %v", st.Warnings)
	}
	b, _ := jsonMarshal(st)
	for _, s := range []string{"MyHomePass99", "hunter2hunter2"} {
		if strings.Contains(string(b), s) {
			t.Errorf("status JSON leaks %q: %s", s, b)
		}
	}
}

// R9: an invalid ap_password silently makes the setup network open; the warning
// must be the first one, whatever else the file has wrong.
func TestInvalidAPPasswordWarningComesFirst(t *testing.T) {
	c := parse(t, "bogus = 1\nrtp = maybe\nap_password = short\nble = 2\n")
	if len(c.Warnings) < 4 {
		t.Fatalf("warnings %v", c.Warnings)
	}
	w := c.Warnings[0]
	if !strings.Contains(w, "ap_password") || !strings.Contains(w, "OPEN") || !strings.Contains(w, "line 3") {
		t.Fatalf("first warning %q", w)
	}
	if c.APPassword != "" {
		t.Fatal("an invalid password must not be used")
	}
}

// web-daemon R2: emm.txt can add names to the web server's Host allow-list, since
// the appliance has no way to pass --allowed-hosts.
func TestParseBootConfigAllowedHosts(t *testing.T) {
	c := parse(t, "allowed_hosts = emm.fritz.box, .MyDomain.example. ;nas.example.net\tpi.lan,,  , EMM.fritz.box\n")
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings: %v", c.Warnings)
	}
	// lower case, trailing dot gone, blanks dropped, repeats removed, order kept
	want := []string{"emm.fritz.box", ".mydomain.example", "nas.example.net", "pi.lan"}
	if !reflect.DeepEqual(c.AllowedHosts, want) {
		t.Fatalf("got %q want %q", c.AllowedHosts, want)
	}

	// Quotes and comments work as for every other key.
	c = parse(t, `allowed_hosts = "emm.fritz.box,.example.org"  # my router`)
	if !reflect.DeepEqual(c.AllowedHosts, []string{"emm.fritz.box", ".example.org"}) || len(c.Warnings) != 0 {
		t.Fatalf("%q %v", c.AllowedHosts, c.Warnings)
	}

	// Empty clears the list; a later line replaces an earlier one (with the usual warning).
	c = parse(t, "allowed_hosts =\n")
	if len(c.AllowedHosts) != 0 || len(c.Warnings) != 0 {
		t.Fatalf("empty value: %q %v", c.AllowedHosts, c.Warnings)
	}
	c = parse(t, "allowed_hosts = a.example\nallowed_hosts = b.example\n")
	if !reflect.DeepEqual(c.AllowedHosts, []string{"b.example"}) || !hasWarning(c, "more than once") {
		t.Fatalf("%q %v", c.AllowedHosts, c.Warnings)
	}

	// A line without '=' is reported by key name only.
	c = parse(t, "allowed_hosts emm.fritz.box\n")
	if len(c.AllowedHosts) != 0 || !hasWarning(c, "no '=' after allowed_hosts") || hasWarning(c, "fritz") {
		t.Fatalf("%q %v", c.AllowedHosts, c.Warnings)
	}
}

func TestParseBootConfigAllowedHostsBadEntries(t *testing.T) {
	long := strings.Repeat("a", 64) + ".example"
	line := "allowed_hosts = good.example, http://user:hunter2@nas.example.net/, nas.example.net:8080, *.example.org, " +
		"[::1], ünï.example, ., ..example.org, a..b, " + long + ", fine.example"
	c := parse(t, line)
	if want := []string{"good.example", "fine.example"}; !reflect.DeepEqual(c.AllowedHosts, want) {
		t.Fatalf("got %q want %q", c.AllowedHosts, want)
	}
	if len(c.Warnings) != 9 {
		t.Fatalf("want one warning per bad entry, got %d: %v", len(c.Warnings), c.Warnings)
	}
	all := strings.Join(c.Warnings, "\n")
	for _, s := range []string{"hunter2", "user:", "8080", "ünï", "::1", "example.org", "nas.example"} {
		if strings.Contains(all, s) { // a rejected entry is never echoed, only its position
			t.Errorf("a warning echoes %q: %v", s, c.Warnings)
		}
	}
	for _, sub := range []string{"line 1", "allowed_hosts", "not a plain host name", "entry 2", "entry 3", "entry 4", "entry 10"} {
		if !strings.Contains(all, sub) {
			t.Errorf("warnings lack %q: %v", sub, c.Warnings)
		}
	}
}

// The example file shows allowed_hosts; what it shows must parse cleanly.
func TestExampleFileAllowedHostsLineParses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "emm.txt.example"))
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "# allowed_hosts") {
			line = strings.TrimPrefix(l, "# ")
		}
	}
	if line == "" {
		t.Fatal("packaging/emm.txt.example has no '# allowed_hosts = ...' line")
	}
	c := parse(t, line)
	if len(c.Warnings) != 0 || len(c.AllowedHosts) == 0 {
		t.Fatalf("%q gave %q, warnings %v", line, c.AllowedHosts, c.Warnings)
	}
}
