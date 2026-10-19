package session

import (
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
