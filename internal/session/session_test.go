package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"amgo/internal/model"
	"amgo/internal/store"
)

func TestCheckoutIDsDistinguishWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	wt := filepath.Join(root, "wt")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", repo)
	run("-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "init")
	run("-C", repo, "worktree", "add", "-q", wt, "-b", "wt")

	repoCheckout, repoRepo := CheckoutIDs(repo)
	wtCheckout, wtRepo := CheckoutIDs(wt)
	if repoCheckout == wtCheckout {
		t.Fatalf("worktree checkout IDs collide: %s", repoCheckout)
	}
	if repoRepo == "" || wtRepo == "" || repoRepo != wtRepo {
		t.Fatalf("repo IDs: %q vs %q", repoRepo, wtRepo)
	}

	sub := filepath.Join(repo, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	subCheckout, subRepo := CheckoutIDs(sub)
	if subCheckout != repoCheckout {
		t.Fatalf("subdirectory checkout ID differs: %s vs %s", subCheckout, repoCheckout)
	}
	if subRepo != repoRepo {
		t.Fatalf("subdirectory repo ID differs: %s vs %s", subRepo, repoRepo)
	}
}

func TestRejoinSameAliasRetargetsEndpoint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	target := model.Target{Adapter: model.AdapterOpenCode, NativeSessionID: "ses_1", Endpoint: "http://127.0.0.1:4000"}
	first, err := Join(st, JoinRequest{
		Alias: "worker", Candidate: model.Candidate{Target: target, Live: true},
		Capabilities: model.Capabilities{LiveMessaging: true}, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAcceptUnknownDelivery(first.ID, true); err != nil {
		t.Fatal(err)
	}
	next := target
	next.Endpoint = "http://127.0.0.1:4001"
	second, err := Join(st, JoinRequest{
		Alias: "worker", Candidate: model.Candidate{Target: next, Live: true},
		Capabilities: model.Capabilities{LiveMessaging: true}, Now: now,
	})
	if err != nil {
		t.Fatalf("retarget failed: %v", err)
	}
	if second.ID != first.ID || second.Target.Endpoint != next.Endpoint {
		t.Fatalf("retargeted session=%+v first=%s", second, first.ID)
	}
	if second.AcceptUnknownDelivery {
		t.Fatal("endpoint change kept the delivery-risk acceptance")
	}
	all, err := st.ListSessions(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("sessions=%+v", all)
	}
	// An unrelated target under the same alias stays a loud conflict rather
	// than silently rebinding the alias.
	other := model.Target{Adapter: model.AdapterClaude, NativeSessionID: "other"}
	if _, err := Join(st, JoinRequest{
		Alias: "worker", Candidate: model.Candidate{Target: other, Live: true},
		Capabilities: model.Capabilities{LiveMessaging: true}, Now: now,
	}); model.Code(err) != model.ErrAmbiguousTarget {
		t.Fatalf("unrelated retarget err=%v", err)
	}
}

func TestValidAlias(t *testing.T) {
	for _, ok := range []string{"a", "architect", "codex-1", "A_b"} {
		if !ValidAlias(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a b", "a/b", "a.b", "é"} {
		if ValidAlias(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestRejoinClearsAcceptanceWhenTargetChanges(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	target := model.Target{Adapter: "fake", HostID: "test", ConfigHome: "/h", NativeSessionID: "n", PID: 1, SocketPath: "/s", ProcStart: "a"}
	cand := model.Candidate{Target: target, Live: true}
	sess, err := Join(st, JoinRequest{Alias: "w", Candidate: cand, Capabilities: model.Capabilities{LiveMessaging: true}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAcceptUnknownDelivery(sess.ID, true); err != nil {
		t.Fatal(err)
	}
	next := cand
	next.Target.PID = 2
	if _, err := Join(st, JoinRequest{Alias: "w", Candidate: next, Capabilities: model.Capabilities{LiveMessaging: true}, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AcceptUnknownDelivery {
		t.Fatal("acceptance survived a process change")
	}
	if err := st.SetAcceptUnknownDelivery(sess.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(st, JoinRequest{Alias: "w", Candidate: next, Capabilities: model.Capabilities{LiveMessaging: true}, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSession(sess.ID)
	if !got.AcceptUnknownDelivery {
		t.Fatal("unchanged target lost its acceptance")
	}
}
