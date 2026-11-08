package blemidi

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// This file is the D-Bus object tree that BlueZ talks to. It is kept free of
// any live connection so that the shapes (property types, GetManagedObjects,
// introspection, method signatures) can be unit-tested without BlueZ.

// Object paths. The GATT objects live under the application root, as BlueZ
// requires; the advertisement and agent are siblings, outside it.
const (
	appPath   dbus.ObjectPath = "/org/emm/ble/app"
	svcPath   dbus.ObjectPath = appPath + "/service0"
	chrcPath  dbus.ObjectPath = svcPath + "/char0"
	advPath   dbus.ObjectPath = "/org/emm/ble/adv0"
	agentPath dbus.ObjectPath = "/org/emm/ble/agent"
)

// D-Bus names.
const (
	bluezName = "org.bluez"

	ifaceProps    = "org.freedesktop.DBus.Properties"
	ifaceObjMgr   = "org.freedesktop.DBus.ObjectManager"
	ifaceIntro    = "org.freedesktop.DBus.Introspectable"
	ifaceGattMgr  = "org.bluez.GattManager1"
	ifaceAdvMgr   = "org.bluez.LEAdvertisingManager1"
	ifaceAgentMgr = "org.bluez.AgentManager1"
	ifaceAdapter  = "org.bluez.Adapter1"
	ifaceDevice   = "org.bluez.Device1"
	ifaceSvc      = "org.bluez.GattService1"
	ifaceChrc     = "org.bluez.GattCharacteristic1"
	ifaceAdv      = "org.bluez.LEAdvertisement1"
	ifaceAgent    = "org.bluez.Agent1"
)

const errFailed = "org.bluez.Error.Failed"

func dbusErr(name, msg string) *dbus.Error {
	return &dbus.Error{Name: name, Body: []interface{}{msg}}
}

// ---- properties ------------------------------------------------------------

// propObject implements org.freedesktop.DBus.Properties over fixed interfaces.
type propObject struct {
	mu    sync.RWMutex
	props map[string]map[string]dbus.Variant
}

func newPropObject(p map[string]map[string]interface{}) *propObject {
	o := &propObject{props: map[string]map[string]dbus.Variant{}}
	for iface, m := range p {
		o.props[iface] = map[string]dbus.Variant{}
		for k, v := range m {
			o.props[iface][k] = dbus.MakeVariant(v)
		}
	}
	return o
}

func (o *propObject) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	m, ok := o.props[iface]
	if !ok {
		return dbus.Variant{}, dbusErr("org.freedesktop.DBus.Error.UnknownInterface", "no such interface "+iface)
	}
	v, ok := m[name]
	if !ok {
		return dbus.Variant{}, dbusErr("org.freedesktop.DBus.Error.UnknownProperty", "no such property "+name)
	}
	return v, nil
}

func (o *propObject) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	m, ok := o.props[iface]
	if !ok {
		return nil, dbusErr("org.freedesktop.DBus.Error.UnknownInterface", "no such interface "+iface)
	}
	out := make(map[string]dbus.Variant, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

// Set is refused: every property here is read-only.
func (o *propObject) Set(iface, name string, _ dbus.Variant) *dbus.Error {
	return dbusErr("org.freedesktop.DBus.Error.PropertyReadOnly", "property "+name+" is read-only")
}

// snapshot copies all property maps (for GetManagedObjects).
func (o *propObject) snapshot() map[string]map[string]dbus.Variant {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]map[string]dbus.Variant, len(o.props))
	for iface, m := range o.props {
		c := make(map[string]dbus.Variant, len(m))
		for k, v := range m {
			c[k] = v
		}
		out[iface] = c
	}
	return out
}

func (o *propObject) update(iface, name string, v interface{}) {
	o.mu.Lock()
	o.props[iface][name] = dbus.MakeVariant(v)
	o.mu.Unlock()
}

// ---- the tree --------------------------------------------------------------

// bus is the part of *dbus.Conn the tree needs at run time.
type bus interface {
	Emit(path dbus.ObjectPath, name string, values ...interface{}) error
}

type devState struct {
	mtu uint16 // 0 = unknown
}

// gattTree holds the MIDI service, its characteristic, the advertisement and
// the pairing agent, plus the little state they share.
type gattTree struct {
	log *slog.Logger
	bus bus // set once connected

	service *propObject
	chrc    *propObject
	adv     *propObject

	inbound     *pktQueue     // BLE-MIDI packets written by centrals, in arrival order
	advReleased chan struct{} // BlueZ dropped our advertisement (cap 1)
	onChange    func()

	mu        sync.Mutex
	notifying bool
	devices   map[dbus.ObjectPath]*devState
}

func chrcFlags(encrypt bool) []string {
	f := []string{"read", "write-without-response", "write", "notify"}
	if encrypt {
		f = append(f, "encrypt-read", "encrypt-write", "encrypt-notify")
	}
	return f
}

func newGattTree(log *slog.Logger, name string, encrypt bool, onChange func()) *gattTree {
	if onChange == nil {
		onChange = func() {}
	}
	t := &gattTree{
		log:         log,
		inbound:     newPktQueue(8192),
		advReleased: make(chan struct{}, 1),
		onChange:    onChange,
		devices:     map[dbus.ObjectPath]*devState{},
	}
	t.service = newPropObject(map[string]map[string]interface{}{
		ifaceSvc: {
			"UUID":    ServiceUUID,
			"Primary": true,
		},
	})
	t.chrc = newPropObject(map[string]map[string]interface{}{
		ifaceChrc: {
			"UUID":      CharacteristicUUID,
			"Service":   svcPath,
			"Flags":     chrcFlags(encrypt),
			"Notifying": false,
			"Value":     []byte{},
		},
	})
	t.adv = newPropObject(map[string]map[string]interface{}{
		ifaceAdv: advProps(name),
	})
	return t
}

func advProps(name string) map[string]interface{} {
	return map[string]interface{}{
		"Type":         "peripheral",
		"ServiceUUIDs": []string{ServiceUUID},
		"LocalName":    name,
		"Discoverable": true,
	}
}

// setName updates the advertised name (the caller re-registers the advertisement).
func (t *gattTree) setName(name string) {
	t.adv.update(ifaceAdv, "LocalName", name)
}

// ---- GetManagedObjects -----------------------------------------------------

// GetManagedObjects implements org.freedesktop.DBus.ObjectManager on the
// application root: the GATT service and its characteristic.
func (t *gattTree) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		svcPath:  t.service.snapshot(),
		chrcPath: t.chrc.snapshot(),
	}, nil
}

// ---- GattCharacteristic1 ---------------------------------------------------

// ReadValue answers the read a BLE-MIDI central does on connect: no payload.
func (t *gattTree) ReadValue(opts map[string]dbus.Variant) ([]byte, *dbus.Error) {
	t.noteDevice(opts)
	return []byte{}, nil
}

// WriteValue acknowledges a write. The payload itself is captured in order by
// the connection's incoming-message interceptor (see intercept), because
// godbus runs method handlers in separate goroutines and would reorder
// packets; this handler only learns who is talking and the link's MTU.
func (t *gattTree) WriteValue(value []byte, opts map[string]dbus.Variant) *dbus.Error {
	t.noteDevice(opts)
	return nil
}

// StartNotify is called by BlueZ when the first central enables notifications.
func (t *gattTree) StartNotify() *dbus.Error {
	t.mu.Lock()
	t.notifying = true
	t.mu.Unlock()
	t.chrc.update(ifaceChrc, "Notifying", true)
	t.emitChanged(ifaceChrc, map[string]dbus.Variant{"Notifying": dbus.MakeVariant(true)})
	t.onChange()
	return nil
}

// StopNotify is called when the last central stops notifications or disconnects.
func (t *gattTree) StopNotify() *dbus.Error {
	t.mu.Lock()
	t.notifying = false
	t.mu.Unlock()
	t.chrc.update(ifaceChrc, "Notifying", false)
	t.emitChanged(ifaceChrc, map[string]dbus.Variant{"Notifying": dbus.MakeVariant(false)})
	t.onChange()
	return nil
}

func (t *gattTree) isNotifying() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.notifying
}

// notify sends one BLE-MIDI packet to every subscribed central by updating
// the characteristic's Value property; BlueZ turns that into ATT
// notifications. It does nothing when nobody is subscribed.
func (t *gattTree) notify(pkt []byte) {
	if !t.isNotifying() {
		return
	}
	v := append([]byte(nil), pkt...)
	t.chrc.update(ifaceChrc, "Value", v)
	t.emitChanged(ifaceChrc, map[string]dbus.Variant{"Value": dbus.MakeVariant(v)})
}

func (t *gattTree) emitChanged(iface string, changed map[string]dbus.Variant) {
	if t.bus == nil {
		return
	}
	if err := t.bus.Emit(chrcPath, ifaceProps+".PropertiesChanged", iface, changed, []string{}); err != nil {
		t.log.Debug("emit PropertiesChanged failed", "err", err)
	}
}

// intercept is the connection's incoming-message interceptor. It runs on the
// connection's reader goroutine, strictly in arrival order, so WriteValue
// payloads are queued in the order the central sent them.
func (t *gattTree) intercept(msg *dbus.Message) {
	if msg == nil || msg.Type != dbus.TypeMethodCall || len(msg.Body) < 1 {
		return
	}
	if m, _ := msg.Headers[dbus.FieldMember].Value().(string); m != "WriteValue" {
		return
	}
	if p, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath); p != chrcPath {
		return
	}
	if i, _ := msg.Headers[dbus.FieldInterface].Value().(string); i != ifaceChrc {
		return
	}
	b, ok := msg.Body[0].([]byte)
	if !ok || len(b) == 0 {
		return
	}
	if d := t.inbound.push(append([]byte(nil), b...), false, 0); d > 0 {
		t.log.Warn("inbound BLE-MIDI queue overflow; dropped oldest packet(s)", "dropped", d)
	}
}

// ---- devices and MTU --------------------------------------------------------

func optDevice(opts map[string]dbus.Variant) (dbus.ObjectPath, bool) {
	if v, ok := opts["device"]; ok {
		switch d := v.Value().(type) {
		case dbus.ObjectPath:
			return d, true
		case string:
			return dbus.ObjectPath(d), true
		}
	}
	return "", false
}

func optMTU(opts map[string]dbus.Variant) (uint16, bool) {
	v, ok := opts["mtu"]
	if !ok {
		return 0, false
	}
	switch m := v.Value().(type) {
	case uint16:
		return m, true
	case uint32:
		return uint16(m), true
	case int32:
		return uint16(m), true
	case int64:
		return uint16(m), true
	case uint64:
		return uint16(m), true
	}
	return 0, false
}

// noteDevice records the central (and its MTU) behind a Read/Write call.
func (t *gattTree) noteDevice(opts map[string]dbus.Variant) {
	dev, ok := optDevice(opts)
	if !ok {
		return
	}
	mtu, _ := optMTU(opts)
	t.mu.Lock()
	d := t.devices[dev]
	isNew := d == nil
	if isNew {
		d = &devState{}
		t.devices[dev] = d
	}
	changed := isNew
	if mtu != 0 && d.mtu != mtu {
		d.mtu = mtu
		changed = true
	}
	t.mu.Unlock()
	if changed {
		t.onChange()
	}
}

// deviceConnected applies a Device1.Connected change seen on the bus.
func (t *gattTree) deviceConnected(dev dbus.ObjectPath, connected bool) {
	if connected {
		return // devices are tracked once they use our service
	}
	t.mu.Lock()
	_, known := t.devices[dev]
	delete(t.devices, dev)
	t.mu.Unlock()
	if known {
		t.onChange()
	}
}

func (t *gattTree) connectedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.devices)
}

// effectiveMTU is the smallest MTU among the centrals in use (23 when a
// central's MTU is unknown, or when there are none): notifications go to all
// of them, so they must fit the weakest link.
func (t *gattTree) effectiveMTU() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	mtu := 0
	for _, d := range t.devices {
		m := int(d.mtu)
		if m == 0 {
			m = DefaultMTU
		}
		if mtu == 0 || m < mtu {
			mtu = m
		}
	}
	if mtu == 0 {
		return DefaultMTU
	}
	return mtu
}

func (t *gattTree) resetDevices() {
	t.mu.Lock()
	t.devices = map[dbus.ObjectPath]*devState{}
	t.notifying = false
	t.mu.Unlock()
}

// handleSignal reacts to the bus signals the session subscribes to: a central
// disconnecting (Device1.Connected -> false) and bluetoothd going away. A
// non-nil error means the session is no longer valid and must be restarted.
func (t *gattTree) handleSignal(sig *dbus.Signal) error {
	switch sig.Name {
	case ifaceProps + ".PropertiesChanged":
		if len(sig.Body) < 2 {
			return nil
		}
		if iface, _ := sig.Body[0].(string); iface != ifaceDevice {
			return nil
		}
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		if v, ok := changed["Connected"]; ok {
			if c, ok := v.Value().(bool); ok {
				t.deviceConnected(sig.Path, c)
			}
		}
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(sig.Body) >= 3 {
			if n, _ := sig.Body[0].(string); n == bluezName {
				return errors.New("bluetoothd restarted or went away")
			}
		}
	}
	return nil
}

// ---- LEAdvertisement1 -------------------------------------------------------

// Release is called by BlueZ when it drops the advertisement (timeout,
// adapter reset, ...). The session notices and registers it again.
func (t *gattTree) Release() *dbus.Error {
	select {
	case t.advReleased <- struct{}{}:
	default:
	}
	return nil
}

// ---- Agent1 (Just Works pairing) ---------------------------------------------

// The agent accepts everything: with capability NoInputNoOutput the device has
// no screen or keyboard, and pairing is "Just Works" (encrypted, but not
// authenticated against a man in the middle).
type agent struct{ log *slog.Logger }

func (a *agent) Release() *dbus.Error { return nil }
func (a *agent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	return "0000", nil
}
func (a *agent) DisplayPinCode(device dbus.ObjectPath, pincode string) *dbus.Error { return nil }
func (a *agent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error)       { return 0, nil }
func (a *agent) DisplayPasskey(device dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	return nil
}
func (a *agent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) *dbus.Error {
	a.log.Debug("pairing accepted", "device", device)
	return nil
}
func (a *agent) RequestAuthorization(device dbus.ObjectPath) *dbus.Error {
	a.log.Debug("authorization accepted", "device", device)
	return nil
}
func (a *agent) AuthorizeService(device dbus.ObjectPath, uuid string) *dbus.Error {
	a.log.Debug("service authorized", "device", device, "uuid", uuid)
	return nil
}
func (a *agent) Cancel() *dbus.Error { return nil }

// ---- export plan --------------------------------------------------------------

// exportSpec is one interface to export on one path.
type exportSpec struct {
	path    dbus.ObjectPath
	iface   string
	methods map[string]interface{}
}

// exports lists every (path, interface, method table) to export. Using
// explicit tables (ExportMethodTable) keeps helper methods off the bus.
func (t *gattTree) exports(a *agent) []exportSpec {
	props := func(o *propObject) map[string]interface{} {
		return map[string]interface{}{"Get": o.Get, "GetAll": o.GetAll, "Set": o.Set}
	}
	return []exportSpec{
		{appPath, ifaceObjMgr, map[string]interface{}{"GetManagedObjects": t.GetManagedObjects}},
		{svcPath, ifaceProps, props(t.service)},
		{chrcPath, ifaceProps, props(t.chrc)},
		{chrcPath, ifaceChrc, map[string]interface{}{
			"ReadValue":   t.ReadValue,
			"WriteValue":  t.WriteValue,
			"StartNotify": t.StartNotify,
			"StopNotify":  t.StopNotify,
		}},
		{advPath, ifaceProps, props(t.adv)},
		{advPath, ifaceAdv, map[string]interface{}{"Release": t.Release}},
		{agentPath, ifaceAgent, map[string]interface{}{
			"Release":              a.Release,
			"RequestPinCode":       a.RequestPinCode,
			"DisplayPinCode":       a.DisplayPinCode,
			"RequestPasskey":       a.RequestPasskey,
			"DisplayPasskey":       a.DisplayPasskey,
			"RequestConfirmation":  a.RequestConfirmation,
			"RequestAuthorization": a.RequestAuthorization,
			"AuthorizeService":     a.AuthorizeService,
			"Cancel":               a.Cancel,
		}},
	}
}

// ---- introspection -------------------------------------------------------------

func arg(name, typ, dir string) introspect.Arg {
	return introspect.Arg{Name: name, Type: typ, Direction: dir}
}

func propsInterface() introspect.Interface {
	return introspect.Interface{
		Name: ifaceProps,
		Methods: []introspect.Method{
			{Name: "Get", Args: []introspect.Arg{arg("interface", "s", "in"), arg("name", "s", "in"), arg("value", "v", "out")}},
			{Name: "GetAll", Args: []introspect.Arg{arg("interface", "s", "in"), arg("properties", "a{sv}", "out")}},
			{Name: "Set", Args: []introspect.Arg{arg("interface", "s", "in"), arg("name", "s", "in"), arg("value", "v", "in")}},
		},
		Signals: []introspect.Signal{
			{Name: "PropertiesChanged", Args: []introspect.Arg{arg("interface", "s", ""), arg("changed_properties", "a{sv}", ""), arg("invalidated_properties", "as", "")}},
		},
	}
}

func ro(name, typ string) introspect.Property {
	return introspect.Property{Name: name, Type: typ, Access: "read"}
}

// nodes returns the introspection data for every exported path.
func (t *gattTree) nodes() map[dbus.ObjectPath]*introspect.Node {
	return map[dbus.ObjectPath]*introspect.Node{
		appPath: {Interfaces: []introspect.Interface{{
			Name: ifaceObjMgr,
			Methods: []introspect.Method{{Name: "GetManagedObjects", Args: []introspect.Arg{
				arg("objects", "a{oa{sa{sv}}}", "out")}}},
			Signals: []introspect.Signal{
				{Name: "InterfacesAdded", Args: []introspect.Arg{arg("object", "o", ""), arg("interfaces", "a{sa{sv}}", "")}},
				{Name: "InterfacesRemoved", Args: []introspect.Arg{arg("object", "o", ""), arg("interfaces", "as", "")}},
			},
		}}, Children: []introspect.Node{{Name: "service0"}}},
		svcPath: {Interfaces: []introspect.Interface{
			propsInterface(),
			{Name: ifaceSvc, Properties: []introspect.Property{ro("UUID", "s"), ro("Primary", "b")}},
		}, Children: []introspect.Node{{Name: "char0"}}},
		chrcPath: {Interfaces: []introspect.Interface{
			propsInterface(),
			{
				Name: ifaceChrc,
				Methods: []introspect.Method{
					{Name: "ReadValue", Args: []introspect.Arg{arg("options", "a{sv}", "in"), arg("value", "ay", "out")}},
					{Name: "WriteValue", Args: []introspect.Arg{arg("value", "ay", "in"), arg("options", "a{sv}", "in")}},
					{Name: "StartNotify"},
					{Name: "StopNotify"},
				},
				Properties: []introspect.Property{
					ro("UUID", "s"), ro("Service", "o"), ro("Flags", "as"), ro("Notifying", "b"), ro("Value", "ay"),
				},
			},
		}},
		advPath: {Interfaces: []introspect.Interface{
			propsInterface(),
			{
				Name:    ifaceAdv,
				Methods: []introspect.Method{{Name: "Release"}},
				Properties: []introspect.Property{
					ro("Type", "s"), ro("ServiceUUIDs", "as"), ro("LocalName", "s"), ro("Discoverable", "b"),
				},
			},
		}},
		agentPath: {Interfaces: []introspect.Interface{{
			Name: ifaceAgent,
			Methods: []introspect.Method{
				{Name: "Release"},
				{Name: "RequestPinCode", Args: []introspect.Arg{arg("device", "o", "in"), arg("pincode", "s", "out")}},
				{Name: "DisplayPinCode", Args: []introspect.Arg{arg("device", "o", "in"), arg("pincode", "s", "in")}},
				{Name: "RequestPasskey", Args: []introspect.Arg{arg("device", "o", "in"), arg("passkey", "u", "out")}},
				{Name: "DisplayPasskey", Args: []introspect.Arg{arg("device", "o", "in"), arg("passkey", "u", "in"), arg("entered", "q", "in")}},
				{Name: "RequestConfirmation", Args: []introspect.Arg{arg("device", "o", "in"), arg("passkey", "u", "in")}},
				{Name: "RequestAuthorization", Args: []introspect.Arg{arg("device", "o", "in")}},
				{Name: "AuthorizeService", Args: []introspect.Arg{arg("device", "o", "in"), arg("uuid", "s", "in")}},
				{Name: "Cancel"},
			},
		}}},
	}
}

// exportAll registers everything on conn.
func (t *gattTree) exportAll(conn *dbus.Conn, a *agent) error {
	for _, e := range t.exports(a) {
		if err := conn.ExportMethodTable(e.methods, e.path, e.iface); err != nil {
			return err
		}
	}
	for path, node := range t.nodes() {
		if err := conn.Export(introspect.NewIntrospectable(node), path, ifaceIntro); err != nil {
			return err
		}
	}
	return nil
}
