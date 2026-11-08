package blemidi

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type emitted struct {
	path   dbus.ObjectPath
	name   string
	values []interface{}
}

type fakeBus struct {
	mu    sync.Mutex
	calls []emitted
}

func (f *fakeBus) Emit(path dbus.ObjectPath, name string, values ...interface{}) error {
	f.mu.Lock()
	f.calls = append(f.calls, emitted{path, name, values})
	f.mu.Unlock()
	return nil
}

func (f *fakeBus) all() []emitted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]emitted(nil), f.calls...)
}

// valuePackets returns the payload of every Value PropertiesChanged emitted.
func (f *fakeBus) valuePackets() [][]byte {
	var out [][]byte
	for _, c := range f.all() {
		if len(c.values) < 2 {
			continue
		}
		if m, ok := c.values[1].(map[string]dbus.Variant); ok {
			if v, ok := m["Value"]; ok {
				out = append(out, v.Value().([]byte))
			}
		}
	}
	return out
}

func newTestTree() (*gattTree, *fakeBus) {
	t := newGattTree(discardLog(), "EMM", false, nil)
	fb := &fakeBus{}
	t.bus = fb
	return t, fb
}

func TestUUIDsMatchTheSpec(t *testing.T) {
	if ServiceUUID != "03b80e5a-ede8-4b33-a751-6ce34ec4c700" {
		t.Error("service UUID")
	}
	if CharacteristicUUID != "7772e5db-3868-4112-a1a9-f2669d106bf3" {
		t.Error("characteristic UUID")
	}
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !re.MatchString(ServiceUUID) || !re.MatchString(CharacteristicUUID) {
		t.Error("UUIDs must be lower-case 128-bit strings")
	}
}

func TestManagedObjectsShape(t *testing.T) {
	tree, _ := newTestTree()
	objs, derr := tree.GetManagedObjects()
	if derr != nil {
		t.Fatal(derr)
	}
	if got := dbus.SignatureOf(objs).String(); got != "a{oa{sa{sv}}}" {
		t.Fatalf("GetManagedObjects signature %q", got)
	}
	if len(objs) != 2 {
		t.Fatalf("want service+characteristic, got %d objects", len(objs))
	}
	svc := objs[svcPath][ifaceSvc]
	if svc["UUID"].Value() != ServiceUUID || svc["Primary"].Value() != true {
		t.Fatalf("service props %v", svc)
	}
	ch := objs[chrcPath][ifaceChrc]
	want := map[string]string{"UUID": "s", "Service": "o", "Flags": "as", "Notifying": "b", "Value": "ay"}
	for name, sig := range want {
		v, ok := ch[name]
		if !ok {
			t.Fatalf("characteristic misses %s", name)
		}
		if v.Signature().String() != sig {
			t.Errorf("%s has signature %q, want %q", name, v.Signature(), sig)
		}
	}
	if ch["UUID"].Value() != CharacteristicUUID || ch["Service"].Value() != svcPath {
		t.Fatalf("characteristic props %v", ch)
	}
	// Nothing outside the GATT hierarchy may be listed.
	for p := range objs {
		if !strings.HasPrefix(string(p), string(appPath)+"/") {
			t.Errorf("object %s is outside the application root", p)
		}
	}
}

func TestCharacteristicFlags(t *testing.T) {
	has := func(f []string, s string) bool {
		for _, x := range f {
			if x == s {
				return true
			}
		}
		return false
	}
	base := chrcFlags(false)
	for _, f := range []string{"read", "write-without-response", "notify"} {
		if !has(base, f) {
			t.Errorf("flags %v miss %s", base, f)
		}
	}
	if has(base, "encrypt-read") {
		t.Error("unencrypted flags contain encrypt-*")
	}
	enc := chrcFlags(true)
	for _, f := range []string{"encrypt-read", "encrypt-write", "encrypt-notify", "notify"} {
		if !has(enc, f) {
			t.Errorf("flags %v miss %s", enc, f)
		}
	}
}

func TestPropertiesInterface(t *testing.T) {
	tree, _ := newTestTree()
	v, err := tree.chrc.Get(ifaceChrc, "UUID")
	if err != nil || v.Value() != CharacteristicUUID {
		t.Fatalf("Get: %v %v", v, err)
	}
	if _, err := tree.chrc.Get(ifaceChrc, "Nope"); err == nil || err.Name != "org.freedesktop.DBus.Error.UnknownProperty" {
		t.Fatalf("unknown property: %v", err)
	}
	if _, err := tree.chrc.Get("org.bogus", "UUID"); err == nil || err.Name != "org.freedesktop.DBus.Error.UnknownInterface" {
		t.Fatalf("unknown interface: %v", err)
	}
	all, err := tree.chrc.GetAll(ifaceChrc)
	if err != nil || len(all) != 5 {
		t.Fatalf("GetAll: %v %v", all, err)
	}
	if err := tree.chrc.Set(ifaceChrc, "UUID", dbus.MakeVariant("x")); err == nil {
		t.Fatal("Set must be refused")
	}
	// GetAll returns a copy.
	delete(all, "UUID")
	if _, err := tree.chrc.Get(ifaceChrc, "UUID"); err != nil {
		t.Fatal("GetAll leaked internal map")
	}
}

func TestAdvertisementProperties(t *testing.T) {
	tree, _ := newTestTree()
	all, _ := tree.adv.GetAll(ifaceAdv)
	want := map[string]string{"Type": "s", "ServiceUUIDs": "as", "LocalName": "s", "Discoverable": "b"}
	for name, sig := range want {
		if v, ok := all[name]; !ok || v.Signature().String() != sig {
			t.Errorf("%s: %v (want signature %s)", name, v, sig)
		}
	}
	if all["Type"].Value() != "peripheral" {
		t.Error("advertisement must be of type peripheral")
	}
	if u := all["ServiceUUIDs"].Value().([]string); len(u) != 1 || u[0] != ServiceUUID {
		t.Errorf("advertised UUIDs %v", u)
	}
	tree.setName("Mainframe")
	if v, _ := tree.adv.Get(ifaceAdv, "LocalName"); v.Value() != "Mainframe" {
		t.Error("setName did not update LocalName")
	}
}

func TestIntrospectionXMLIsConsistentWithExports(t *testing.T) {
	tree, _ := newTestTree()
	ag := &agent{log: discardLog()}
	nodes := tree.nodes()

	// Every node renders to well-formed XML.
	parsed := map[dbus.ObjectPath]introspect.Node{}
	for path, n := range nodes {
		doc := string(introspect.NewIntrospectable(n))
		i := strings.Index(doc, "<node")
		if i < 0 {
			t.Fatalf("%s: no <node> in %q", path, doc)
		}
		var out introspect.Node
		if err := xml.Unmarshal([]byte(doc[i:]), &out); err != nil {
			t.Fatalf("%s: bad XML: %v", path, err)
		}
		parsed[path] = out
	}

	find := func(path dbus.ObjectPath, iface string) *introspect.Interface {
		n, ok := parsed[path]
		if !ok {
			return nil
		}
		for i := range n.Interfaces {
			if n.Interfaces[i].Name == iface {
				return &n.Interfaces[i]
			}
		}
		return nil
	}

	// Every exported method is declared in the introspection data, and is a func
	// whose last result (if any) is *dbus.Error.
	for _, e := range tree.exports(ag) {
		in := find(e.path, e.iface)
		if in == nil {
			t.Errorf("%s %s is exported but not introspectable", e.path, e.iface)
			continue
		}
		declared := map[string]introspect.Method{}
		for _, m := range in.Methods {
			declared[m.Name] = m
		}
		for name, fn := range e.methods {
			m, ok := declared[name]
			if !ok {
				t.Errorf("%s %s.%s exported but not declared", e.path, e.iface, name)
				continue
			}
			ft := reflect.TypeOf(fn)
			if ft.Kind() != reflect.Func {
				t.Errorf("%s.%s is not a func", e.iface, name)
				continue
			}
			errT := reflect.TypeOf((*dbus.Error)(nil))
			if ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != errT {
				t.Errorf("%s.%s must return *dbus.Error last", e.iface, name)
			}
			// Declared in-args match the Go parameter count; declared out-args
			// match the results (minus the error).
			var ins, outs int
			for _, a := range m.Args {
				if a.Direction == "out" {
					outs++
				} else {
					ins++
				}
			}
			if ins != ft.NumIn() || outs != ft.NumOut()-1 {
				t.Errorf("%s.%s: introspection says %d in/%d out, Go says %d in/%d out",
					e.iface, name, ins, outs, ft.NumIn(), ft.NumOut()-1)
			}
		}
		for name := range declared {
			if _, ok := e.methods[name]; !ok && e.iface != ifaceProps {
				t.Errorf("%s.%s declared but not exported", e.iface, name)
			}
		}
	}

	// Declared properties exist and have the declared D-Bus type.
	check := func(path dbus.ObjectPath, iface string, o *propObject) {
		in := find(path, iface)
		if in == nil {
			t.Fatalf("%s %s missing from introspection", path, iface)
		}
		for _, p := range in.Properties {
			v, derr := o.Get(iface, p.Name)
			if derr != nil {
				t.Errorf("%s.%s declared but absent: %v", iface, p.Name, derr)
				continue
			}
			if v.Signature().String() != p.Type {
				t.Errorf("%s.%s declared %q but is %q", iface, p.Name, p.Type, v.Signature())
			}
		}
	}
	check(svcPath, ifaceSvc, tree.service)
	check(chrcPath, ifaceChrc, tree.chrc)
	check(advPath, ifaceAdv, tree.adv)

	// The Agent1 interface declares every method BlueZ may call.
	if in := find(agentPath, ifaceAgent); in == nil || len(in.Methods) != 9 {
		t.Fatalf("agent introspection: %+v", in)
	}
}

func TestExportPathsAreWhereBluezExpectsThem(t *testing.T) {
	if !strings.HasPrefix(string(svcPath), string(appPath)+"/") || !strings.HasPrefix(string(chrcPath), string(svcPath)+"/") {
		t.Fatal("GATT objects must be nested under the application root")
	}
	for _, p := range []dbus.ObjectPath{advPath, agentPath} {
		if strings.HasPrefix(string(p), string(appPath)) {
			t.Errorf("%s must not be inside the GATT application tree", p)
		}
		if !p.IsValid() {
			t.Errorf("%s is not a valid object path", p)
		}
	}
	for _, p := range []dbus.ObjectPath{appPath, svcPath, chrcPath} {
		if !p.IsValid() {
			t.Errorf("%s is not a valid object path", p)
		}
	}
}

func TestReadValueIsEmpty(t *testing.T) {
	tree, _ := newTestTree()
	b, err := tree.ReadValue(map[string]dbus.Variant{})
	if err != nil || b == nil || len(b) != 0 {
		t.Fatalf("ReadValue = %v, %v; the spec wants a read with no payload", b, err)
	}
}

func writeCall(path dbus.ObjectPath, iface, member string, body ...interface{}) *dbus.Message {
	return &dbus.Message{
		Type: dbus.TypeMethodCall,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:      dbus.MakeVariant(path),
			dbus.FieldInterface: dbus.MakeVariant(iface),
			dbus.FieldMember:    dbus.MakeVariant(member),
		},
		Body: body,
	}
}

func TestInterceptQueuesWritesInOrder(t *testing.T) {
	tree, _ := newTestTree()
	opts := map[string]dbus.Variant{}
	for i := 0; i < 100; i++ {
		tree.intercept(writeCall(chrcPath, ifaceChrc, "WriteValue", []byte{0x80, byte(i)}, opts))
	}
	for i := 0; i < 100; i++ {
		p, ok := tree.inbound.pop(ctxT(t))
		if !ok || !reflect.DeepEqual(p, []byte{0x80, byte(i)}) {
			t.Fatalf("packet %d: %x %v", i, p, ok)
		}
	}
}

func TestInterceptIgnoresEverythingElse(t *testing.T) {
	tree, _ := newTestTree()
	opts := map[string]dbus.Variant{}
	for _, m := range []*dbus.Message{
		nil,
		writeCall(chrcPath, ifaceChrc, "ReadValue", opts),
		writeCall(svcPath, ifaceChrc, "WriteValue", []byte{1}, opts),
		writeCall(chrcPath, "org.other.Iface", "WriteValue", []byte{1}, opts),
		writeCall(chrcPath, ifaceChrc, "WriteValue", []byte{}, opts), // empty payload
		writeCall(chrcPath, ifaceChrc, "WriteValue", "not bytes", opts),
		writeCall(chrcPath, ifaceChrc, "WriteValue"), // no body
		{Type: dbus.TypeSignal, Headers: map[dbus.HeaderField]dbus.Variant{}, Body: []interface{}{[]byte{1}}},
	} {
		tree.intercept(m)
	}
	if n := tree.inbound.len(); n != 0 {
		t.Fatalf("%d unexpected packets queued", n)
	}
}

func TestInterceptCopiesPayload(t *testing.T) {
	tree, _ := newTestTree()
	buf := []byte{0x80, 0x80, 0xF8}
	tree.intercept(writeCall(chrcPath, ifaceChrc, "WriteValue", buf, map[string]dbus.Variant{}))
	buf[2] = 0xFF // godbus may reuse its buffers
	p, _ := tree.inbound.pop(ctxT(t))
	if p[2] != 0xF8 {
		t.Fatal("payload aliases the message buffer")
	}
}

func TestNotifyOnlyWhenSubscribed(t *testing.T) {
	tree, fb := newTestTree()
	tree.notify([]byte{0x80, 0x80, 0xF8})
	if len(fb.all()) != 0 {
		t.Fatal("notified without a subscriber")
	}

	if err := tree.StartNotify(); err != nil {
		t.Fatal(err)
	}
	if !tree.isNotifying() {
		t.Fatal("not notifying after StartNotify")
	}
	pkt := []byte{0x80, 0x80, 0xF8}
	tree.notify(pkt)
	pkt[2] = 0 // the emitted value must be a copy
	got := fb.valuePackets()
	if len(got) != 1 || !reflect.DeepEqual(got[0], []byte{0x80, 0x80, 0xF8}) {
		t.Fatalf("value packets %x", got)
	}
	// Shape of the signal BlueZ listens for.
	var found bool
	for _, c := range fb.all() {
		if c.path == chrcPath && c.name == "org.freedesktop.DBus.Properties.PropertiesChanged" &&
			dbus.SignatureOf(c.values...).String() == "sa{sv}as" && c.values[0] == ifaceChrc {
			if _, ok := c.values[1].(map[string]dbus.Variant)["Value"]; ok {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no well-formed PropertiesChanged(Value) among %+v", fb.all())
	}

	if err := tree.StopNotify(); err != nil {
		t.Fatal(err)
	}
	n := len(fb.valuePackets())
	tree.notify([]byte{0x80, 0x80, 0xFA})
	if len(fb.valuePackets()) != n {
		t.Fatal("notified after StopNotify")
	}
	if v, _ := tree.chrc.Get(ifaceChrc, "Notifying"); v.Value() != false {
		t.Fatal("Notifying property not cleared")
	}
}

func TestMTUTrackingAndEffectiveMTU(t *testing.T) {
	changes := 0
	tree := newGattTree(discardLog(), "EMM", false, func() { changes++ })
	devA, devB := dbus.ObjectPath("/org/bluez/hci0/dev_A"), dbus.ObjectPath("/org/bluez/hci0/dev_B")
	opts := func(dev dbus.ObjectPath, mtu uint16) map[string]dbus.Variant {
		m := map[string]dbus.Variant{"device": dbus.MakeVariant(dev)}
		if mtu != 0 {
			m["mtu"] = dbus.MakeVariant(mtu)
		}
		return m
	}
	if tree.effectiveMTU() != 23 || tree.connectedCount() != 0 {
		t.Fatal("defaults")
	}
	tree.ReadValue(opts(devA, 185))
	if tree.effectiveMTU() != 185 || tree.connectedCount() != 1 {
		t.Fatalf("one central: mtu %d count %d", tree.effectiveMTU(), tree.connectedCount())
	}
	tree.WriteValue([]byte{1}, opts(devB, 0)) // second central, MTU unknown
	if tree.effectiveMTU() != 23 || tree.connectedCount() != 2 {
		t.Fatalf("unknown MTU must fall back to 23: %d", tree.effectiveMTU())
	}
	tree.WriteValue([]byte{1}, opts(devB, 100))
	if tree.effectiveMTU() != 100 {
		t.Fatalf("minimum of 185 and 100: %d", tree.effectiveMTU())
	}
	tree.deviceConnected(devB, false)
	if tree.effectiveMTU() != 185 || tree.connectedCount() != 1 {
		t.Fatalf("after B left: mtu %d count %d", tree.effectiveMTU(), tree.connectedCount())
	}
	tree.deviceConnected(dbus.ObjectPath("/org/bluez/hci0/dev_other"), false) // not ours: ignored
	tree.deviceConnected(devA, true)                                          // connect events alone add nothing
	if tree.connectedCount() != 1 {
		t.Fatal("count changed")
	}
	if changes == 0 {
		t.Fatal("onChange never called")
	}
	tree.resetDevices()
	if tree.connectedCount() != 0 || tree.effectiveMTU() != 23 {
		t.Fatal("resetDevices")
	}
}

func TestOptionDecoding(t *testing.T) {
	for _, v := range []interface{}{uint16(185), uint32(185), int32(185), int64(185), uint64(185)} {
		if m, ok := optMTU(map[string]dbus.Variant{"mtu": dbus.MakeVariant(v)}); !ok || m != 185 {
			t.Errorf("optMTU(%T) = %d,%v", v, m, ok)
		}
	}
	if _, ok := optMTU(map[string]dbus.Variant{"mtu": dbus.MakeVariant("x")}); ok {
		t.Error("string accepted as MTU")
	}
	if d, ok := optDevice(map[string]dbus.Variant{"device": dbus.MakeVariant(dbus.ObjectPath("/a/b"))}); !ok || d != "/a/b" {
		t.Error("optDevice(ObjectPath)")
	}
	if d, ok := optDevice(map[string]dbus.Variant{"device": dbus.MakeVariant("/a/b")}); !ok || d != "/a/b" {
		t.Error("optDevice(string)")
	}
	if _, ok := optDevice(nil); ok {
		t.Error("optDevice(nil)")
	}
}

func TestHandleSignal(t *testing.T) {
	tree, _ := newTestTree()
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA")
	tree.noteDevice(map[string]dbus.Variant{"device": dbus.MakeVariant(dev)})
	if tree.connectedCount() != 1 {
		t.Fatal("setup")
	}
	// Unrelated interface: ignored.
	_ = tree.handleSignal(&dbus.Signal{Path: dev, Name: ifaceProps + ".PropertiesChanged",
		Body: []interface{}{"org.bluez.Battery1", map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)}, []string{}}})
	if tree.connectedCount() != 1 {
		t.Fatal("reacted to the wrong interface")
	}
	// Device1.Connected = false removes the central.
	if err := tree.handleSignal(&dbus.Signal{Path: dev, Name: ifaceProps + ".PropertiesChanged",
		Body: []interface{}{ifaceDevice, map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)}, []string{}}}); err != nil {
		t.Fatal(err)
	}
	if tree.connectedCount() != 0 {
		t.Fatal("central still counted after disconnect")
	}
	// Malformed bodies never panic.
	_ = tree.handleSignal(&dbus.Signal{Name: ifaceProps + ".PropertiesChanged"})
	_ = tree.handleSignal(&dbus.Signal{Name: ifaceProps + ".PropertiesChanged", Body: []interface{}{1, 2}})
	_ = tree.handleSignal(&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []interface{}{"x"}})

	// bluetoothd restart ends the session; other names do not.
	if err := tree.handleSignal(&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged",
		Body: []interface{}{"org.bluez", ":1.5", ""}}); err == nil {
		t.Fatal("bluetoothd going away must end the session")
	}
	if err := tree.handleSignal(&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged",
		Body: []interface{}{"org.other", ":1.5", ""}}); err != nil {
		t.Fatal(err)
	}
}

func TestAdvertisementReleaseSignalsSession(t *testing.T) {
	tree, _ := newTestTree()
	tree.Release()
	tree.Release() // a second release must not block
	select {
	case <-tree.advReleased:
	default:
		t.Fatal("Release did not signal")
	}
}

func TestAgentAcceptsEverything(t *testing.T) {
	a := &agent{log: discardLog()}
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA")
	if err := a.Release(); err != nil {
		t.Error(err)
	}
	if pin, err := a.RequestPinCode(dev); err != nil || pin == "" {
		t.Error("RequestPinCode", pin, err)
	}
	if err := a.DisplayPinCode(dev, "1234"); err != nil {
		t.Error(err)
	}
	if _, err := a.RequestPasskey(dev); err != nil {
		t.Error(err)
	}
	if err := a.DisplayPasskey(dev, 1, 2); err != nil {
		t.Error(err)
	}
	if err := a.RequestConfirmation(dev, 123456); err != nil {
		t.Error(err)
	}
	if err := a.RequestAuthorization(dev); err != nil {
		t.Error(err)
	}
	if err := a.AuthorizeService(dev, ServiceUUID); err != nil {
		t.Error(err)
	}
	if err := a.Cancel(); err != nil {
		t.Error(err)
	}
}

func TestOptionsDefaultsAndStatusJSON(t *testing.T) {
	o := Options{}.withDefaults()
	if o.Name != "EMM" || o.Adapter != "hci0" || o.Logger == nil || o.PacketInterval <= 0 {
		t.Fatalf("defaults %+v", o)
	}
	if PortID != "ble:0" {
		t.Fatal("port id")
	}
}
