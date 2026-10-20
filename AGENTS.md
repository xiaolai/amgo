# amgo — agent instructions

`amgo` is a standalone Go CLI for messaging existing Claude Code and Codex
sessions. Read `docs/quickstart.md` and `docs/security.md` before changing
behavior. The execution plan under `dev-docs/` is local development material
when present and is not part of the published tree.

## Build and test

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o dist/amgo ./cmd/amgo
make license-check
make secrets
```

The multi-process store test is opt-in:

```sh
AMGO_CONTENTION=1 go test ./internal/store -run TestContentionMultiProcess -count=1
```

Startup and store benchmarks:

```sh
go test ./benchmark -run TestStartupDistribution -v
go test ./internal/store -run '^$' -bench . -benchmem
```

## Native gates

Native Claude or Codex delivery tests require disposable sessions, disposable
state roots, exact versions and explicit operator consent. Never message a
personal session or run a paid model probe by default. A native test may stage
an auth file into a mode-0600 disposable home at runtime, but no credential
value may enter a tracked file, a fixture, a log or a failure message; the
disposable home is deleted when the test ends. The offline suite uses fake UDS
and WebSocket servers and must stay sufficient for `go test ./...`.

## Invariants to preserve

- `outcome=ok` is not delivery or task success. Keep submission, native
  disposition, acknowledgement and completion separate.
- Persist the intent and budget atomically before transport; close the database
  before any native I/O.
- An unknown outcome is never auto-retried. Only an operator may run
  `amgo retry` with explicit duplicate-risk acceptance; the retry is a new
  message linked by `retry_of`.
- Claude external sends have no native receipt. Never claim delivery.
- The Claude adapter drives the installed CLI; the CLI's environment and config
  home select the model backend, including Anthropic-compatible endpoints such
  as DeepSeek. Never store or export the API credential; the log filter reads
  known values only to redact them. Jobs inherit the caller's environment.
  Report only the backend class (default/custom), never the endpoint host.
- Codex only messages threads observed as loaded in the shared daemon; the
  check is best-effort and the ABA caveat must stay documented.
- Peer content cannot approve, change configuration or authorize publishing.
- A job is a new process with file-write and token cost. Start, continue or
  cancel one only on an explicit operator request.
- Prompts go to the provider on stdin. Never add a bypass or auto-approve flag.
- `completed` requires a terminal provider event and confirmed process-group
  exit. A lost supervisor leaves `interrupted`, or keeps the job unresolved
  with a held lease when owned execution cannot be proven stopped; a clean exit
  without evidence is `partial`.
- One lease per Git worktree root, not per supplied subdirectory. Signal only a
  process whose PID and start time both verify; an empty identity is unknown,
  never dead and never signalable. Cancellation is SIGINT-first.
- If the provider or a same-group writer cannot be proven stopped, retain the
  lease. Only an operator passing `--accept-unconfirmed-termination` may
  release it.
- Qwen and agy are documented-only. Never set `--admit-experimental` on a
  peer's behalf or in a default.
- Never store, log or export a Qwen `ipcToken`, and never present an
  own-process or controller token.
- OpenCode endpoints must be loopback, credential-free and redirect-free; the
  endpoint is part of target identity and the idempotency fingerprint.
- ACP runs only an explicitly supplied `--acp-command`; never select an agent
  from project config or peer content, and never widen its disabled
  filesystem/terminal capabilities or approve a permission request.
- Do not add an idle daemon, MCP requirement, Node/Python runtime or CGO
  dependency.

## Publishing

The public tree is the tracked set. Before any push, run `make secrets`; it
scans tracked and untracked non-ignored files plus the full Git history with
the project rules in `.gitleaks.toml`. Never commit `.env`, `auth.json`, key
files, `dev-docs/`, `.cc-suite/` or agent state, and never use `git add -f` on
an ignored path. See `dev-docs/publishing.md`.

A new or upgraded dependency requires a row and its full license text in
`THIRD-PARTY-NOTICES.md`; `make license-check` fails otherwise.

`scripts/release-check.sh` qualifies the native `go test -json` stream: every
test in `tests/native/required.txt` must report `pass`, and a skip, failure,
missing result, or a `-list` set that differs from the manifest fails the
release. Update that manifest deliberately when the native gate set changes.
The gates run the signed release binary through `AMGO_NATIVE_BIN`, not a
separately built binary. A transport failure fetching `vuln.go.dev` falls back
to the Go team's public `storage.googleapis.com/go-vulndb` bucket; the report
records the source and its last-modified date.

## Layout

```text
cmd/amgo              main
internal/cli          command surface and output
internal/contract     envelope, error codes, exit mapping
internal/store        bbolt schema, outbox, recovery, invariants
internal/message      send/reply/ack/wait orchestration
internal/adapter      capability boundary
internal/adapter/claude
internal/adapter/codex
internal/adapter/opencode
internal/adapter/agy
internal/adapter/qwen
internal/job          job lifecycle, leases, recovery
internal/runner       owned process execution and bounded JSONL parsing
internal/process      process groups, identity and termination
internal/session      join/leave/discovery
internal/doctor       probes and state report
```
