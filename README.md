# amgo

A standalone Go CLI for native coding-agent session messaging and supervised
jobs across Claude Code, Codex, Antigravity CLI, Qwen Code and OpenCode.

**Status: v0.1 implemented; native delivery gates passing.** Claude and Codex
session messaging is covered by offline fixture tests and by an opt-in native
suite that launches disposable sessions in private homes. Manual gates remain
for Claude session reset, Codex approval routing, real PID recycling and the
Codex ABA reload window. Jobs and the wider adapters remain later milestones.

- [Quickstart](docs/quickstart.md)
- [Security notes](docs/security.md)
- [Publishing checklist](docs/publishing.md)

Implementation plans, research notes and audit evidence are kept as local
development material under `dev-docs/` and are not tracked.

## What exists

| Area | Status |
|---|---|
| One CGO-free Go binary | implemented |
| Versioned JSON envelope and stable exit codes | implemented |
| bbolt state with short transactions, reservations and counters | implemented |
| Explicit join/leave and metadata-only discovery | implemented |
| Claude UDS send, peer-key handling, session-ID guard | implemented; native gate passing |
| Codex WebSocket discovery and queue send | implemented; native gate passing |
| Idempotency, budgets, reply/ack/wait, recovery, export, prune | implemented |
| Doctor and read-only state invariants | implemented |
| Jobs, OpenCode/Qwen live messaging, managed/ACP | later milestones |
| Linux native certification and Windows | not claimed |

## Install

```sh
brew install xiaolai/tap/amgo
```

Or build from source (Go 1.26+):

```sh
CGO_ENABLED=0 go build -trimpath -o dist/amgo ./cmd/amgo
```

## Build and test

```sh
CGO_ENABLED=0 go build -trimpath -o dist/amgo ./cmd/amgo
go test ./...
go test -race ./...
go vet ./...
make secrets   # publication-set and history secret scan
```

The multi-process store test is opt-in:

```sh
AMGO_CONTENTION=1 go test ./internal/store -run TestContentionMultiProcess -count=1
```

The native delivery gates are opt-in. They launch disposable Claude and Codex
sessions in private homes, never a personal session, and may spend model
tokens:

```sh
AMGO_NATIVE=1 make native
```

See `tests/native/README.md` for the gate matrix and the manual remainder.

## Honest limits

- A Claude send is `submitted` when the native inbox socket accepts the frame.
  Holds, refusals and rate-limit drops are not observable to an external sender.
  The recipient must acknowledge explicitly.
- The Codex app-server WebSocket and `thread/queue/add` are experimental. The
  adapter is pinned to the tested minor release and fails closed otherwise.
- The Codex loaded-target guard is best-effort; a loaded/unloaded/reloaded cycle
  between checks is invisible.
- A send to an idle Codex thread starts a turn and can spend tokens.
- No amgo process survives a bounded command. There is no always-on daemon.
- License: ISC.
