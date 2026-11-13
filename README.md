# Clubhouse

> Several people, several AI agents, **one** product that actually runs.

Clubhouse is a CLI for short, AI-native hackathons. Everyone stays on their own laptop with their own coding agent (Claude Code, Codex, Cursor), and all of them build into **one shared, always-live project**. You don't end up with five separate prototypes.

## Get started

**1. Install** (macOS or Linux):

```
curl -fsSL https://clubhouse.sh | sh
```

**2. One person hosts.** Run `club` and answer a few questions. Each one has a default, so you can just press Enter:

```
$ club
  ...
  session live. share with your team:

      club join ypl8rk
```

**3. Everyone else joins** with the PIN:

```
club join ypl8rk
```

If a teammate doesn't have Clubhouse yet, this line installs it and joins in one go: `curl -fsSL https://clubhouse.sh/ypl8rk | sh`

**4. Start building.** Step into the session's codebase and run your agent of choice there:

```
club open
```

That's the whole setup. Your agent already knows what to do from there. (Shortcut: `club claude` or `club codex` does both steps in one.)

## What it feels like

- **Your agent just builds.** Joining installs an `AGENTS.md` that tells your agent how to work in the session. It runs `club pull` before a task and `club sync` after one, which brings in everyone else's work and shares its own. Nobody writes commits, opens PRs or resolves merge conflicts.
- **There's always one working product.** Clubhouse merges everyone's work into a single version as it lands, fixes conflicts with the agent CLI you already use, and watches the dev server for errors so it can repair breakage. The dev server shows that combined version live from minute one.
- **No accounts and no setup.** You don't need GitHub or API keys, and git is built in. The only AI it uses is the subscription you already pay for, and it never asks for provider API tokens.
- **Anyone can leave.** You join a *session*, not a machine. If the host's laptop drops out, the session moves to another member without anyone re-joining.

## Commands you'll actually use

| Command | What it does |
|---|---|
| `club` | host a new session and get its PIN |
| `club join <pin>` | join a session |
| `club open` | open a shell in the session's codebase |
| `club claude` / `club codex` | shortcut: open the session and start that agent |
| `club site` | print the URL for the live product |
| `club monitor` | watch the session live: who's in, what's merged |
| `club run <cmd>` | run a command on the host (migrations, seeds, …) |
| `club report` | send diagnostics when something breaks |
| `club update` | update to the latest version |

Run `club --help` for everything else.

## Where the session runs

- **On a teammate's laptop** (default). Nothing extra to set up. Code the agents write runs as that person, so only do this with people you trust.
- **On a cloud box** (`club --box`, or `--box-ssh` for a machine you own). The code runs in an isolated container, and the session keeps going even if every laptop disconnects.

Either way, teammates connect through an end-to-end encrypted tunnel, so nobody has to deal with ports or networking.

## What it's not

- **Not a deploy, CI or code-review tool.** It deliberately skips all of that during the build window.
- **Not a referee.** If two people want to build incompatible things, Clubhouse will merge the code and make it obvious that you disagree.
- **Not a backup.** If the code needs to outlive the session, turn on the optional GitHub mirror.

## Trust and privacy

[`TRUST.md`](./TRUST.md) explains what you're trusting: what the relay can and can't see, what a cloud box holds, and what runs on your machine. Telemetry is **off by default** (`club telemetry on` to opt in), and it never records your code. Details are in [`docs/what-is-recorded.md`](./docs/what-is-recorded.md). To report a security problem, see [`SECURITY.md`](./SECURITY.md).

## Contributing

Issues and PRs are welcome, and bug reports from real sessions are the most useful of all. This repo is the client half of Clubhouse. The servers and most of the test suite live in a private repo, so PRs get validated privately before merging and that can take a while. To build from source (Go 1.26+):

```
git clone https://github.com/NWylynko/clubhouse-cli
cd clubhouse-cli
make build
```

[`AGENTS.md`](./AGENTS.md) covers how the repo is laid out and the rules for working on it.

## License

[PolyForm Shield 1.0.0](./LICENSE.md). This is source-available, not OSI open source. You can use it anywhere (company hackathons included), change it and ship it. The one thing it forbids is building a competing product from its code.
