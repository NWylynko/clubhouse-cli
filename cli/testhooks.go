package cli

import (
	"context"
	"sync"

	"github.com/nwylynko/clubhouse-cli/box"
	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/spf13/cobra"
)

type TestHooks struct {
	SetupAgent     conductor.Scaffolder
	BoxRunner      func(*cobra.Command, []string) (box.Runner, error)
	ConsoleEntered func(ConsoleUp)
}

type ConsoleUp struct {
	PIN   string
	Leave func(context.Context) error
}

var (
	hooksMu sync.RWMutex
	hooks   TestHooks
)

func SetTestHooks(h TestHooks) (restore func()) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	prev := hooks
	if h.SetupAgent != nil {
		hooks.SetupAgent = h.SetupAgent
	}
	if h.BoxRunner != nil {
		hooks.BoxRunner = h.BoxRunner
	}
	if h.ConsoleEntered != nil {
		hooks.ConsoleEntered = h.ConsoleEntered
	}
	return func() {
		hooksMu.Lock()
		defer hooksMu.Unlock()
		hooks = prev
	}
}

func testHooks() TestHooks {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return hooks
}
