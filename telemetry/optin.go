package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwylynko/clubhouse-cli/session"
)

const (
	ModeOn  = "on"
	ModeOff = "off"

	EnvMode = "CLUB_TELEMETRY"

	settingKey = "telemetry"
)

func Resolve() (on bool, why string) {
	if v := strings.TrimSpace(os.Getenv(EnvMode)); v != "" {
		switch strings.ToLower(v) {
		case ModeOn:
			return true, "on — this box was provisioned recording (" + EnvMode + "=on)"
		case ModeOff:
			return false, "off — this box was provisioned not recording (" + EnvMode + "=off)"
		default:
			return false, fmt.Sprintf("off — %s=%q is not on or off, so nothing is recorded", EnvMode, v)
		}
	}
	switch readSetting() {
	case ModeOn:
		return true, "on — you turned it on with `club telemetry on` (this machine)"
	case ModeOff:
		return false, "off — you turned it off with `club telemetry off` (this machine)"
	default:
		return false, "off — the default, on every machine, until somebody runs `club telemetry on`"
	}
}

func SetMode(on bool) error {
	mode := ModeOff
	if on {
		mode = ModeOn
	}
	return writeSetting(mode)
}

func defaultsPath() string { return filepath.Join(session.Home(), "defaults.json") }

func readSetting() string {
	data, err := os.ReadFile(defaultsPath())
	if err != nil {
		return ""
	}
	var all map[string]any
	if json.Unmarshal(data, &all) != nil {
		return ""
	}
	s, _ := all[settingKey].(string)
	return strings.ToLower(strings.TrimSpace(s))
}

func writeSetting(mode string) error {
	if err := os.MkdirAll(session.Home(), 0o755); err != nil {
		return err
	}
	all := map[string]any{}
	if data, err := os.ReadFile(defaultsPath()); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	all[settingKey] = mode
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(defaultsPath(), append(data, '\n'), 0o644)
}

type MemberConfig struct {
	PIN        string
	SessionUID string
	MemberID   string
	Advertised string
	Ticket     string
	Version    string
}

func ForMember(cfg MemberConfig) *Emitter {
	on, _ := Resolve()
	if !on {
		return New(Config{Service: "client", Version: cfg.Version})
	}
	url := SessionIngest(cfg.PIN, cfg.Advertised)
	if url == "" || strings.TrimSpace(cfg.Ticket) == "" {
		return New(Config{Service: "client", Version: cfg.Version})
	}
	return New(Config{URL: url, Bearer: cfg.Ticket, Service: "client", Version: cfg.Version})
}
