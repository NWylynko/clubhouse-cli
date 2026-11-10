package sessionnet

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

const KeyLen = 32

type Key [KeyLen]byte

func NewKey() Key {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		panic("sessionnet: no entropy: " + err.Error())
	}
	return k
}

func (k Key) Zero() bool {
	return k == Key{}
}

func (k Key) String() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

func ParseKey(s string) (Key, error) {
	var k Key
	if s == "" {
		return k, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("sessionnet: bad session key: %w", err)
	}
	if len(b) != KeyLen {
		return k, fmt.Errorf("sessionnet: session key is %d bytes, want %d", len(b), KeyLen)
	}
	copy(k[:], b)
	return k, nil
}
