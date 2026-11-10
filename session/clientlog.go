package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	ClientLogName = "client.log"
	clientLogMax  = 4 << 20
)

func ClientLogPath(pin string) string {
	return filepath.Join(ForPin(pin).Root, ClientLogName)
}

type ClientLog struct {
	mu sync.Mutex
	f  *os.File
}

func OpenClientLog(pin string) (*ClientLog, error) {
	path := ClientLogPath(pin)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > clientLogMax {
		if b, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(path, b[len(b)-clientLogMax/2:], 0o644)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &ClientLog{f: f}, nil
}

func (l *ClientLog) Write(ts time.Time, level, component, message string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s %-5s %-6s %s\n",
		ts.Format("2006-01-02 15:04:05.000"), strings.ToUpper(level), component, message)
}

func (l *ClientLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func ReadClientLogTail(pin string, n int) (string, error) {
	b, err := os.ReadFile(ClientLogPath(pin))
	if err != nil {
		return "", err
	}
	if n > 0 && len(b) > n {
		b = b[len(b)-n:]
		if i := strings.IndexByte(string(b), '\n'); i >= 0 && i+1 < len(b) {
			b = b[i+1:]
		}
	}
	return string(b), nil
}
