package web

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"emm/internal/config"
	"emm/internal/sshaccess"
)

// SSHControl switches SSH access on the appliance image on and off. It is nil
// where there is nothing to switch (a Raspberry Pi OS install, a Mac).
type SSHControl interface {
	// Apply makes the system match the settings: server running or stopped, root's
	// password hash, authorized keys.
	Apply(enabled bool, passwordHash string, keys []string) error
	// Status reports whether the server is running and the host key fingerprint.
	Status() (running bool, fingerprint string)
}

type sshBody struct {
	Available   bool     `json:"available"`
	Enabled     bool     `json:"enabled"`
	Running     bool     `json:"running"`
	PasswordSet bool     `json:"password_set"`
	Keys        []string `json:"keys"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	// NeedsWebPassword: SSH gives a root shell, so it can only be switched on once
	// the web interface itself is protected by a password.
	NeedsWebPassword bool   `json:"needs_web_password"`
	Host             string `json:"host"`
}

const minSSHPassword = 8

func (s *Server) webPasswordSet() bool {
	if s.d.BootWebPassword != "" {
		return true
	}
	set := false
	s.d.Store.View(func(st *config.State) { set = st.WebPasswordSet() })
	return set
}

func (s *Server) buildSSH() sshBody {
	if s.d.SSH == nil {
		return sshBody{Keys: []string{}}
	}
	st := s.d.Store.Snapshot()
	b := sshBody{
		Available: true, Enabled: st.SSH.Enabled, PasswordSet: st.SSH.PasswordHash != "",
		Keys: append([]string{}, st.SSH.AuthorizedKeys...), Host: s.d.Hostname() + ".local",
		NeedsWebPassword: !s.webPasswordSet(),
	}
	b.Running, b.Fingerprint = s.d.SSH.Status()
	return b
}

func (s *Server) handleSSHGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildSSH())
}

// PUT /api/ssh {"enabled": true, "password": "...", "authorized_keys": "..."}.
// Every field is optional. An empty password removes the password login; an empty
// authorized_keys removes the keys.
func (s *Server) handleSSHPut(w http.ResponseWriter, r *http.Request) {
	if s.d.SSH == nil {
		writeErr(w, http.StatusNotImplemented, "SSH access can only be switched here on the device image")
		return
	}
	var body struct {
		Enabled        *bool   `json:"enabled"`
		Password       *string `json:"password"`
		AuthorizedKeys *string `json:"authorized_keys"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cur := s.d.Store.Snapshot().SSH
	enabled, hash, keys := cur.Enabled, cur.PasswordHash, cur.AuthorizedKeys
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if body.Password != nil {
		pw := *body.Password
		switch {
		case pw == "":
			hash = ""
		case utf8.RuneCountInString(pw) < minSSHPassword || len(pw) > 100:
			writeErr(w, http.StatusBadRequest, "the SSH password must be 8–100 characters")
			return
		default:
			h, err := sshaccess.HashPassword(pw)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "could not process the password")
				return
			}
			hash = h
		}
	}
	if body.AuthorizedKeys != nil {
		k, err := sshaccess.ParseKeys(*body.AuthorizedKeys)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		keys = k
	}
	if enabled {
		if !s.webPasswordSet() {
			writeErr(w, http.StatusConflict, "set a password for this web interface first (System tab): SSH gives a root shell, and without it anyone on your network could switch SSH on")
			return
		}
		if hash == "" && len(keys) == 0 {
			writeErr(w, http.StatusBadRequest, "set an SSH password or add a public key first")
			return
		}
	}
	if err := s.d.SSH.Apply(enabled, hash, keys); err != nil {
		s.log.Warn("could not apply the SSH settings", "err", err)
		writeErr(w, http.StatusBadGateway, strings.TrimSpace(err.Error()))
		return
	}
	s.d.Store.Update(func(st *config.State) {
		st.SSH.Enabled, st.SSH.PasswordHash, st.SSH.AuthorizedKeys = enabled, hash, keys
	})
	writeJSON(w, http.StatusOK, s.buildSSH())
}
