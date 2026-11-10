package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nwylynko/clubhouse-cli/internal/wiresnapshot"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	surface, err := wiresnapshot.GenerateWireSurface(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wire snapshot:", err)
		os.Exit(1)
	}
	out := filepath.Join(root, wiresnapshot.SnapshotPath)
	if err := os.WriteFile(out, []byte(surface), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "wire snapshot:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", out)
	fmt.Println("now classify the change: .wire-changes/<slug>.md (see .wire-changes/README.md)")
}
