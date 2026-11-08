package netcfg

import (
	"context"
	"strings"
	"testing"
)

// R1: every command runs in the C locale, whatever the daemon inherited.
func TestExecRunnerPinsLocale(t *testing.T) {
	t.Setenv("LC_ALL", "da_DK.UTF-8")
	t.Setenv("LANG", "da_DK.UTF-8")
	t.Setenv("LANGUAGE", "da:en")
	t.Setenv("LC_MESSAGES", "da_DK.UTF-8")
	t.Setenv("EMM_TEST_KEEP", "kept")

	out, err := ExecRunner{}.Run(context.Background(), "env")
	if err != nil {
		t.Skipf("no env command: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	if got["LC_ALL"] != "C" || got["LANG"] != "C" || got["LANGUAGE"] != "" {
		t.Fatalf("locale not pinned: LC_ALL=%q LANG=%q LANGUAGE=%q", got["LC_ALL"], got["LANG"], got["LANGUAGE"])
	}
	if got["EMM_TEST_KEEP"] != "kept" {
		t.Fatal("the rest of the environment must be passed on")
	}
}

func TestCmdEnvReplacesLocaleVariables(t *testing.T) {
	env := cmdEnv([]string{"PATH=/bin", "LANG=de_DE.UTF-8", "LC_ALL=fr_FR", "LANGUAGE=fr", "HOME=/root"})
	want := []string{"PATH=/bin", "HOME=/root", "LC_ALL=C", "LANG=C", "LANGUAGE="}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v want %v", env, want)
	}
}

func TestParseDevicesStateIsCaseInsensitive(t *testing.T) {
	devs := parseDevices("wlan0:wifi:Connected:HomeNet\neth0:ethernet:UNAVAILABLE:\n")
	if devs[0].State != "connected" || devs[1].State != "unavailable" {
		t.Fatalf("%+v", devs)
	}
}
