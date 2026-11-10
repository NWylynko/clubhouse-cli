package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func (p Paths) RelayTickets() string { return filepath.Join(p.Root, "relay-tickets.json") }

func WriteRelayTickets(pin string, tickets map[string]string) error {
	if len(tickets) == 0 {
		return nil
	}
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(tickets)
	if err != nil {
		return err
	}
	return os.WriteFile(p.RelayTickets(), b, 0o600)
}

func ReadRelayTickets(pin string) map[string]string {
	b, err := os.ReadFile(ForPin(pin).RelayTickets())
	if err != nil || len(b) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
