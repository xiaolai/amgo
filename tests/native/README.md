# Native delivery gates

These tests exercise real Claude Code and Codex sessions. They are opt-in and
never run in the default suite.

```sh
AMGO_NATIVE=1 go test -tags native -count=1 -timeout 20m ./tests/native -v
```

or:

```sh
make native
```

Every test creates its own Claude config home, Codex home, git repository and
state root under `t.TempDir`. No personal session is messaged. Codex auth is
copied from `$HOME/.codex/auth.json` into the disposable home with mode 0600;
the whole directory is removed when the test ends. Codex tests skip when that
file is absent. Claude tests use the inherited `ANTHROPIC_*` environment and
skip when `claude` or `tmux` is unavailable.

## Automated gates

| Gate | Test | What it proves |
|---|---|---|
| Claude accept | `TestClaudeAcceptNonceAndRecipientAck` | A nonce reaches the disposable session transcript; an explicit recipient ack is recorded with `provenance=recipient` |
| Claude hold | `TestClaudeHoldDoesNotDeliver` | A held nonce never reaches the transcript; amgo reports `submitted`, never acknowledged |
| Claude refuse | `TestClaudeRefuseDoesNotDeliver` | A refused nonce never reaches the transcript; amgo reports `submitted` |
| Claude dead target | `TestClaudeDeadTargetReplayAndNewSend` | A new send after process death is `target_unavailable`; an identical replay is served from durable membership without a native send |
| Claude target move | `TestClaudeTargetSnapshotMismatch` | A changed process-start snapshot rejects the send and delivers nothing |
| Claude budget / replay / close / expiry | `TestClaudeBudgetReplayCloseExpiry` | Budget exhaustion, close and expiry reject new work; the identical replay after exhaustion still returns the original result |
| Codex queue | `TestCodexQueueDeliveryAndIdempotentReplay` | `thread/queue/add` delivers to a disposable TUI; the nonce is acknowledged; an identical replay produces no second native frame |
| Codex busy queue | `TestCodexBusyQueue` | A nonce sent during a running turn is queued and delivered after it |
| Codex dead endpoint | `TestCodexDeadTarget` | Stopping the disposable daemon makes new sends `target_unavailable`; replay remains idempotent |
| Interrupt during write | `TestClaudeInterruptDuringWriteIsUnknown` | A blocked frame interrupted by cancellation is `unknown` with `ErrInterrupted`, never `failed_before_submit` |
| Persistence after acceptance | `TestStoreDiskFullReportsOutcomeUnpersisted` | A post-acceptance write failure returns `outcome_unpersisted` with identifiers and a journal record |

## Gates that remain manual

These require an interactive TUI decision or a process transition the harness
cannot force reliably. Keep the code guards and record evidence when run.

1. **Claude session reset.** Enqueue for session A while it is busy, reset the
   process to session B before the turn completes, then release/drain. Verify
   no A-addressed frame appears in B. The `session_id` ingress guard remains in
   `buildFrame` and is unit-tested, but the transition is not automated.
2. **Codex approval follow-up.** Trigger a benign approval-requiring action,
   disconnect amgo, answer the approval in the TUI, and verify the turn
   continues. This needs a human decision in the TUI.
3. **PID recycling.** The harness mutates the stored process-start snapshot
   instead of forcing an OS PID to be reused. Real PID recycling remains a
   manual check; the process-start comparison is the guard.
4. **ABA thread reload.** The Codex client cannot distinguish a thread that
   reloaded between the loaded check and the queue call. This is documented as
   an accepted limitation; no test can prove its absence.
