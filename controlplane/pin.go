package controlplane

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"unicode"
)

const (
	pinLen      = 6
	pinAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func ValidatePIN(pin string) error {
	if len(pin) != pinLen {
		return fmt.Errorf("pin must be exactly %d characters", pinLen)
	}
	for _, r := range pin {
		if r > unicode.MaxASCII || (!unicode.IsLower(r) && !unicode.IsDigit(r)) {
			return fmt.Errorf("pin must be lowercase alphanumeric [a-z0-9]")
		}
	}
	return nil
}

func MintPIN() (string, error) {
	out := make([]byte, pinLen)
	for i := range out {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(pinAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = pinAlphabet[v.Int64()]
	}
	return string(out), nil
}
