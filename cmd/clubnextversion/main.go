package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nwylynko/clubhouse-cli/internal/nextversion"
)

func main() {
	consume := flag.Bool("consume", false, "delete the pending entries and append their sentences to CHANGELOG.md")
	flag.Parse()

	root := "."
	if args := flag.Args(); len(args) > 0 {
		root = args[0]
	}

	ctx := context.Background()
	lastTag, err := nextversion.LastReleaseTag(ctx, root)
	if err != nil {
		fail(err)
	}
	rel, err := nextversion.LoadRelease(ctx, root, lastTag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if !*consume {
		if err := nextversion.RenderNextVersion(os.Stdout, rel); err != nil {
			fail(err)
		}
		return
	}

	result, err := nextversion.ConsumeLedger(root, rel)
	if err != nil {
		fail(err)
	}
	if !result.Appended {
		fmt.Printf("the ledger is empty: nothing consumed, %s unchanged.\n", nextversion.ChangelogPath)
		fmt.Printf("%s is still the derived release — commit and tag it.\n", rel.Next)
		return
	}
	for _, file := range result.Removed {
		fmt.Printf("consumed %s\n", file)
	}
	fmt.Printf("appended %d sentence(s) to %s under %s\n", len(result.Removed), nextversion.ChangelogPath, result.Version)
	fmt.Printf("now commit the tree, then tag it %s — neither of those is this tool's to do.\n", result.Version)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "next version:", err)
	os.Exit(1)
}
