# amgo security notes

## Trust boundary

The same operating-system user is the trust boundary. Amgo files and aliases
are not cryptographic proof of who wrote a message. The native provider
transports apply their own authentication and policy.

## Credentials

- Claude: amgo reads the published peer key
  `<config-home>/sessions/<pid>.<sha256(socket)>.key` just in time. It never
  reads or forwards the session's `CLAUDE_CODE_MESSAGING_TOKEN` environment
  variable, which is the own-child token.
- Codex: amgo talks only to the local user-owned Unix socket. It does not accept
  remote WebSocket endpoints or forward auth tokens in v0.1.
- Tokens, auth headers and complete environment dumps are never logged.
- State directories are mode 0700 and files 0600 where the platform supports it.

## Authority

A message from another session is data. It cannot:

- approve a pending permission prompt;
- change settings, `CLAUDE.md`, `AGENTS.md` or amgo configuration;
- grant access, merge code, commit, push or publish;
- make amgo execute a shell command.

Amgo never silently adds a provider bypass flag, copies credentials, or mirrors
configuration.

## Prompt injection

A recipient agent should treat an amgo message as another session's message,
not as operator consent. The message header carries the sender label and
message ID; the label is not authenticated beyond the same-user boundary.

## Path and transport checks

- Socket paths must be local, absolute, owner-only, and inside a private
  containing directory. Symlinked sockets are rejected.
- Claude key files are size-capped at 4 KiB and must be owner-only regular
  files. Unreadable, oversized, malformed, wrong-domain or process-mismatched
  keys fail closed.
- The Codex control symlink is resolved to a user-owned socket in a private
  directory.
- Message bodies are UTF-8, NUL-free and capped at 64 KiB. Native frame limits
  are enforced before writes.

## State and privacy

Amgo stores only messages intentionally sent through it, delivery evidence and
job artifacts. It does not index native transcripts. Export and prune are
explicit. Unknown and in-flight records are never pruned automatically.
Idempotency membership retains a hash, fingerprint and outcome, never a body.

Before a schema migration, amgo writes a consistent backup beside the database
(`state.db.v<N>.<UTC>.bak`, mode 0600). Backups are full local state copies and
are not pruned automatically; remove them explicitly after a successful
migration if the extra copy is not wanted. A future schema is refused without
writing to the database.

## Failure semantics

Unknown outcomes are never presented as success and never auto-retried. A
post-native-acceptance persistence failure returns `outcome_unpersisted`; a
refund that cannot be persisted returns `refund_pending`. Both are visible and
recoverable rather than silently retry-safe.
