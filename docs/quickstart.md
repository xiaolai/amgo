# amgo quickstart

`amgo` is one standalone binary for messaging an existing Claude Code or Codex
session. It does not run a daemon, MCP server, or model SDK.

## Install

```sh
brew install xiaolai/tap/amgo
amgo version
```

Build from source with Go 1.26+ if you prefer:

```sh
CGO_ENABLED=0 go build -trimpath -o dist/amgo ./cmd/amgo
./dist/amgo version
./dist/amgo doctor
```

The rest of this guide assumes `amgo` is on your `PATH`.

State lives under `AMGO_HOME` or the platform default. Use `--home <dir>` for a
disposable state root.

## Claude Code

Discover live sessions. Discovery reads the native registry only; it does not
scan transcripts or send messages.

```sh
amgo sessions --discover --config-home "${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
amgo join claude --name architect --session <native-session-id> \
  --config-home "${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
```

Send. The first send to a Claude target requires an explicit acknowledgement
that native holds and refusals are not observable:

```sh
printf '%s' 'Please review the parser change.' | \
  amgo send architect --stdin --accept-unknown-delivery --expect-reply
```

A successful command means the native inbox socket accepted the frame. It does
not mean the recipient's model read it. A recipient in a holding permission
class may never see it. Ask the recipient to run `amgo ack <message-id>`.

## Codex

The Codex adapter talks to the shared local app-server daemon. Only threads
loaded in that daemon are messageable; a stored-but-unloaded thread is refused
because queueing it would load a new runtime.

```sh
amgo sessions --discover --codex-home "${CODEX_HOME:-$HOME/.codex}"
amgo join codex --name builder --thread <thread-id>
printf '%s' 'Please run the focused tests.' | amgo send builder --stdin
```

Sending to an idle Codex thread starts a turn immediately and can spend tokens.
The command returns after the queue submission, not after the turn.

## Replies and acknowledgement

Every message body starts with an amgo header carrying its message ID. A
recipient can reply with:

```sh
printf '%s' 'Done; the tests pass.' | amgo reply <message-id> --stdin --as <their-alias>
```

Or acknowledge without a reply:

```sh
amgo ack <message-id> --as <their-alias>
```

Agents should pass `--as <their-alias>` on sends and replies. Without it, the
idempotency namespace is the operator, not the session, so a retry from a
different environment can look like a new request.

The operator can wait for an acknowledgement or reply:

```sh
amgo wait <message-id> --until acknowledged --timeout 60s
amgo inbox
amgo read <message-id>
```

## Retrying an unknown outcome

`unknown` is never retried automatically. If the operator accepts that the
original submission may already have reached the target, resend it explicitly:

```sh
amgo retry <message-id> --accept-duplicate-risk
```

The retry reuses the stored body and conversation, creates a new message with
`retry_of` set, and records `retried_by` on the original. It refuses messages
that are not `unknown`.

## Conversations and budgets

A bare send creates a one-shot conversation with a four-message, one-hour
budget. A named conversation is explicit:

```sh
amgo conversation create review --max-messages 12 --expires 1h
printf '%s' 'First finding.' | amgo send builder --conversation review --stdin
amgo conversation show review
amgo conversation close review
```

Closing blocks new amgo sends; it cannot recall native text already queued.

## Delivery checks and recovery

```sh
amgo doctor --state          # read-only invariants
amgo doctor --probe architect --timeout 60s   # costs one model turn
amgo recover                 # reconcile dead attempts, import the journal
```

`doctor --probe` sends a nonce and waits for an acknowledgement whose
provenance is the bound recipient. An operator acknowledgement does not satisfy
the probe.

Retention is explicit. `prune --yes` removes fully acknowledged messages older
than 30 days; `--compact` rebuilds the database so the file actually shrinks:

```sh
amgo prune --dry-run
amgo prune --yes --compact
```

## Jobs

A job runs one owned, non-interactive provider process. The prompt, state,
native session ID, usage, bounded log and terminal evidence are stored locally.
Use exactly one of `--prompt-file`, `--stdin` or `--text`; the prompt is passed
to the provider on stdin and does not appear in the process list.

```sh
amgo run codex --cwd . --prompt-file task.txt --deadline 15m
amgo run claude --cwd . --prompt-file review.txt --background
amgo jobs --limit 20
amgo status <job-id>
amgo follow <job-id> --after 0 --timeout 60s
amgo result <job-id>
amgo continue <job-id> --prompt-file followup.txt
amgo cancel <job-id>
```

States are `created → starting → running → completed | partial | needs_action |
failed | timed_out | cancelled | interrupted | unknown`. `completed` requires a
terminal provider event; a clean provider exit without one is `partial` and
`result_unverified`. A lost supervisor yields `interrupted`, or leaves the job
unresolved with a held lease when the provider may still be running; it never
yields `completed`.

One amgo job owns a checkout at a time. A second `run` there returns
`workspace_busy` (exit 10). The lease coordinates amgo writers only; it cannot
exclude an editor, build tool or another agent.

If amgo cannot prove that the provider and its process group are gone, it does
not release the lease. The job stays unresolved and `status` shows the reason.
After inspecting the process yourself, release the workspace with
`amgo cancel <job-id> --accept-unconfirmed-termination`; that flag is an
explicit acceptance of the risk that a writer may still be running.

`--policy review` maps to Codex's read-only sandbox. Claude and OpenCode refuse
that policy because they do not expose an equivalent enforceable sandbox.
Codex resume cannot change its sandbox, so a review continuation is refused.

`--background` starts one detached supervisor and returns immediately. The
supervisor exits after the provider does. `cancel` asks the provider to cancel
with SIGINT, then escalates to SIGTERM and SIGKILL, and confirms process-group
exit.

`continue` requires a terminal job with a native session ID, refuses while that
session is registered as live, and creates a new job linked by `resume_of`.

## OpenCode sessions

The TUI's random port has no documented discovery file. Start a loopback
server, or start the TUI with an explicit port, and name that endpoint:

```sh
opencode serve --port 4096            # or: opencode --port 4096
amgo sessions --discover --endpoint http://127.0.0.1:4096
amgo join opencode --name oc --session ses_... --endpoint http://127.0.0.1:4096
printf '%s' 'Review the parser change.' | \
  amgo send oc --stdin --accept-unknown-delivery
```

Set `OPENCODE_SERVER_PASSWORD` (and optionally
`OPENCODE_SERVER_USERNAME`) in the environment when the server uses basic
auth. amgo reads the credential at send time; it is never stored. HTTP 204
means the prompt was accepted, not delivered or completed. Only a loopback
endpoint is accepted.

## Qwen sessions (documented-only)

Qwen publishes a cross-session registry and an authenticated Unix-socket
inbox. amgo implements that protocol from the official documentation; there is
no native gate, so the send path requires the operator's admission:

```sh
amgo sessions --discover --qwen-home "${QWEN_HOME:-$HOME/.qwen}"
amgo join qwen --name qw --session <qwen-session-id>
printf '%s' 'Status?' | \
  amgo send qw --stdin --admit-experimental --accept-unknown-delivery
```

amgo presents only the target registry `ipcToken`, never an own-process or
controller token, and omits `fromMode`, so a receiver that applies mode parity
holds the message for review. A delivery-status receipt is observed for a
short bounded window; no receipt means delivery is unverified. On systems
without Linux's `procStart`/`pidNs`, PID reuse is not distinguishable.

## Qwen and agy jobs (documented-only)

The headless commands are built from the official Qwen and Antigravity CLI
documentation. They fail closed without the operator flag, and neither is
native-certified here:

```sh
amgo --admit-experimental run qwen --cwd . --prompt-file task.txt
amgo --admit-experimental run agy --cwd . --prompt-file task.txt --effort high
```

Qwen documents no reasoning-effort flag, so `--effort` is refused. agy exits
with an eligibility error on the current account. Neither path adds a
permission bypass. Review policy is refused for both because no enforceable
read-only sandbox is documented and certified.

## ACP agents (documented-only)

ACP v1 runs JSON-RPC/NDJSON over the agent's stdio. The agent command is
explicit operator input; amgo offers no discovery or config-file selection:

```sh
amgo --admit-experimental run acp --cwd . --prompt-file task.txt \
  --acp-command /path/to/agent --acp-arg=--acp
```

The client advertises no filesystem or terminal capabilities, never
auto-authenticates, and rejects every permission request. A rejected request
makes the job `needs_action`. Continuation uses `session/load` and is refused
when the agent does not advertise `loadSession`. This lane has fake-agent
fixtures only; no real ACP agent is installed or certified here.

## Honest limits

- Claude has no observable native receipt for an external sender. Holds,
  refusals and rate-limit drops look like silence. `native_disposition` stays
  `unknown`.
- The Codex app-server WebSocket and `thread/queue/add` are experimental. The
  adapter fails closed outside the tested minor version.
- The Codex loaded-target check is best-effort: a loaded, unloaded and reloaded
  thread between checks is invisible.
- A provider can detach a grandchild that ignores provider cancellation. amgo
  confirms the process group it owns; it does not claim a kernel sandbox.
- Claude jobs inherit local permission settings and may be denied tools; that is
  `needs_action` or `partial`, never `completed`.
- Job logs are private (0600) and capped at 100 MiB with explicit truncation.
  They can contain model output and tool output.
- Qwen, agy and ACP mutation paths are documented-only and refuse to run
  without `--admit-experimental`; native behavior is not certified. agy's
  current account is not eligible in this location.
- ACP runs only the command the operator supplies. It provides no sandbox,
  filesystem, terminal or auto-authentication capability.
- Qwen registry tokens are read just in time; they are never stored, logged or
  exported. On non-Linux systems PID reuse is not distinguishable.
- OpenCode needs an explicit loopback endpoint because the TUI's random port
  has no documented discovery file. HTTP 204 is acceptance only.
- No amgo process survives a bounded command, wait or job.
- Windows, SSH and remote hosts are not supported in v0.2.
