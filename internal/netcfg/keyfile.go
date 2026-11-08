package netcfg

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Connection IDs of the profiles EMM owns. The files are derived data: they
// are re-rendered from emm.txt + saved settings on every boot.
const (
	wifiConnID = "emm-wifi"
	apConnID   = "emm-setup-ap"
	ethConnID  = "emm-eth"

	// APAddress is the gateway address of the setup network; the captive
	// portal lives here.
	APAddress = "10.42.0.1"
	apCIDR    = APAddress + "/24"

	// dnsmasqSnippet is the file name of the wildcard-DNS snippet that makes
	// every host name resolve to the Pi while the setup network is up.
	dnsmasqSnippet = "emm-captive.conf"
)

// ErrInvalid is wrapped by errors about bad user input (HTTP 400 material).
var ErrInvalid = errors.New("invalid value")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// validateSSID checks the length of a WiFi network name (1..32 bytes).
func validateSSID(s string) error {
	if len(s) == 0 {
		return invalid("network name is empty")
	}
	if len(s) > 32 {
		return invalid("network name is %d bytes, the maximum is 32", len(s))
	}
	return nil
}

// validatePSK checks a WPA passphrase: empty (open network), 8..63 characters,
// or a 64-digit hex key.
func validatePSK(p string) error {
	if p == "" {
		return nil
	}
	if len(p) == 64 && isHex(p) {
		return nil
	}
	if len(p) < 8 || len(p) > 63 {
		return invalid("a WiFi password must be 8-63 characters (or 64 hex digits), this one is %d", len(p))
	}
	if !utf8.ValidString(p) || hasControl(p) {
		return invalid("the WiFi password contains control or invalid characters")
	}
	return nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// kfEscape escapes a string value the way GLib key files (and so NetworkManager
// keyfiles) expect: backslash, newline, tab, CR, and leading/trailing spaces
// (which would otherwise be trimmed).
func kfEscape(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case ' ':
			if i == 0 || i == len(s)-1 {
				b.WriteString(`\s`)
			} else {
				b.WriteByte(' ')
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ssidValue renders an SSID. NetworkManager's keyfile reader (nm-keyfile.c,
// ssid_parser/get_bytes) takes `ssid=` as a plain string with two traps:
//
//   - a value made only of digits, ';' and white space may be read as the
//     obsolete decimal byte list ("49;50;"), so it is not safe as a string;
//   - in a string, the two characters `\;` are read as an escaped ';' (the
//     writer escapes ';' that way), so an SSID that itself contains a backslash
//     followed by ';' would lose the backslash.
//
// Both, anything unprintable and anything that is not valid UTF-8 are written
// as an explicit byte list, which is unambiguous. An SSID with any backslash
// takes that form too: they are rare and the list needs no escaping at all.
func ssidValue(ssid string) string {
	plain := utf8.ValidString(ssid)
	if plain {
		for _, r := range ssid {
			if !unicode.IsPrint(r) {
				plain = false
				break
			}
		}
	}
	if plain && strings.Trim(ssid, "0123456789; \t\n\v\f\r") == "" {
		plain = false
	}
	if plain && strings.Contains(ssid, `\`) {
		plain = false
	}
	if plain {
		return kfEscape(ssid)
	}
	var b strings.Builder
	for i := 0; i < len(ssid); i++ {
		b.WriteString(strconv.Itoa(int(ssid[i])))
		b.WriteByte(';')
	}
	return b.String()
}

// uuidFor derives a stable name-based (v5-style) UUID so that re-rendering a
// profile keeps its identity.
func uuidFor(name string) string {
	h := sha1.Sum([]byte("emm-midi-mainframe:" + name))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// renderWifiClient renders the keyfile for joining a WiFi network. An empty psk
// means an open network.
func renderWifiClient(ssid, psk string) (string, error) {
	return renderWifiClientOpts(ssid, psk, false)
}

// renderWifiClientOpts is renderWifiClient with optionally relaxed passphrase
// length rules (see Options.RelaxedPSK). Control characters stay forbidden.
func renderWifiClientOpts(ssid, psk string, relaxed bool) (string, error) {
	return renderWifiProfile(ssid, psk, relaxed, false)
}

// renderWifiProfile is renderWifiClientOpts for a network that may not
// broadcast its name. With hidden set the profile carries `hidden=true`, which
// makes NetworkManager probe for the SSID itself: without it a profile for a
// non-broadcasting network is not available to autoconnect (after a reboot),
// because no access point with that name ever shows up in a scan.
func renderWifiProfile(ssid, psk string, relaxed, hidden bool) (string, error) {
	if err := validateSSID(ssid); err != nil {
		return "", err
	}
	if relaxed {
		if hasControl(psk) {
			return "", invalid("the WiFi password contains control characters")
		}
	} else if err := validatePSK(psk); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[connection]\n")
	b.WriteString("id=" + wifiConnID + "\n")
	b.WriteString("uuid=" + uuidFor(wifiConnID) + "\n")
	b.WriteString("type=wifi\n")
	b.WriteString("autoconnect=true\n")
	b.WriteString("autoconnect-priority=10\n")
	b.WriteString("autoconnect-retries=0\n")
	b.WriteString("\n[wifi]\n")
	b.WriteString("mode=infrastructure\n")
	b.WriteString("ssid=" + ssidValue(ssid) + "\n")
	if hidden {
		b.WriteString("hidden=true\n")
	}
	b.WriteString("powersave=2\n") // power saving off: it adds latency and jitter to MIDI
	if psk != "" {
		b.WriteString("\n[wifi-security]\n")
		b.WriteString("key-mgmt=wpa-psk\n")
		b.WriteString("psk=" + kfEscape(psk) + "\n")
	}
	b.WriteString("\n[ipv4]\nmethod=auto\n")
	b.WriteString("\n[ipv6]\nmethod=auto\n")
	return b.String(), nil
}

// renderAP renders the keyfile for the setup access point. An empty psk makes
// the setup network open.
func renderAP(ssid, psk string) (string, error) {
	if err := validateSSID(ssid); err != nil {
		return "", err
	}
	if err := validatePSK(psk); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[connection]\n")
	b.WriteString("id=" + apConnID + "\n")
	b.WriteString("uuid=" + uuidFor(apConnID) + "\n")
	b.WriteString("type=wifi\n")
	b.WriteString("autoconnect=false\n") // brought up explicitly by the manager
	b.WriteString("\n[wifi]\n")
	b.WriteString("mode=ap\n")
	b.WriteString("band=bg\n")
	b.WriteString("channel=6\n")
	b.WriteString("ssid=" + ssidValue(ssid) + "\n")
	if psk != "" {
		b.WriteString("\n[wifi-security]\n")
		b.WriteString("key-mgmt=wpa-psk\n")
		b.WriteString("proto=rsn;\n")
		b.WriteString("pairwise=ccmp;\n")
		b.WriteString("group=ccmp;\n")
		b.WriteString("psk=" + kfEscape(psk) + "\n")
	}
	b.WriteString("\n[ipv4]\n")
	b.WriteString("address1=" + apCIDR + "\n")
	b.WriteString("method=shared\n")
	b.WriteString("\n[ipv6]\nmethod=ignore\n")
	return b.String(), nil
}

// normalizeEthernet fills defaults and validates.
func normalizeEthernet(e EthernetSettings) (EthernetSettings, error) {
	e.Mode = strings.ToLower(strings.TrimSpace(e.Mode))
	if e.Mode == "" {
		e.Mode = "auto"
	}
	if !validEthMode(e.Mode) {
		return e, invalid("ethernet mode must be auto, dhcp, static or off, not %q", e.Mode)
	}
	e.Address = strings.TrimSpace(e.Address)
	e.Gateway = strings.TrimSpace(e.Gateway)
	if e.Mode != "static" {
		// Static fields are meaningless for the other modes; drop them so a
		// stale address can't come back later.
		return EthernetSettings{Mode: e.Mode}, nil
	}
	ip, ipnet, err := net.ParseCIDR(e.Address)
	if err != nil || ip.To4() == nil {
		return e, invalid("a static address needs an IPv4 address with prefix length, e.g. 192.168.1.50/24")
	}
	if e.Gateway != "" {
		if !isIPv4(e.Gateway) {
			return e, invalid("gateway %q is not an IPv4 address", e.Gateway)
		}
		if !ipnet.Contains(net.ParseIP(e.Gateway)) {
			return e, invalid("gateway %s is not inside %s", e.Gateway, ipnet)
		}
	}
	var dns []string
	for _, d := range e.DNS {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if !isIPv4(d) {
			return e, invalid("DNS server %q is not an IPv4 address", d)
		}
		dns = append(dns, d)
	}
	e.DNS = dns
	return e, nil
}

// renderEthernet renders the wired profile. For mode "off" it returns a
// profile that never auto-connects (the manager also disconnects the device).
func renderEthernet(e EthernetSettings) (string, error) {
	e, err := normalizeEthernet(e)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[connection]\n")
	b.WriteString("id=" + ethConnID + "\n")
	b.WriteString("uuid=" + uuidFor(ethConnID) + "\n")
	b.WriteString("type=ethernet\n")
	if e.Mode == "off" {
		b.WriteString("autoconnect=false\n")
	} else {
		b.WriteString("autoconnect=true\n")
		b.WriteString("autoconnect-priority=100\n")
	}
	b.WriteString("\n[ethernet]\n")
	switch e.Mode {
	case "auto":
		b.WriteString("\n[ipv4]\nmethod=auto\n")
		b.WriteString("\n[ipv6]\nmethod=auto\n")
	case "dhcp":
		b.WriteString("\n[ipv4]\nmethod=auto\nmay-fail=false\n")
		b.WriteString("\n[ipv6]\nmethod=ignore\n")
	case "static":
		addr := e.Address
		if e.Gateway != "" {
			addr += "," + e.Gateway
		}
		b.WriteString("\n[ipv4]\n")
		b.WriteString("address1=" + addr + "\n")
		if len(e.DNS) > 0 {
			b.WriteString("dns=" + strings.Join(e.DNS, ";") + ";\n")
		}
		b.WriteString("method=manual\n")
		b.WriteString("\n[ipv6]\nmethod=ignore\n")
	case "off":
		b.WriteString("\n[ipv4]\nmethod=disabled\n")
		b.WriteString("\n[ipv6]\nmethod=disabled\n")
	}
	return b.String(), nil
}

// dnsmasqWildcard is the snippet that points every name at the Pi.
func dnsmasqWildcard() string {
	return "# Written by emmd: answer every DNS query with the setup address so that\n" +
		"# phones detect the captive portal.\n" +
		"address=/#/" + APAddress + "\n"
}
