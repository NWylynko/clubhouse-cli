package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Standup struct {
	pin string
	f   *os.File
}

const standupLock = "standup.lock"

func BeginStandup(pin string, role Role, branch string) (*Standup, error) {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.Root, standupLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open standup lock for %s: %w", pin, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock standup for %s: %w", pin, err)
	}
	if err := WriteLiveMarker(pin, LiveMarker{
		PID:       os.Getpid(),
		Role:      role,
		Branch:    BranchLabel(branch),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	return &Standup{pin: pin, f: f}, nil
}

func (s *Standup) Ready(meta Session) error {
	if s == nil {
		return nil
	}
	m := LiveMarker{
		PID:       os.Getpid(),
		Role:      meta.Role,
		Branch:    BranchLabel(meta.Branch),
		StartedAt: time.Now().UTC(),
		ReadyAt:   time.Now().UTC(),
	}
	if prev, ok := readMarker(s.pin); ok {
		m.StartedAt = prev.StartedAt
		m.DevURL = prev.DevURL
		if m.Branch == "" {
			m.Branch = prev.Branch
		}
	}
	if err := WriteLiveMarker(s.pin, m); err != nil {
		return err
	}
	s.unlock()
	return nil
}

func (s *Standup) Release() {
	if s == nil || s.f == nil {
		return
	}
	_ = ClearLive(s.pin)
	s.unlock()
}

func (s *Standup) unlock() {
	if s == nil || s.f == nil {
		return
	}
	syscall.Flock(int(s.f.Fd()), syscall.LOCK_UN)
	s.f.Close()
	s.f = nil
}

func WaitUntilReady(ctx context.Context, pin string) error {
	if m, ok := LiveHere(pin); ok && openable(pin, m) {
		return nil
	}
	path := filepath.Join(ForPin(pin).Root, standupLock)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("wait for session %s: %w", pin, err)
	}
	defer f.Close()

	locked := make(chan error, 1)
	go func() { locked <- syscall.Flock(int(f.Fd()), syscall.LOCK_SH) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-locked:
		if err != nil {
			return fmt.Errorf("wait for session %s: %w", pin, err)
		}
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}

	if m, ok := LiveHere(pin); ok && openable(pin, m) {
		return nil
	}
	return fmt.Errorf("session %s stopped standing up before its work tree was ready — "+
		"check the terminal running `club join %s`", pin, pin)
}

func StandingUp(pin string) bool {
	m, ok := LiveHere(pin)
	return ok && !openable(pin, m)
}

func openable(pin string, m LiveMarker) bool {
	if !m.ReadyAt.IsZero() {
		return true
	}
	_, err := os.Stat(filepath.Join(ForPin(pin).Work, ".git"))
	return err == nil
}

func ReadyHere(pin string) (LiveMarker, bool) {
	m, ok := LiveHere(pin)
	if !ok || !openable(pin, m) {
		return LiveMarker{}, false
	}
	return m, true
}

func readMarker(pin string) (LiveMarker, bool) {
	b, err := os.ReadFile(ForPin(pin).Live())
	if err != nil {
		return LiveMarker{}, false
	}
	var m LiveMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return LiveMarker{}, false
	}
	return m, true
}
