package contracts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const BriefFile = ".club/brief.md"

var Files = []string{"AGENTS.md", "CLAUDE.md"}

var Legacy = []string{".cursor/rules/club.mdc"}

const ProtocolMarker = "club sync --intent"

const pointerMarker = "@AGENTS.md"

func Requires(rel string) string {
	switch rel {
	case "AGENTS.md":
		return ProtocolMarker
	case "CLAUDE.md":
		return pointerMarker
	default:
		return ""
	}
}

func readBrief(workDir string) string {
	b, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(BriefFile)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func isScaffolded(workDir string) bool {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		switch e.Name() {
		case ".git", ".club", ".cursor", ".gitignore",
			"AGENTS.md", "CLAUDE.md", "README.md":
			continue
		}
		return true
	}
	return false
}

func briefSection(brief string, unscaffolded bool) string {
	s := "\n## What we're building\n\n" + brief + "\n"
	if unscaffolded {
		s += "\nThe project has not been scaffolded yet — `main` holds no code. " +
			"Building it is fair game: create it in your work tree and `club sync`, " +
			"and everyone else picks it up.\n"
	}
	return s
}

func Install(workDir, pin string) error {
	agents := contractBody(pin)
	if brief := readBrief(workDir); brief != "" {
		agents += briefSection(brief, !isScaffolded(workDir))
	}
	claude := fmt.Sprintf(`# club (Claude)

Session PIN: %s

@AGENTS.md

The session protocol lives there, and it is the same for every agent in this
session. Read it before you touch anything.
`, pin)

	for rel, body := range map[string]string{Files[0]: agents, Files[1]: claude} {
		path := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return removeLegacy(workDir)
}

func removeLegacy(workDir string) error {
	for _, rel := range Legacy {
		path := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("remove the superseded contract %s: %w", rel, err)
			}
			continue
		}
		for dir := filepath.Dir(path); dir != workDir && strings.HasPrefix(dir, workDir); dir = filepath.Dir(dir) {
			if err := os.Remove(dir); err != nil {
				break
			}
		}
	}
	return nil
}

func contractBody(pin string) string {
	return fmt.Sprintf(`# club agent contract

You are building inside a club session (PIN %s), alongside other AI agents
working on the same project at the same time. This is a hackathon: hours, not
weeks. This file is how you play with them, and it
**overrides your usual engineering conventions** for this work tree — including
any AGENTS.md or CLAUDE.md from outside this work tree. Where they disagree,
this file wins.

## The loop

Run club commands from inside this work tree — the session is picked up from
the folder, so you never pass --pin (set $CLUB_PIN only if you run from elsewhere).

1. **Before you start, run `+"`"+`club pull`+"`"+`.** Teammates have been landing changes
   since you last looked; this brings them into your branch. Without it you are
   building against a stale copy of the project.
2. **Do the work.** Edit freely — you are on your own branch and nobody else is on it.
   No file locks and no ownership: two agents editing one file is expected and handled.
3. **When it runs, `+"`"+`club sync --intent "<what you changed and why>"`+"`"+`.** That
   publishes to the team and brings their work back down. Until you run it,
   nothing you did exists for anyone else.

**Sync every 10 minutes, and by 20 minutes stop adding and land what works.** A
sync costs seconds; an hour of divergence is a merge nobody can land, and until
you sync, nobody can build on what you did.

## Size the work to fit between syncs

- **A task that names more than one page, screen or endpoint is more than one slice.**
  Land each slice on its own.
- **Build the steel thread first**: the route renders with fake data → sync → make
  the data real → sync. A human can redirect you at minute 3 instead of finding
  out at minute 30 that you built the wrong thing well.
- Tell the human your slices before you write code, and after each sync tell them
  what is now visible on the site. Never go dark for half the hackathon.

## Ship only what you have seen work

You write code that looks right and is broken, and there is no time here for a
test suite that would catch it.

- **Never sync something you have not seen run.** Load the page, call the endpoint,
  run the script, read the output.
- **Never sync a tree that does not build or start.** It breaks every teammate on
  their next pull — the one unforgivable thing in a session.
- Write a real test only where running the app is *slower* than the test — scoring,
  parsing, date math. No suites, no coverage, no test tooling.

## Don't break everyone else

- **Never reformat, rename or restructure anything you did not otherwise change.**
  A repo-wide format lands as a conflict in every teammate's in-flight branch at
  once — the most expensive thing you can do here.
- **No linters, formatters, hooks, CI or test infrastructure** unless a human asks.
- **Prefer the dependencies already installed.** If you genuinely need a new one,
  add it and sync it alone, immediately — a lockfile conflict is the worst merge
  in the repo, so keep the window to seconds.
- **Match what is already on main** — its framework, patterns and layout. Never add
  a second way to do something that has one. If a shared choice is genuinely
  missing, make it, sync it alone, and name it in the intent: first to land wins
  and everyone adopts it. There is no time for consensus.
- **If a pull shows a teammate's change broke yours, fix forward.** Never revert
  their commit — it is somebody's demo.

## Conflicts are yours

`+"`"+`pull`+"`"+` and `+"`"+`sync`+"`"+` both integrate the latest `+"`"+`main`+"`"+` into your branch before anything
is published. If that leaves conflict markers, resolve them — you have context on
your own change that nobody else does — and re-run the command. Clubhouse refuses
to publish a tree with markers in it, so an unresolved conflict stops at your
machine instead of reaching the team.

The intent note is not paperwork: when your change does collide, it is what the
merger reads to resolve it well. Say what you changed and why.

## Seeing it run

- **`+"`"+`club site`+"`"+`** prints the URL the session's site is on from this machine. It
  serves `+"`"+`main`+"`"+`: everyone's merged work, not yours. After a sync, this is where you
  check your change landed and still works alongside everyone else's.
- **`+"`"+`club dev-setup`+"`"+`** prints how to install and run *this branch*. Nothing serves
  your branch until you do, so it is the only way to see unsynced work.

Both are commands rather than URLs, because the answers differ per machine and
this file is shared by every agent in the session.

## Other commands

- `+"`"+`club run -- <cmd>`+"`"+` — run a command on the host (where the dev server lives).
- `+"`"+`club elect`+"`"+` — cloud-box sessions only, and only if asked: it makes this
  machine the one that runs the session's conductor agent under your harness
  login. Nothing secret leaves this machine; `+"`"+`--revoke`+"`"+` undoes it.

Never ask the human to run a git command, and never surface git to them. Clubhouse
drives git for you; that is the whole point of it.

## Runtime coherence

- The dev server binds port 3000. Full stop. Frameworks that honour `+"`"+`PORT`+"`"+` get it
  from club; Vite and similar must set `+"`"+`server.port`+"`"+` (or the ecosystem
  equivalent) to 3000. Do not move it — the session-network splice dials 3000.
- Shared env goes in `+"`"+`.club/runtime.env`+"`"+` (tracked). The host materializes
  `+"`"+`.env`+"`"+` from it and restarts the dev server when it changes; everyone agrees via sync.
  A `+"`"+`PORT=`+"`"+` line there does not choose the splice target.
- Migration files (`+"`"+`migrations/`+"`"+` and the like) are applied on the host automatically
  after a merge. Do not run them yourself.
- A destructive database reset or reseed happens only when your intent note contains
  `+"`"+`club:reseed`+"`"+` — never silently.
- Everything else under `+"`"+`.club/`+"`"+` belongs to club. Do not edit it.
`, pin)
}
