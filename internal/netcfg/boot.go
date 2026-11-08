package netcfg

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// BootFileName is the hand-edited config file on the FAT32 partition.
const BootFileName = "emm.txt"

// ResetFileName, placed next to emm.txt, forces setup mode on the next boot.
const ResetFileName = "reset-network"

// maxBootFile caps how much of emm.txt is read.
const maxBootFile = 64 << 10

// BootConfig is what emm.txt says. Empty strings / nil pointers mean "not set";
// anything that is set wins over settings saved by the web UI.
//
// Secrets are tagged json:"-" so a BootConfig can be logged or serialised
// without leaking them.
type BootConfig struct {
	Hostname     string   `json:"hostname,omitempty"`
	WifiCountry  string   `json:"wifi_country,omitempty"`
	WifiSSID     string   `json:"wifi_ssid,omitempty"`
	WifiPassword string   `json:"-"`
	Ethernet     string   `json:"ethernet,omitempty"` // auto | dhcp | static | off
	EthAddress   string   `json:"eth_address,omitempty"`
	EthGateway   string   `json:"eth_gateway,omitempty"`
	EthDNS       []string `json:"eth_dns,omitempty"`
	APSSID       string   `json:"ap_ssid,omitempty"`
	APPassword   string   `json:"-"`
	BLEName      string   `json:"ble_name,omitempty"`
	WebPassword  string   `json:"-"`
	RTP          *bool    `json:"rtp,omitempty"`
	BLE          *bool    `json:"ble,omitempty"`
	// AllowedHosts are extra host names the web UI may be opened with (the Host
	// header allow-list, see the web package): lower-case names without a port,
	// a leading dot meaning "every name below this domain".
	AllowedHosts []string `json:"allowed_hosts,omitempty"`

	// Warnings lists problems found while parsing (unknown keys, bad values,
	// duplicates). The daemon should log them and may show them in the UI.
	Warnings []string `json:"warnings,omitempty"`
}

// ParseBootConfig parses emm.txt. It is deliberately lenient: it never fails
// on content, only on I/O errors. Problems become Warnings and the offending
// line is ignored.
//
// Format: one `key = value` per line, keys case-insensitive.
//
//   - Blank lines and lines whose first non-blank character is '#' are comments.
//   - A UTF-8 BOM and CRLF line endings are tolerated.
//   - Values may be wrapped in "double" or 'single' quotes. Inside quotes
//     everything is literal (including '#', '=' and spaces); there are no
//     escape sequences, so use the other quote character if you need one.
//   - Unquoted values end at a '#' that is preceded by whitespace
//     ("pass #comment" is "pass"); a '#' inside a word ("pa#ss") or directly
//     after the '=' ("key=#x") is part of the value. A value that starts with
//     '#' after whitespace, or contains " #", must therefore be quoted.
//   - '=' inside a value is fine: only the first '=' on the line splits.
//   - Duplicate keys: the last one wins, with a warning.
func ParseBootConfig(r io.Reader) (BootConfig, error) {
	var c BootConfig
	data, err := io.ReadAll(io.LimitReader(r, maxBootFile+1))
	if err != nil {
		return c, err
	}
	if len(data) > maxBootFile {
		data = data[:maxBootFile]
		c.warn(0, "file larger than %d bytes, the rest was ignored", maxBootFile)
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")

	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		line = strings.TrimRight(line, "\r")
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		eq := strings.IndexByte(t, '=')
		if eq < 0 {
			// Never echo the line: "wifi_password secret1234" is a password.
			if k := knownKeyOf(t); k != "" {
				c.warn(n, "expected key = value: no '=' after %s", k)
			} else {
				c.warn(n, "expected key = value (this line has no '=')")
			}
			continue
		}
		key := strings.ToLower(strings.TrimSpace(t[:eq]))
		val, w := parseValue(t[eq+1:])
		if w != "" {
			c.warn(n, "%s: %s", keyLabel(key), w)
		}
		if seen[key] {
			c.warn(n, "%s is set more than once, the last value wins", key)
		}
		if c.assign(n, key, val) {
			seen[key] = true
		}
	}
	return c, nil
}

// LoadBootConfig reads dir/emm.txt. A missing file is not an error and yields
// the zero BootConfig.
func LoadBootConfig(dir string) (BootConfig, error) {
	f, err := os.Open(filepath.Join(dir, BootFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BootConfig{}, nil
		}
		return BootConfig{}, err
	}
	defer f.Close()
	return ParseBootConfig(f)
}

func (c *BootConfig) warn(line int, format string, a ...any) {
	c.Warnings = append(c.Warnings, bootWarning(line, format, a...))
}

// warnFirst is warn for problems that must not be overlooked: the warning goes
// to the front of the list.
func (c *BootConfig) warnFirst(line int, format string, a ...any) {
	c.Warnings = append([]string{bootWarning(line, format, a...)}, c.Warnings...)
}

func bootWarning(line int, format string, a ...any) string {
	msg := fmt.Sprintf(format, a...)
	if line > 0 {
		return fmt.Sprintf("%s line %d: %s", BootFileName, line, msg)
	}
	return BootFileName + ": " + msg
}

// bootKeys are the keys emm.txt understands.
var bootKeys = map[string]bool{
	"hostname": true, "wifi_country": true, "wifi_ssid": true, "wifi_password": true,
	"ethernet": true, "eth_address": true, "eth_gateway": true, "eth_dns": true,
	"ap_ssid": true, "ap_password": true, "rtp": true, "ble": true, "ble_name": true,
	"web_password": true, "allowed_hosts": true,
}

// firstToken returns the lower-cased text up to the first blank, ':' or '='.
func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t:="); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// knownKeyOf returns the key a line starts with if it is one of ours, or "".
// Only a name from bootKeys is ever returned, never text from the line.
func knownKeyOf(line string) string {
	if k := firstToken(line); bootKeys[k] {
		return k
	}
	return ""
}

// keyLabel makes the text left of '=' safe to put in a warning. Warnings reach
// the journal and the (unauthenticated, in setup mode) status API, and a
// malformed line such as `wifi_password: abc=def` has half of a secret in
// that position. It returns the first token if it looks like a key name,
// otherwise a placeholder.
func keyLabel(key string) string {
	k := firstToken(key)
	if k == "" || len(k) > 32 {
		return "(key)"
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "(key)"
		}
	}
	return k
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// parseValue extracts the value from the text after '='.
func parseValue(raw string) (val, warning string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	if q := s[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			rest := strings.TrimSpace(s[2+end:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				warning = "text after the closing quote was ignored"
			}
			return s[1 : 1+end], warning
		}
		warning = "unterminated quote, value taken literally"
		return s, warning
	}
	// Unquoted: a '#' preceded by whitespace starts a comment. Work on the raw
	// text so that "key = #x" (comment, empty value) differs from "key=#x".
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			return strings.TrimSpace(raw[:i]), ""
		}
	}
	return s, ""
}

func (c *BootConfig) assign(n int, key, val string) bool {
	switch key {
	case "hostname":
		h := strings.ToLower(val)
		if !ValidHostname(h) {
			c.warn(n, "hostname %q is not a valid host name (a-z, 0-9, '-', max 63, no leading/trailing '-')", clip(val))
			return false
		}
		c.Hostname = h
	case "wifi_country":
		cc := strings.ToUpper(val)
		if !validCountry(cc) {
			c.warn(n, "wifi_country must be a two-letter country code such as DK, got %q", clip(val))
			return false
		}
		c.WifiCountry = cc
	case "wifi_ssid":
		if err := validateSSID(val); err != nil {
			c.warn(n, "wifi_ssid ignored: %v", err)
			return false
		}
		c.WifiSSID = val
	case "wifi_password":
		if err := validatePSK(val); err != nil {
			// Kept as given: joining will fail and say why, and the device falls
			// back to setup mode, which is recoverable.
			c.warn(n, "wifi_password looks wrong: %v", err)
		}
		c.WifiPassword = val
	case "ethernet":
		m := strings.ToLower(val)
		if !validEthMode(m) {
			c.warn(n, "ethernet must be auto, dhcp, static or off, got %q", clip(val))
			return false
		}
		c.Ethernet = m
	case "eth_address":
		if val != "" {
			ip, _, err := net.ParseCIDR(val)
			if err != nil || ip.To4() == nil {
				c.warn(n, "eth_address must be an IPv4 address with prefix length, e.g. 192.168.1.50/24, got %q", clip(val))
				return false
			}
		}
		c.EthAddress = val
	case "eth_gateway":
		if val != "" && !isIPv4(val) {
			c.warn(n, "eth_gateway must be an IPv4 address, got %q", clip(val))
			return false
		}
		c.EthGateway = val
	case "eth_dns":
		c.EthDNS = nil
		for _, f := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == ';' }) {
			if !isIPv4(f) {
				c.warn(n, "eth_dns: %q is not an IPv4 address, skipped", clip(f))
				continue
			}
			c.EthDNS = append(c.EthDNS, f)
		}
	case "ap_ssid":
		if val == "" {
			c.APSSID = ""
			break
		}
		if err := validateSSID(val); err != nil {
			c.warn(n, "ap_ssid ignored: %v", err)
			return false
		}
		c.APSSID = val
	case "ap_password":
		if err := validatePSK(val); err != nil {
			// Security relevant, so it goes first in Warnings (and so first in the UI).
			c.warnFirst(n, "ap_password ignored: the setup network will be OPEN, anyone in range can join it (%v)", err)
			return false
		}
		c.APPassword = val
	case "rtp", "ble":
		b, ok := parseBool(val)
		if !ok {
			c.warn(n, "%s must be on/off (or true/false, yes/no, 1/0), got %q", key, clip(val))
			return false
		}
		if key == "rtp" {
			c.RTP = &b
		} else {
			c.BLE = &b
		}
	case "ble_name":
		if hasControl(val) {
			c.warn(n, "ble_name contains control characters, ignored")
			return false
		}
		c.BLEName = val
	case "web_password":
		c.WebPassword = val
	case "allowed_hosts":
		c.AllowedHosts = nil
		seen := map[string]bool{}
		for i, f := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == ';' }) {
			h, ok := cleanAllowedHost(f)
			if !ok {
				// The entry is not echoed: it may be anything the user typed, a URL
				// with a password in it, say, and warnings reach the journal and the
				// status API. The position is enough to find it.
				c.warn(n, "allowed_hosts: entry %d is not a plain host name (names only: no http://, port, path or wildcard; "+
					"a leading dot allows a whole domain), skipped", i+1)
				continue
			}
			if !seen[h] {
				seen[h] = true
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
	default:
		c.warn(n, "unknown key %q ignored", keyLabel(key))
		return false
	}
	return true
}

// cleanAllowedHost checks one allowed_hosts entry and returns it in the form
// the web server compares with: lower case, no trailing dot. An entry is a DNS
// name (labels of letters, digits, '-' and '_', at most 63 characters each, 253
// in all), optionally with one leading dot for "this domain and everything below
// it". Anything else (a URL, a port, a path, a wildcard, an IPv6 literal, a
// non-ASCII name) is refused.
func cleanAllowedHost(f string) (string, bool) {
	h := strings.TrimSuffix(strings.ToLower(f), ".")
	name := strings.TrimPrefix(h, ".")
	if name == "" || len(name) > 253 {
		return "", false
	}
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '.' {
			c := name[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", false
			}
			continue
		}
		if i == start || i-start > 63 { // empty or over-long label
			return "", false
		}
		start = i + 1
	}
	return h, true
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "1":
		return true, true
	case "off", "false", "no", "0":
		return false, true
	}
	return false, false
}

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && !strings.Contains(s, ":")
}

func validCountry(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func validEthMode(m string) bool {
	switch m {
	case "auto", "dhcp", "static", "off":
		return true
	}
	return false
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// DefaultHostname is the name the appliance image (/etc/hostname) and the Pi OS
// installer give a fresh box. Manager no longer applies it: it renames the
// machine only when emm.txt or the saved UI settings choose a name, and it
// reports "emm" in Status.Hostname only if the machine's own name cannot be
// determined at all.
const DefaultHostname = "emm"

// ValidHostname reports whether h is a valid lower-case RFC 1123 host name label.
func ValidHostname(h string) bool {
	if len(h) == 0 || len(h) > 63 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i != 0 && i != len(h)-1:
		default:
			return false
		}
	}
	return true
}
