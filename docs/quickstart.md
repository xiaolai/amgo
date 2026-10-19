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

## Honest limits

- Claude has no observable native receipt for an external sender. Holds,
  refusals and rate-limit drops look like silence. `native_disposition` stays
  `unknown`.
- The Codex app-server WebSocket and `thread/queue/add` are experimental. The
  adapter fails closed outside the tested minor version.
- The Codex loaded-target check is best-effort: a loaded, unloaded and reloaded
  thread between checks is invisible.
- No amgo process survives a bounded command or wait.
- Windows, SSH and remote hosts are not supported in v0.1.
