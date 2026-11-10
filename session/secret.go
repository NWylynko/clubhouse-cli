package session

import (
	"os"
	"path/filepath"
	"strings"
)

func (p Paths) Secret() string { return filepath.Join(p.Root, "secret") }

func (p Paths) MemberID() string { return filepath.Join(p.Root, "member-id") }

func WriteMembership(pin, memberID, secret string) error {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.Secret(), []byte(secret), 0o600); err != nil {
		return err
	}
	if memberID == "" {
		return nil
	}
	return os.WriteFile(p.MemberID(), []byte(memberID), 0o600)
}

func ReadMembership(pin string) (memberID, secret string) {
	return ReadMemberID(pin), ReadMemberSecret(pin)
}

func ReadMemberSecret(pin string) string {
	return readTrimmed(ForPin(pin).Secret())
}

func ReadMemberID(pin string) string {
	return readTrimmed(ForPin(pin).MemberID())
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
