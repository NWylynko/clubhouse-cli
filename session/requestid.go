package session

import (
	"crypto/rand"
	"encoding/base32"
	"os"
	"path/filepath"
	"strings"
)

func (p Paths) RequestID() string { return filepath.Join(p.Root, "request-id") }

func WriteRequestID(pin, id string) error {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p.RequestID(), []byte(id), 0o600)
}

func ReadRequestID(pin string) string {
	b, err := os.ReadFile(ForPin(pin).RequestID())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func NewRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

func ClearRequestID(pin string) error {
	err := os.Remove(ForPin(pin).RequestID())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
