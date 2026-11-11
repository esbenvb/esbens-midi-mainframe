// Package netcfg manages EMM's network: boot-file config, WiFi and Ethernet
// through NetworkManager, the setup-mode access point and the captive portal.
//
// # Model
//
// The appliance's root filesystem lives in RAM, so nothing in /etc survives a
// reboot. All persistent network configuration is therefore kept in exactly two
// places, and everything NetworkManager sees is rendered from them at every
// boot:
//
//   - emm.txt on the FAT32 partition: hand-edited, wins for every key it
//     contains (see ParseBootConfig for the format);
//   - Saved: what the web UI changed, persisted by the caller (the daemon keeps
//     it in system.json, with the web password and SSH settings) through the Load and Save hooks.
//
// The rendered NetworkManager keyfiles (emm-wifi, emm-eth, emm-setup-ap in
// /etc/NetworkManager/system-connections, mode 0600) are derived data, written
// as files rather than with `nmcli ... password X` so that WiFi passwords never
// show up in a process list. They are the only profiles EMM ever creates or
// deletes: on Raspberry Pi OS, where WiFi may have been set up with Imager,
// nmtui or raspi-config before EMM was installed, every other profile is
// somebody else's and is left alone (see "Existing connectivity" below).
// emm-eth is not always written (see "Ethernet profile").
//
// # Boot sequence (Manager.Run)
//
//  1. Read emm.txt and the saved settings; note and delete reset-network.
//  2. rfkill unblock and the regulatory domain (wifi_country), once there is a
//     WiFi device. The host name is set only if emm.txt or the saved settings
//     name one.
//  3. Render the Ethernet profile (when it is wanted, see "Ethernet profile")
//     and the WiFi profile (if configured); reload.
//  4. Wait for a network: up to 45 s for a configured WiFi network, otherwise
//     up to 15 s for a cable's DHCP lease (5 s if no cable is plugged in), or,
//     with nothing configured and a WiFi profile of somebody else's that
//     NetworkManager may bring up, up to 45 s for that. Ethernet being up
//     counts as online and skips the access point.
//  5. Otherwise enter setup mode: scan WiFi first (a single radio cannot scan
//     while serving an AP), then bring up the open (or WPA2) access point
//     EMM-Setup-XXXX on 10.42.0.1/24 with wildcard DNS, so phones open the
//     captive portal (see CaptiveMiddleware).
//
// # Existing connectivity
//
// When neither emm.txt nor the saved settings configure WiFi, EMM has nothing
// of its own to bring up, and whatever the box is already connected to is used
// as it is: a WiFi connection that somebody else made (Status.Adopted; Status.SSID
// and IP are NetworkManager's) or a cable. Nothing is touched and no access
// point is started; setup mode is entered only when there is no connectivity at
// all after the waits of step 4. The monitor loop keeps the status current but
// never starts setup mode because a connection dropped (NetworkManager
// reconnects; an access point would only get in its way). If EMM does have a
// WiFi network configured, step 4 waits for that one and the old rules hold.
// reset-network adopts nothing: it forces setup mode, whatever is online.
//
// Connect from the UI creates EMM's own emm-wifi profile (autoconnect priority
// 10, above the default of the profiles somebody else made, so it also wins
// after a reboot). A join that fails goes back to the adopted connection, and so
// does Forget of such a network. Forget with nothing saved does nothing.
//
// # Ethernet profile
//
// emm-eth has autoconnect priority 100, which beats every profile a user made,
// so it is written at boot only when it cannot take a user's setup away:
//
//   - Ethernet is explicitly configured: emm.txt has an `ethernet` key (whatever
//     its value), or the saved UI settings have a mode other than the plain
//     default "auto". Whatever else NetworkManager has for the cable, the
//     configured profile is written and wins.
//   - or NetworkManager has no Ethernet profile that is not EMM's own (one
//     `nmcli -t -f NAME,TYPE,AUTOCONNECT connection show`): the appliance image
//     has none, and there the default DHCP profile is what makes a cable work.
//
// Otherwise (a static address set with nmtui, NetworkManager's "Wired
// connection 1", on a box with default Ethernet settings) emm-eth is not
// written, and one that an earlier boot wrote is removed if it is the default
// profile exactly as EMM renders it. A profile that carries settings (static,
// DHCP-required, off) is never removed. If the profile list cannot be read,
// emm-eth is left as it is. Mode "off" works as before: the profile does not
// autoconnect and the device is disconnected. Ethernet changed from the UI at
// run time (SetEthernet) is always written.
//
// # Host name
//
// The machine is renamed only when emm.txt (hostname) or the saved settings
// (SetHostname) choose a name. Otherwise it keeps the name it has, and
// Status.Hostname is that name (lower-case first label of `hostname`, read
// through the Runner and refreshed by the monitor loop). It is never empty:
// DefaultHostname ("emm") stands in only when the machine's name cannot be
// determined at all, because the web layer builds its Host header allow-list
// from it. The appliance image sets /etc/hostname to emm itself.
//
// After a rename the 127.0.1.1 line of /etc/hosts (Options.HostsFile) is given
// the new name, or appended if there is none, so that sudo can still resolve the
// machine's own name. This is best effort (a failure is logged at debug level
// and never fails the rename), written atomically, and no other line is touched.
// Nothing is changed when no rename happens: a name that is already right, a
// rename that failed, or no name chosen at all.
//
// # WiFi device
//
// On the appliance image the WiFi driver is a module that udev loads, so the
// wlan device can appear seconds after emmd starts. The boot sequence therefore
// polls for it (one `nmcli device` per Timing.Poll) for up to
// Timing.WifiDeviceWait (30 s) before concluding that there is no adapter, but
// only while something needs it: Ethernet being online ends the wait at once.
// rfkill and the regulatory domain are handled when the device appears. A
// device that shows up even later is noticed by the monitor loop, which then
// does what the boot sequence would have done: join the configured WiFi, adopt
// what NetworkManager connected, or bring up the access point if nothing is
// online (or setup mode was forced with reset-network).
//
// In setup mode the monitor loop does three things. Every 30 s it checks
// whether Ethernet has come up and leaves setup mode if so. If the access point
// is down (a failed start, or a forced scan whose caller gave up) it starts it
// again, backing off from 5 s to a minute. And every 3 minutes
// (Timing.SetupRetry) it retries the configured WiFi, emm.txt's or the saved
// one: this is the way back for a box whose router booted slower than it did
// after a power cut. The retry happens only when `iw dev X station dump` shows
// no client on the access point, so a phone that is configuring the box is
// never dropped (a round with a client only restarts the clock); a failed retry
// puts the access point back and says why in Status.LastError. Nothing is
// retried when setup mode was forced with reset-network, which promises to
// ignore the configured WiFi for that boot, or when the configured WiFi cannot
// work as written.
//
// A box without a WiFi adapter has no access point and so no setup mode: it is
// "offline" (with ErrNoWifi as the reason) until a cable brings it up, and its
// web password stays in force.
//
// Connect in setup mode returns as soon as the credentials are validated and
// saved; the switch happens two seconds later in the background so the HTTP
// response can still reach the phone. If joining fails the saved WiFi
// credentials are rolled back (and only those), the access point returns and
// Status.LastError says why. A second Connect while one is pending is refused
// with ErrBusy.
//
// A network that is not in the scan cache when it is chosen (the UI's "Other
// network...") is saved as hidden (Saved.WifiHidden) and its profile carries
// hidden=true, so that NetworkManager can autoconnect to it after a reboot. An
// emm.txt network is judged by the scan cache when its profile is rendered.
//
// Every external command runs with LC_ALL=C and LANG=C (see ExecRunner), so the
// English words the package matches do not depend on the system locale.
//
// # Testing and what is NOT verified
//
// Everything is tested against a fake Runner (see fake.go), which simulates
// NetworkManager closely enough to drive the whole state machine, including
// failures and timeouts under a fake clock. NewFake exposes that simulation so
// the UI can be developed on a laptop.
//
// UNVERIFIED on real hardware (needs a Raspberry Pi with NetworkManager):
//
//   - the exact `nmcli -t` output formats this package parses (device list,
//     wifi list, connection list with NAME,TYPE,AUTOCONNECT and the TYPE words
//     "wifi" or "802-11-wireless" and "ethernet" or "802-3-ethernet",
//     `-g IP4.ADDRESS`, `-g GENERAL.HWADDR`,
//     `-g 802-11-wireless.ssid connection show id NAME`): written from the
//     documentation, not from a live device;
//   - how soon after boot NetworkManager joins a profile that Imager made, and
//     that 45 s is enough (the wait while one exists is Timing.WifiWait);
//   - when the wlan device appears on the image, and that the rfkill and
//     regulatory-domain commands work from that moment on;
//   - that NetworkManager accepts the generated keyfiles as written (SSID byte
//     list form, `hidden=true`, `powersave=2`, `autoconnect-retries=0`, `band=bg`
//     on the AP), and that `hidden=true` is what lets autoconnect find a
//     non-broadcasting network (from reading nm-keyfile.c and nm-device-wifi.c,
//     not from a device);
//   - the `iw dev X station dump` output the retry counts ("Station <mac> (on
//     <dev>)" lines);
//   - that NetworkManager's `shared` mode starts dnsmasq with our snippet in
//     /etc/NetworkManager/dnsmasq-shared.d, that `address=/#/10.42.0.1` answers
//     every name, and that shared mode works without an upstream connection
//     and without iptables/nftables installed;
//   - that phones behave as hoped (open the portal, survive the AP going down),
//     in particular the Android and iOS probe handling;
//   - that disabling Ethernet through an `autoconnect=false` profile plus
//     `nmcli device disconnect` stops NetworkManager from creating its own
//     default wired connection;
//   - that `nmcli device wifi list --rescan yes` right after an AP has been
//     taken down returns fresh results.
package netcfg
