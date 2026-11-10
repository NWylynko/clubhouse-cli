package controlplane

import (
	"os"
	"strings"
)

var DefaultURL = "http://127.0.0.1:7777"

func BaseURL(override string) string {
	if u := strings.TrimSpace(override); u != "" {
		return strings.TrimRight(u, "/")
	}
	if u := strings.TrimSpace(os.Getenv("CLUB_CONTROL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultURL
}
