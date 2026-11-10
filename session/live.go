package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type LiveMarker struct {
	PID       int       `json:"pid"`
	Role      Role      `json:"role"`
	Branch    string    `json:"branch,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	DevURL    string    `json:"devUrl,omitempty"`
	ReadyAt   time.Time `json:"readyAt,omitempty"`
}

type LiveEntry struct {
	PIN  string
	Live LiveMarker
}

func (p Paths) Live() string { return filepath.Join(p.Root, "live.json") }

func WriteLive(s Session) error {
	now := time.Now().UTC()
	return WriteLiveMarker(s.PIN, LiveMarker{
		PID:       os.Getpid(),
		Role:      s.Role,
		Branch:    BranchLabel(s.Branch),
		StartedAt: now,
		ReadyAt:   now,
	})
}

func WriteLiveMarker(pin string, m LiveMarker) error {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.Live(), b, 0o644)
}

func ClearLive(pin string) error {
	err := os.Remove(ForPin(pin).Live())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func PublishDevURL(pin, url string) error {
	path := ForPin(pin).Live()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m LiveMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m.DevURL == url {
		return nil
	}
	m.DevURL = url
	return WriteLiveMarker(pin, m)
}

func LiveHere(pin string) (LiveMarker, bool) {
	path := ForPin(pin).Live()
	b, err := os.ReadFile(path)
	if err != nil {
		return LiveMarker{}, false
	}
	var m LiveMarker
	if err := json.Unmarshal(b, &m); err != nil {
		_ = os.Remove(path)
		return LiveMarker{}, false
	}
	if !processAlive(m.PID) {
		_ = os.Remove(path)
		return LiveMarker{}, false
	}
	return m, true
}

func LivePins() ([]LiveEntry, error) {
	root := filepath.Join(Home(), "sessions")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LiveEntry
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pin := e.Name()
		m, ok := LiveHere(pin)
		if !ok {
			continue
		}
		out = append(out, LiveEntry{PIN: pin, Live: m})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Live.StartedAt.After(out[j].Live.StartedAt)
	})
	return out, nil
}

func BranchLabel(branch string) string {
	if branch == "" {
		return ""
	}
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		return branch[i+1:]
	}
	return branch
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
