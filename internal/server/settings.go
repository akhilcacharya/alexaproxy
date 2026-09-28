package server

import (
	"log"
	"net/http"

	"github.com/akhilcacharya/alexaproxy/internal/store"
)

// defaultDeviceJSON describes the effective default device and where it
// came from: "settings" (set over the API) or "flag" (--default-device).
type defaultDeviceJSON struct {
	Name   string `json:"name"`
	Serial string `json:"serial,omitempty"`
	Source string `json:"source"`
}

func (s *Server) loadSettings() *store.Settings {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.settings == nil {
		st, err := s.Alexa.Store.LoadSettings()
		if err != nil {
			log.Printf("warning: %v; starting with empty settings", err)
			st = &store.Settings{}
		}
		s.settings = st
	}
	return s.settings
}

// defaultDevice returns the effective default device, or nil if none.
// A device set over the API wins over --default-device.
func (s *Server) defaultDevice() *defaultDeviceJSON {
	st := s.loadSettings()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if d := st.DefaultDevice; d != nil {
		return &defaultDeviceJSON{Name: d.Name, Serial: d.Serial, Source: "settings"}
	}
	if s.DefaultDevice != "" {
		return &defaultDeviceJSON{Name: s.DefaultDevice, Source: "flag"}
	}
	return nil
}

func (s *Server) saveDefaultDevice(ref *store.DeviceRef) error {
	st := s.loadSettings()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	next := *st
	next.DefaultDevice = ref
	if err := s.Alexa.Store.SaveSettings(&next); err != nil {
		return err
	}
	s.settings = &next
	return nil
}

func (s *Server) getSettings(http.ResponseWriter, *http.Request, input) (any, error) {
	return map[string]any{"default_device": s.defaultDevice()}, nil
}

func (s *Server) setDefaultDevice(_ http.ResponseWriter, _ *http.Request, in input) (any, error) {
	query, err := in.require("device")
	if err != nil {
		return nil, err
	}
	// Resolve now so a typo fails here rather than on the next command, and
	// store the serial so later renames or look-alike names don't matter.
	devs, err := s.Alexa.Devices()
	if err != nil {
		return nil, err
	}
	d, err := findDevice(devs, query)
	if err != nil {
		return nil, err
	}
	ref := &store.DeviceRef{Name: deviceName(*d), Serial: d.SerialNumber}
	if err := s.saveDefaultDevice(ref); err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "default_device": s.defaultDevice()}
	if !d.Online {
		out["warning"] = "this device is currently offline"
	}
	return out, nil
}

func (s *Server) clearDefaultDevice(http.ResponseWriter, *http.Request, input) (any, error) {
	if err := s.saveDefaultDevice(nil); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "default_device": s.defaultDevice()}, nil
}
