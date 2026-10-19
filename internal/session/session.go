// Package session joins native discovery to explicit amgo registrations.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/adapter/claude"
	"amgo/internal/adapter/codex"
	"amgo/internal/adapter/opencode"
	"amgo/internal/adapter/qwen"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/store"
)

var aliasRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func ValidAlias(alias string) bool { return aliasRE.MatchString(alias) }

type JoinRequest struct {
	Alias        string
	Candidate    model.Candidate
	Capabilities model.Capabilities
	Now          time.Time
}

func Join(st *store.Store, req JoinRequest) (model.Session, error) {
	if !ValidAlias(req.Alias) {
		return model.Session{}, model.NewError(model.ErrUsage, "alias must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}")
	}
	if !req.Candidate.Live {
		reason := req.Candidate.Reason
		if reason == "" {
			reason = "target is not live"
		}
		return model.Session{}, model.NewError(model.ErrTargetUnavailable, "cannot join: "+reason)
	}
	if existing, ok, err := st.FindSessionByTarget(req.Candidate.Target.Adapter, req.Candidate.Target.ConfigHome, req.Candidate.Target.NativeSessionID); err != nil {
		return model.Session{}, err
	} else if ok {
		old := existing.Target
		next := req.Candidate.Target
		if old.PID != next.PID || old.ProcStart != next.ProcStart ||
			old.SocketPath != next.SocketPath || old.PIDDomain != next.PIDDomain ||
			old.NativeSessionID != next.NativeSessionID || old.ConfigHome != next.ConfigHome {
			// Delivery uncertainty does not carry across a new process,
			// endpoint, or native target snapshot.
			existing.AcceptUnknownDelivery = false
		}
		existing.Alias = req.Alias
		existing.Target = next
		existing.Capabilities = req.Capabilities
		existing.UnregisteredAt = nil
		if err := st.PutSession(existing); err != nil {
			return model.Session{}, err
		}
		return existing, nil
	}
	sess := model.Session{
		RecordVersion: 1,
		ID:            ids.New("s"),
		Alias:         req.Alias,
		Target:        req.Candidate.Target,
		Capabilities:  req.Capabilities,
		RegisteredAt:  req.Now,
	}
	if err := st.PutSession(sess); err != nil {
		return model.Session{}, err
	}
	return sess, nil
}

func Leave(st *store.Store, alias string, now time.Time) (model.Session, error) {
	sess, err := st.GetSessionByAlias(alias)
	if err != nil {
		return model.Session{}, err
	}
	if err := st.UnregisterSession(sess.ID, now); err != nil {
		return model.Session{}, err
	}
	return sess, nil
}

func Resolve(st *store.Store, alias string) (model.Session, error) {
	sess, err := st.GetSessionByAlias(alias)
	if err != nil {
		return model.Session{}, err
	}
	if sess.UnregisteredAt != nil {
		return model.Session{}, model.NewError(model.ErrTargetUnavailable, "session is not registered")
	}
	if !sess.Capabilities.LiveMessaging {
		return model.Session{}, model.NewError(model.ErrUnsupported, "session does not support live messaging")
	}
	return sess, nil
}

func DiscoverClaude(ctx context.Context, homes []string, timeout time.Duration) ([]model.Candidate, error) {
	a := claude.New()
	return a.Discover(ctx, adapter.DiscoverRequest{Homes: homes, Timeout: timeout})
}

func DiscoverCodex(ctx context.Context, home string, timeout time.Duration) ([]model.Candidate, error) {
	a := codex.New()
	return a.Discover(ctx, adapter.DiscoverRequest{CodexHome: home, Timeout: timeout})
}

// DiscoverQwen reads the documented cross-session registry. It never dials a
// socket and never returns credentials.
func DiscoverQwen(ctx context.Context, home string, timeout time.Duration) ([]model.Candidate, error) {
	a := qwen.New()
	return a.Discover(ctx, adapter.DiscoverRequest{QwenHome: home, Timeout: timeout})
}

// DiscoverOpenCode lists sessions on an explicitly supplied loopback server.
func DiscoverOpenCode(ctx context.Context, endpoint string, timeout time.Duration) ([]model.Candidate, error) {
	a := opencode.New()
	return a.Discover(ctx, adapter.DiscoverRequest{Endpoint: endpoint, Timeout: timeout})
}

// CheckoutIDs distinguishes a worktree checkout from the shared Git repository.
func CheckoutIDs(cwd string) (checkoutID, repoID string) {
	canon, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		canon = cwd
	}
	if abs, err := filepath.Abs(canon); err == nil {
		canon = abs
	}
	checkoutID = hash(canon)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "-C", canon, "rev-parse", "--git-common-dir").Output(); err == nil {
		common := strings.TrimSpace(string(out))
		if !filepath.IsAbs(common) {
			common = filepath.Join(canon, common)
		}
		if abs, err := filepath.Abs(common); err == nil {
			common = abs
		}
		repoID = hash(common)
	}
	return checkoutID, repoID
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// CurrentClaudeSessionID returns the native Claude session ID when amgo is
// invoked inside a Claude session.
func CurrentClaudeSessionID() string { return os.Getenv("CLAUDE_CODE_SESSION_ID") }
