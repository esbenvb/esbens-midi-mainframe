package netcfg

import (
	"reflect"
	"testing"
)

func TestSplitTerse(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a:b:c", []string{"a", "b", "c"}},
		{`*:Studio\:5G:80:WPA2`, []string{"*", "Studio:5G", "80", "WPA2"}},
		{`x:back\\slash:1`, []string{"x", `back\slash`, "1"}},
		{":::", []string{"", "", "", ""}},
		{"", []string{""}},
		{`trailing\`, []string{`trailing\`}},
	}
	for _, c := range cases {
		if got := splitTerse(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitTerse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseWifiList(t *testing.T) {
	out := ` :HomeNet:55:WPA2
*:HomeNet:81:WPA2
 :HomeNet:30:WPA2
 :Studio\:5G:40:WPA2 WPA3
 :CoffeeShop:38:--
 ::70:WPA2
 :Weak:0:WPA2
 :Loud:250:WPA3
 :NoSecurityField:12:

garbage
`
	got := parseWifiList(out)
	want := []Network{
		{SSID: "Loud", Signal: 100, Security: "WPA3"},
		{SSID: "HomeNet", Signal: 81, Security: "WPA2", InUse: true},
		{SSID: "Studio:5G", Signal: 40, Security: "WPA2 WPA3"},
		{SSID: "CoffeeShop", Signal: 38, Security: "open"},
		{SSID: "NoSecurityField", Signal: 12, Security: "open"},
		{SSID: "Weak", Signal: 0, Security: "WPA2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if !got[3].Open() || got[0].Open() {
		t.Error("Open() wrong")
	}
}

func TestParseWifiListDuplicateKeepsStrongestSecurity(t *testing.T) {
	// The strongest access point's security wins, InUse is sticky.
	got := parseWifiList("*:Net:20:WPA2\n :Net:90:WPA3\n")
	if len(got) != 1 || got[0].Signal != 90 || got[0].Security != "WPA3" || !got[0].InUse {
		t.Fatalf("%+v", got)
	}
}

func TestParseDevicesAndPick(t *testing.T) {
	out := `eth0:ethernet:connected:emm-eth
usb0:ethernet:unmanaged:
wlan0:wifi:connecting (getting IP configuration):emm-wifi
p2p-dev-wlan0:wifi-p2p:disconnected:
lo:loopback:unmanaged:
`
	devs := parseDevices(out)
	if len(devs) != 5 {
		t.Fatalf("%d devices: %+v", len(devs), devs)
	}
	if devs[2].State != "connecting" || devs[2].Conn != "emm-wifi" {
		t.Errorf("wlan0: %+v", devs[2])
	}
	wifi, eth := pickDevices(devs, "", "")
	if wifi != "wlan0" || eth != "eth0" {
		t.Errorf("picked %q/%q", wifi, eth)
	}
	// An unmanaged Ethernet device is not chosen when it comes first.
	devs2 := parseDevices("usb0:ethernet:unmanaged:\nend0:ethernet:disconnected:\n")
	if _, eth := pickDevices(devs2, "", ""); eth != "end0" {
		t.Errorf("picked %q", eth)
	}
	wifi, eth = pickDevices(devs, "wlan1", "enx1")
	if wifi != "wlan1" || eth != "enx1" {
		t.Errorf("overrides ignored: %q/%q", wifi, eth)
	}
	if d, ok := findDevice(devs, "wlan0"); !ok || d.Name != "wlan0" {
		t.Error("findDevice")
	}
	if _, ok := findDevice(devs, "nope"); ok {
		t.Error("findDevice found a ghost")
	}
}

func TestParseIPv4(t *testing.T) {
	cases := map[string]string{
		"192.168.1.50/24\n":                "192.168.1.50",
		"192.168.1.50/24 | 10.0.0.2/8":     "192.168.1.50",
		"IP4.ADDRESS[1]:192.168.1.50/24\n": "192.168.1.50",
		"":                                 "",
		"fe80::1/64\n":                     "",
		"fe80::1/64\n10.0.0.9/24\n":        "10.0.0.9",
		"garbage":                          "",
		"192.168.1.50":                     "192.168.1.50",
	}
	for in, want := range cases {
		if got := parseIPv4(in); got != want {
			t.Errorf("parseIPv4(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMACAndSuffix(t *testing.T) {
	mac := parseMAC(`B8\:27\:EB\:3F\:A1\:C2` + "\n")
	if mac != "b8:27:eb:3f:a1:c2" {
		t.Fatalf("mac %q", mac)
	}
	if s := macSuffix(mac); s != "A1C2" {
		t.Errorf("suffix %q", s)
	}
	if parseMAC("nonsense") != "" || macSuffix("nonsense") != "" {
		t.Error("garbage must yield empty")
	}
}

func TestFriendlyNMError(t *testing.T) {
	cases := map[string]string{
		"Error: Connection activation failed: (7) Secrets were required, but not provided.": "wrong password, or the network refused the connection",
		"Error: Connection activation failed: No network with SSID 'x' found.":              "network not found",
		"Error: Timeout expired (45 seconds)":                                               "timed out",
		"Error: something unusual":                                                          "something unusual",
	}
	for in, want := range cases {
		err := &RunError{Cmd: "nmcli", Stderr: in + "\n", Err: errTest}
		if got := friendlyNMError(err); got != want {
			t.Errorf("friendlyNMError(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseProfilesAndForeignWifi(t *testing.T) {
	out := `preconfigured:802-11-wireless:yes
Wired connection 1:ethernet:yes
Home\:Net:wifi:yes
manual-only:wifi:no
emm-wifi:wifi:yes
emm-eth:ethernet:yes
emm-setup-ap:wifi:no
Hotspot:wifi:yes

garbage
:wifi:yes
`
	ps := parseProfiles(out)
	if len(ps) != 8 {
		t.Fatalf("%d profiles: %+v", len(ps), ps)
	}
	if ps[2].Name != "Home:Net" || ps[2].Type != "wifi" || !ps[2].Autoconnect || ps[3].Autoconnect {
		t.Errorf("fields: %+v", ps[2:4])
	}
	// EMM's own profiles and anything that cannot autoconnect do not count.
	want := []string{"preconfigured", "Home:Net", "Hotspot"}
	if got := foreignWifiProfiles(ps); !reflect.DeepEqual(got, want) {
		t.Errorf("foreignWifiProfiles = %q, want %q", got, want)
	}
	if foreignWifiProfiles(nil) != nil {
		t.Error("nothing in, nothing out")
	}
	if !isEMMConn("emm-wifi") || !isEMMConn("emm-setup-ap") || isEMMConn("preconfigured") || isEMMConn("my emm-wifi") {
		t.Error("isEMMConn")
	}
}

func TestUnescapeTerse(t *testing.T) {
	cases := map[string]string{
		`Studio\:5G`:  "Studio:5G",
		`back\\slash`: `back\slash`,
		"plain":       "plain",
		"":            "",
		`trailing\`:   `trailing\`,
	}
	for in, want := range cases {
		if got := unescapeTerse(in); got != want {
			t.Errorf("unescapeTerse(%q) = %q, want %q", in, got, want)
		}
	}
}
