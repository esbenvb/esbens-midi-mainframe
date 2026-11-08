package netcfg

// Read-only accessors for the web layer. They never talk to NetworkManager:
// GET /api/network is polled by the UI, and a WiFi scan briefly disturbs the
// radio (and with it BLE and RTP-MIDI traffic), so listing networks must not
// start one.

// CachedScan returns the most recent scan result without scanning.
func (m *Manager) CachedScan() []Network {
	return m.cachedScan()
}

// SavedSSID returns the WiFi network that is configured (emm.txt wins over the
// UI's saved setting), or "".
func (m *Manager) SavedSSID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.eff.SSID
}

// EthernetSettings returns the effective wired settings (emm.txt wins).
func (m *Manager) EthernetSettings() EthernetSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.eff.Eth
	e.DNS = append([]string(nil), e.DNS...)
	return e
}
