package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nwylynko/clubhouse-cli/detect"
)

type Role string

const (
	RoleHost   Role = "host"
	RoleClient Role = "client"
)

func Home() string {
	if h := os.Getenv("CLUB_HOME"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".club")
	}
	return ".club"
}

type Paths struct {
	Root      string
	Meta      string
	Canonical string
	Mirror    string
	Work      string
	Cursors   string
}

func ForPin(pin string) Paths {
	root := filepath.Join(Home(), "sessions", pin)
	return Paths{
		Root:      root,
		Meta:      filepath.Join(root, "session.json"),
		Canonical: filepath.Join(root, "canonical"),
		Mirror:    filepath.Join(root, "mirror"),
		Work:      filepath.Join(root, "work"),
		Cursors:   filepath.Join(root, "cursors.json"),
	}
}

type Session struct {
	PIN             string         `json:"pin"`
	Role            Role           `json:"role"`
	Branch          string         `json:"branch"`
	HostOverlayAddr string         `json:"hostOverlayAddr"`
	MemberID        string         `json:"memberId,omitempty"`
	Capability      detect.Profile `json:"capability,omitempty"`
}

func (s Session) Save() error {
	p := ForPin(s.PIN)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.Meta, b, 0o644)
}

func Load(pin string) (Session, error) {
	var s Session
	b, err := os.ReadFile(ForPin(pin).Meta)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
