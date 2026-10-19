// Package cli implements the amgo command surface. Human and agent use share
// the same commands and the same versioned JSON envelope.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/adapter/claude"
	"amgo/internal/adapter/codex"
	"amgo/internal/config"
	"amgo/internal/contract"
	"amgo/internal/doctor"
	"amgo/internal/message"
	"amgo/internal/model"
	"amgo/internal/render"
	"amgo/internal/session"
	"amgo/internal/store"
	"amgo/internal/version"
)

// Version is the release version shared with adapters and tests.
var Version = version.Version

type App struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	Home     string
	JSON     bool
	Verbose  bool
	NoColor  bool
	Timeout  time.Duration
	Adapters map[string]adapter.Adapter
	Svc      *message.Service
}

func Run(args []string, in io.Reader, out, errOut io.Writer) int {
	global, rest, gerr := parseGlobals(args)
	if gerr != nil {
		if global.json || wantsJSON(args) {
			_ = contract.WriteJSON(out, contract.Fail("startup", model.WrapError(model.ErrUsage, "invalid global flags", gerr), nil))
		} else {
			fmt.Fprintln(errOut, "error:", gerr)
		}
		return 2
	}
	app := &App{
		In:      in,
		Out:     out,
		Err:     errOut,
		JSON:    global.json,
		Verbose: global.verbose,
		NoColor: global.noColor,
		Timeout: global.timeout,
	}
	if app.Timeout <= 0 {
		app.Timeout = 60 * time.Second
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
		app.usage()
		if len(rest) == 0 {
			return 2
		}
		return 0
	}
	home, herr := config.Home(global.home)
	if herr != nil {
		return app.fail("startup", model.WrapError(model.ErrUsage, "cannot resolve the state home", herr), nil)
	}
	app.Home = home
	app.Adapters = map[string]adapter.Adapter{
		model.AdapterClaude: claude.New(),
		model.AdapterCodex:  codex.New(),
	}
	app.Svc = message.New(app.Home, app.Adapters)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch rest[0] {
	case "version":
		return app.cmdVersion()
	case "doctor":
		return app.cmdDoctor(ctx, rest[1:])
	case "adapters":
		return app.cmdAdapters(ctx)
	case "sessions":
		return app.cmdSessions(ctx, rest[1:])
	case "join":
		return app.cmdJoin(ctx, rest[1:])
	case "leave":
		return app.cmdLeave(rest[1:])
	case "send":
		return app.cmdSend(ctx, rest[1:])
	case "retry":
		return app.cmdRetry(ctx, rest[1:])
	case "reply":
		return app.cmdReply(ctx, rest[1:])
	case "inbox":
		return app.cmdInbox(rest[1:])
	case "read":
		return app.cmdRead(rest[1:])
	case "ack":
		return app.cmdAck(ctx, rest[1:])
	case "wait":
		return app.cmdWait(ctx, rest[1:])
	case "conversation":
		return app.cmdConversation(ctx, rest[1:])
	case "export":
		return app.cmdExport(rest[1:])
	case "prune":
		return app.cmdPrune(rest[1:])
	case "recover":
		return app.cmdRecover(rest[1:])
	default:
		return app.fail("unknown command", model.NewError(model.ErrUsage, fmt.Sprintf("unknown command %q", rest[0])), map[string]any{"command": rest[0]})
	}
}

func (a *App) usage() {
	fmt.Fprint(a.Err, `amgo — native coding-agent session messaging

Usage:
  amgo [global flags] <command> [command flags]

Commands:
  version      build/version; no provider work
  doctor       capability probes and read-only state invariants
  adapters     adapter capability matrix
  sessions     registered sessions; --discover for native candidates
  join         register a live Claude or Codex session
  leave        remove an amgo registration
  send         send a message to a registered session
  retry        resend an unknown outcome; requires duplicate-risk acceptance
  reply        reply to a message
  inbox        list local messages
  read         inspect a message, attempts and events
  ack          explicitly acknowledge a message
  wait         bounded wait for acknowledgement or reply
  conversation create|show|close
  export       export messages as JSONL
  prune        retention dry-run or delete
  recover      reconcile dead attempts and import the outcome journal

Global flags (accepted before or after the command):
  --json --home <dir> --timeout <dur> --verbose --no-color
`)
}

type globalFlags struct {
	json    bool
	home    string
	timeout time.Duration
	verbose bool
	noColor bool
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" || arg == "-json" {
			return true
		}
	}
	return false
}

// commandValueFlags names command flags that consume the following argument.
// The global pre-parser must copy their values verbatim; otherwise a body such
// as "--json" would be stolen from the command.
var commandValueFlags = map[string]bool{
	"probe": true, "codex-home": true, "config-home": true, "name": true, "session": true,
	"cwd": true, "thread": true, "file": true, "text": true, "as": true,
	"conversation": true, "idempotency-key": true, "limit": true,
	"until": true, "max-messages": true, "expires": true, "format": true,
}

func parseGlobals(args []string) (globalFlags, []string, error) {
	var g globalFlags
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if len(rest) == 0 {
				rest = append(rest, args[i+1:]...)
			} else {
				rest = append(rest, args[i:]...)
			}
			break
		}
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		if strings.HasPrefix(arg, "-") && commandValueFlags[name] {
			rest = append(rest, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		switch {
		case arg == "--json" || arg == "-json":
			g.json = true
		case arg == "--verbose" || arg == "-verbose":
			g.verbose = true
		case arg == "--no-color" || arg == "-no-color":
			g.noColor = true
		case arg == "--home" || arg == "-home":
			if i+1 >= len(args) {
				return g, nil, fmt.Errorf("--home requires a value")
			}
			i++
			g.home = args[i]
		case strings.HasPrefix(arg, "--home=") || strings.HasPrefix(arg, "-home="):
			g.home = strings.SplitN(arg, "=", 2)[1]
		case arg == "--timeout" || arg == "-timeout":
			if i+1 >= len(args) {
				return g, nil, fmt.Errorf("--timeout requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return g, nil, err
			}
			g.timeout = d
		case strings.HasPrefix(arg, "--timeout=") || strings.HasPrefix(arg, "-timeout="):
			d, err := time.ParseDuration(strings.SplitN(arg, "=", 2)[1])
			if err != nil {
				return g, nil, err
			}
			g.timeout = d
		default:
			rest = append(rest, arg)
		}
	}
	return g, rest, nil
}

func parseCommand(fs *flag.FlagSet, args []string, takesValue map[string]bool) ([]string, error) {
	var flagArgs, pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
			name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
			if !strings.Contains(arg, "=") && takesValue[name] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		pos = append(pos, arg)
	}
	return pos, fs.Parse(flagArgs)
}

func (a *App) ok(op string, data any, human func()) int {
	if a.JSON {
		if err := contract.WriteJSON(a.Out, contract.OK(op, data)); err != nil {
			fmt.Fprintln(a.Err, "error: writing JSON output:", err)
			return 1
		}
		return 0
	}
	if human != nil {
		human()
	}
	return 0
}

func (a *App) fail(op string, err error, data any) int {
	if a.JSON {
		_ = contract.WriteJSON(a.Out, contract.Fail(op, err, data))
	} else {
		fmt.Fprintln(a.Err, "error:", render.Safe(err.Error()))
	}
	return contract.ExitCode(err)
}

// failSend reports the reconciliation identifiers on every send failure. A
// caller that has to recover an unknown outcome needs them even when the
// message record itself could not be written.
func (a *App) failSend(op string, err error, out message.SendOutput, recipient, key string) int {
	data := sendData(out)
	data["recipient"] = recipient
	if key == "" {
		key = out.IdempotencyKey
	}
	data["idempotency_key"] = key
	if !a.JSON {
		var parts []string
		add := func(name, value string) {
			if value != "" {
				parts = append(parts, name+"="+render.Safe(value))
			}
		}
		add("key", key)
		add("message", out.Message.ID)
		add("conversation", out.Message.ConversationID)
		add("attempt", out.Attempt.ID)
		add("reservation", out.Reservation.ID)
		add("submission", out.Submission)
		if len(parts) > 0 {
			fmt.Fprintln(a.Err, "diagnostics:", strings.Join(parts, " "))
		}
	}
	return a.fail(op, err, data)
}

func (a *App) maybeRecover() error {
	_, err := a.Svc.RecoverBatch(32)
	return err
}

// ---- version / doctor ----

func (a *App) cmdVersion() int {
	return a.ok("version", map[string]any{"version": Version, "schema_version": contract.SchemaVersion}, func() {
		fmt.Fprintln(a.Out, "amgo", Version)
	})
}

func (a *App) cmdDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	state := fs.Bool("state", false, "include state invariants")
	probe := fs.String("probe", "", "send a nonce to an alias and wait for an explicit acknowledgement")
	pos, err := parseCommand(fs, args, map[string]bool{"probe": true})
	if err != nil {
		return a.fail("doctor", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	_ = pos
	if *probe != "" {
		res, err := doctor.ProbeSession(ctx, a.Svc, *probe, a.Timeout)
		if err != nil {
			return a.fail("doctor probe", err, map[string]any{"message_id": res.Message.ID, "state": res.Message.State})
		}
		return a.ok("doctor probe", map[string]any{"message_id": res.Message.ID, "state": res.Message.State, "acknowledged": true}, func() {
			fmt.Fprintf(a.Out, "probe acknowledged: %s\n", res.Message.ID)
		})
	}
	rep, err := doctor.Run(ctx, a.Home, Version, a.Adapters)
	if err != nil {
		return a.fail("doctor", err, nil)
	}
	_ = state
	return a.ok("doctor", rep, func() {
		fmt.Fprintf(a.Out, "amgo %s  go=%s  host=%s\n", rep.Version, rep.Go, rep.Host)
		fmt.Fprintf(a.Out, "home: %s\n", rep.Home)
		if !rep.State.Initialized {
			fmt.Fprintln(a.Out, "state: not initialized")
		} else if rep.State.OK {
			fmt.Fprintln(a.Out, "state: ok")
		} else {
			fmt.Fprintf(a.Out, "state: %s\n", rep.State.Error)
			for _, v := range rep.State.Violations {
				fmt.Fprintf(a.Out, "  %s: %s\n", v.Code, v.Detail)
			}
		}
		for _, c := range rep.Adapters {
			fmt.Fprintf(a.Out, "%-8s maturity=%s live=%v receipts=%v %s\n", c.Adapter, c.Maturity, c.LiveMessaging, c.NativeReceipts, render.Safe(strings.Join(c.Constraints, "; ")))
		}
	})
}

func (a *App) cmdAdapters(ctx context.Context) int {
	caps := doctor.AdapterCapabilities(ctx, a.Adapters)
	return a.ok("adapters", map[string]any{"adapters": caps}, func() {
		for _, c := range caps {
			fmt.Fprintf(a.Out, "%-8s maturity=%s live=%v receipts=%v %s\n", c.Adapter, c.Maturity, c.LiveMessaging, c.NativeReceipts, render.Safe(strings.Join(c.Constraints, "; ")))
		}
	})
}

// ---- sessions / join / leave ----

func (a *App) cmdSessions(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	discover := fs.Bool("discover", false, "include native candidates")
	all := fs.Bool("all", false, "include unregistered sessions")
	var homes stringList
	fs.Var(&homes, "config-home", "Claude config home (repeatable)")
	codexHome := fs.String("codex-home", "", "Codex config home")
	_, err := parseCommand(fs, args, map[string]bool{"config-home": true, "codex-home": true})
	if err != nil {
		return a.fail("sessions", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	var registered []model.Session
	statErr := error(nil)
	if _, statErr = os.Stat(config.DBPath(a.Home)); statErr == nil {
		st, err := store.OpenReadOnly(config.DBPath(a.Home), 2*time.Second)
		if err != nil {
			return a.fail("sessions", err, nil)
		}
		registered, err = st.ListSessions(*all)
		_ = st.Close()
		if err != nil {
			return a.fail("sessions", err, nil)
		}
	} else if !os.IsNotExist(statErr) {
		return a.fail("sessions", model.WrapError(model.ErrInternal, "cannot inspect state", statErr), nil)
	}
	type row struct {
		Alias      string `json:"alias"`
		Adapter    string `json:"adapter"`
		NativeID   string `json:"native_session_id"`
		Status     string `json:"status"`
		Registered bool   `json:"registered"`
		Active     bool   `json:"active"`
		Reachable  *bool  `json:"reachable"`
		Reason     string `json:"reason,omitempty"`
	}
	rows := make([]row, 0, len(registered))
	reg := map[string]bool{}
	for _, s := range registered {
		reg[s.Target.Adapter+"\x00"+s.Target.ConfigHome+"\x00"+s.Target.NativeSessionID] = true
		rows = append(rows, row{Alias: s.Alias, Adapter: s.Target.Adapter, NativeID: s.Target.NativeSessionID, Status: s.Target.Status, Registered: true, Active: s.UnregisteredAt == nil})
	}
	if *discover {
		if cands, err := session.DiscoverClaude(ctx, config.ClaudeHomes(homes), a.Timeout); err == nil {
			for _, c := range cands {
				if reg[c.Target.Adapter+"\x00"+c.Target.ConfigHome+"\x00"+c.Target.NativeSessionID] {
					continue
				}
				reachable := c.Live
				rows = append(rows, row{Alias: c.Name, Adapter: c.Adapter, NativeID: c.NativeSessionID, Status: c.Status, Reachable: &reachable, Reason: c.Reason})
			}
		} else {
			fmt.Fprintln(a.Err, "warning: claude discovery:", err)
		}
		if cands, err := session.DiscoverCodex(ctx, config.CodexHome(*codexHome), a.Timeout); err == nil {
			for _, c := range cands {
				if reg[c.Target.Adapter+"\x00"+c.Target.ConfigHome+"\x00"+c.Target.NativeSessionID] {
					continue
				}
				if !c.Live && !*all {
					continue
				}
				reachable := c.Live
				rows = append(rows, row{Alias: c.Name, Adapter: c.Adapter, NativeID: c.NativeSessionID, Status: c.Status, Reachable: &reachable, Reason: c.Reason})
			}
		} else {
			fmt.Fprintln(a.Err, "warning: codex discovery:", err)
		}
	}
	return a.ok("sessions", map[string]any{"sessions": rows}, func() {
		for _, r := range rows {
			state := "candidate"
			if r.Registered {
				if r.Active {
					state = "registered-active"
				} else {
					state = "registered-inactive"
				}
			}
			reach := ""
			if r.Reachable != nil {
				if *r.Reachable {
					reach = "reachable"
				} else {
					reach = "not-reachable"
				}
			}
			extra := ""
			if r.Reason != "" {
				extra = " (" + r.Reason + ")"
			}
			fmt.Fprintf(a.Out, "%-8s %-20s %-14s %-18s %s%s\n", render.Safe(r.Alias), r.Adapter, render.Safe(r.NativeID), state, reach, render.Safe(r.Status+" "+extra))
		}
	})
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func (a *App) cmdJoin(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail("join", model.NewError(model.ErrUsage, "join requires claude or codex"), nil)
	}
	if err := a.maybeRecover(); err != nil {
		return a.fail("join", err, nil)
	}
	kind := args[0]
	rest := args[1:]
	switch kind {
	case "claude":
		fs := flag.NewFlagSet("join claude", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		name := fs.String("name", "", "alias")
		sid := fs.String("session", "", "native Claude session ID")
		cwdFlag := fs.String("cwd", "", "checkout directory")
		var homes stringList
		fs.Var(&homes, "config-home", "Claude config home (repeatable)")
		_, err := parseCommand(fs, rest, map[string]bool{"name": true, "session": true, "config-home": true, "cwd": true})
		if err != nil {
			return a.fail("join", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
		}
		if *name == "" {
			return a.fail("join", model.NewError(model.ErrUsage, "--name is required"), nil)
		}
		want := *sid
		if want == "" {
			want = session.CurrentClaudeSessionID()
		}
		cands, err := session.DiscoverClaude(ctx, config.ClaudeHomes(homes), a.Timeout)
		if err != nil {
			return a.fail("join", err, nil)
		}
		var pick *model.Candidate
		for i := range cands {
			if want != "" {
				if cands[i].NativeSessionID == want {
					pick = &cands[i]
					break
				}
				continue
			}
			if cands[i].Live {
				if pick != nil {
					return a.fail("join", model.NewError(model.ErrAmbiguousTarget, "multiple live Claude sessions; pass --session"), nil)
				}
				pick = &cands[i]
			}
		}
		if pick == nil {
			return a.fail("join", model.NewError(model.ErrTargetUnavailable, "no matching live Claude session"), nil)
		}
		caps, err := a.Adapters[model.AdapterClaude].Probe(ctx)
		if err != nil || !caps.LiveMessaging {
			caps = observedClaudeCapabilities(pick.Target.Version)
		}
		setCheckout(&pick.Target, *cwdFlag)
		st, err := store.Open(config.DBPath(a.Home), 2*time.Second)
		if err != nil {
			return a.fail("join", err, nil)
		}
		defer st.Close()
		joined, err := session.Join(st, session.JoinRequest{Alias: *name, Candidate: *pick, Capabilities: caps, Now: time.Now().UTC()})
		if err != nil {
			return a.fail("join", err, nil)
		}
		return a.ok("join", joined, func() {
			fmt.Fprintf(a.Out, "joined %s -> %s (%s)\n", render.Safe(joined.Alias), joined.ID, render.Safe(joined.Target.NativeSessionID))
		})
	case "codex":
		fs := flag.NewFlagSet("join codex", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		name := fs.String("name", "", "alias")
		threadID := fs.String("thread", "", "thread ID")
		codexHome := fs.String("codex-home", "", "Codex config home")
		cwdFlag := fs.String("cwd", "", "checkout directory")
		_, err := parseCommand(fs, rest, map[string]bool{"name": true, "thread": true, "codex-home": true, "cwd": true})
		if err != nil {
			return a.fail("join", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
		}
		if *name == "" || *threadID == "" {
			return a.fail("join", model.NewError(model.ErrUsage, "--name and --thread are required"), nil)
		}
		cands, err := session.DiscoverCodex(ctx, config.CodexHome(*codexHome), a.Timeout)
		if err != nil {
			return a.fail("join", err, nil)
		}
		var pick *model.Candidate
		for i := range cands {
			if cands[i].NativeSessionID == *threadID {
				pick = &cands[i]
				break
			}
		}
		if pick == nil {
			return a.fail("join", model.NewError(model.ErrTargetUnavailable, "thread not found in the shared daemon"), nil)
		}
		if !pick.Live {
			return a.fail("join", model.NewError(model.ErrTargetNotLive, pick.Reason), nil)
		}
		caps, err := a.Adapters[model.AdapterCodex].Probe(ctx)
		if err != nil || !caps.LiveMessaging {
			caps = observedCodexCapabilities(pick.Target.Version)
		}
		setCheckout(&pick.Target, *cwdFlag)
		st, err := store.Open(config.DBPath(a.Home), 2*time.Second)
		if err != nil {
			return a.fail("join", err, nil)
		}
		defer st.Close()
		joined, err := session.Join(st, session.JoinRequest{Alias: *name, Candidate: *pick, Capabilities: caps, Now: time.Now().UTC()})
		if err != nil {
			return a.fail("join", err, nil)
		}
		return a.ok("join", joined, func() {
			fmt.Fprintf(a.Out, "joined %s -> %s (%s)\n", render.Safe(joined.Alias), joined.ID, render.Safe(joined.Target.NativeSessionID))
		})
	default:
		return a.fail("join", model.NewError(model.ErrUsage, "join requires claude or codex"), nil)
	}
}

func (a *App) cmdLeave(args []string) int {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	pos, err := parseCommand(fs, args, nil)
	if err != nil || len(pos) != 1 {
		return a.fail("leave", model.NewError(model.ErrUsage, "usage: amgo leave <alias>"), nil)
	}
	if err := a.maybeRecover(); err != nil {
		return a.fail("leave", err, nil)
	}
	st, err := store.Open(config.DBPath(a.Home), 2*time.Second)
	if err != nil {
		return a.fail("leave", err, nil)
	}
	defer st.Close()
	sess, err := session.Leave(st, pos[0], time.Now().UTC())
	if err != nil {
		return a.fail("leave", err, nil)
	}
	return a.ok("leave", sess, func() { fmt.Fprintf(a.Out, "left %s\n", sess.Alias) })
}

// ---- send / reply ----

func (a *App) readBody(ctx context.Context, stdin, file, text string) (string, error) {
	n := 0
	if stdin != "" {
		n++
	}
	if file != "" {
		n++
	}
	if text != "" {
		n++
	}
	_ = stdin
	if n != 1 {
		return "", model.NewError(model.ErrUsage, "exactly one of --stdin, --file or --text is required")
	}
	var b []byte
	var err error
	switch {
	case file != "":
		f, ferr := os.Open(file)
		if ferr != nil {
			return "", model.WrapError(model.ErrUsage, "cannot read message file", ferr)
		}
		defer f.Close()
		b, err = io.ReadAll(io.LimitReader(f, message.MaxBodyBytes+1))
	case text != "":
		b = []byte(text)
	default:
		type readResult struct {
			b   []byte
			err error
		}
		ch := make(chan readResult, 1)
		go func() {
			data, rerr := io.ReadAll(io.LimitReader(a.In, message.MaxBodyBytes+1))
			ch <- readResult{b: data, err: rerr}
		}()
		select {
		case r := <-ch:
			b, err = r.b, r.err
		case <-ctx.Done():
			return "", model.WrapError(model.ErrInterrupted, "interrupted while reading the message body", ctx.Err())
		}
	}
	if err != nil {
		return "", model.WrapError(model.ErrUsage, "cannot read message body", err)
	}
	if len(b) > message.MaxBodyBytes {
		return "", model.NewError(model.ErrUsage, "message body exceeds 64 KiB")
	}
	if strings.IndexByte(string(b), 0) >= 0 {
		return "", model.NewError(model.ErrUsage, "message body contains NUL")
	}
	if !isValidUTF8(b) {
		return "", model.NewError(model.ErrUsage, "message body is not valid UTF-8")
	}
	return string(b), nil
}

func isValidUTF8(b []byte) bool {
	return strings.ToValidUTF8(string(b), "�") == string(b)
}

func (a *App) cmdSend(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	useStdin := fs.Bool("stdin", false, "read the body from stdin")
	file := fs.String("file", "", "read the body from a file")
	text := fs.String("text", "", "inline body")
	as := fs.String("as", "", "sender alias")
	conv := fs.String("conversation", "", "named conversation")
	key := fs.String("idempotency-key", "", "idempotency key")
	expect := fs.Bool("expect-reply", false, "append a reply hint")
	accept := fs.Bool("accept-unknown-delivery", false, "accept that Claude holds/refusals are unobservable")
	pos, err := parseCommand(fs, args, map[string]bool{"file": true, "text": true, "as": true, "conversation": true, "idempotency-key": true})
	if err != nil {
		return a.fail("send", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("send", model.NewError(model.ErrUsage, "usage: amgo send <alias> [--stdin|--file|--text]"), nil)
	}
	src := ""
	if *useStdin {
		src = "stdin"
	}
	body, err := a.readBody(ctx, src, *file, *text)
	if err != nil {
		return a.fail("send", err, nil)
	}
	if err := a.maybeRecover(); err != nil {
		return a.fail("send", err, nil)
	}
	out, err := a.Svc.Send(ctx, message.SendInput{
		RecipientAlias:        pos[0],
		SenderAlias:           *as,
		ConversationName:      *conv,
		Body:                  body,
		IdempotencyKey:        *key,
		ExpectReply:           *expect,
		AcceptUnknownDelivery: *accept,
	})
	if err != nil {
		return a.failSend("send", err, out, pos[0], *key)
	}
	return a.ok("send", sendData(out), func() {
		dup := ""
		if out.Duplicate {
			dup = " duplicate"
		}
		fmt.Fprintf(a.Out, "sent %s -> %s (%s, native %s)%s key=%s\n", out.Message.ID, render.Safe(pos[0]), out.Submission, out.NativeDisposition, dup, render.Safe(out.IdempotencyKey))
	})
}

func (a *App) cmdRetry(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	as := fs.String("as", "", "sender alias")
	key := fs.String("idempotency-key", "", "idempotency key for the retry")
	accept := fs.Bool("accept-duplicate-risk", false, "accept that the original submission may already have reached the target")
	acceptUnknown := fs.Bool("accept-unknown-delivery", false, "accept that Claude holds and refusals are unobservable")
	pos, err := parseCommand(fs, args, map[string]bool{"as": true, "idempotency-key": true})
	if err != nil {
		return a.fail("retry", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("retry", model.NewError(model.ErrUsage, "usage: amgo retry <message-id> --accept-duplicate-risk"), nil)
	}
	if err := a.maybeRecover(); err != nil {
		return a.fail("retry", err, nil)
	}
	out, err := a.Svc.Retry(ctx, message.RetryInput{
		MessageID:             pos[0],
		SenderAlias:           *as,
		AcceptDuplicateRisk:   *accept,
		AcceptUnknownDelivery: *acceptUnknown,
		IdempotencyKey:        *key,
	})
	if err != nil {
		return a.failSend("retry", err, out, "", *key)
	}
	return a.ok("retry", sendData(out), func() {
		fmt.Fprintf(a.Out, "retried %s (retry of %s) (%s, native %s) duplicate risk accepted key=%s\n",
			out.Message.ID, out.Message.RetryOf, out.Submission, out.NativeDisposition, render.Safe(out.IdempotencyKey))
	})
}

func sendData(out message.SendOutput) map[string]any {
	return map[string]any{
		"message_id":           out.Message.ID,
		"conversation_id":      out.Message.ConversationID,
		"attempt_id":           out.Attempt.ID,
		"reservation_id":       out.Reservation.ID,
		"submission":           out.Submission,
		"native_disposition":   out.NativeDisposition,
		"native_submission_id": out.NativeSubmissionID,
		"retry_of":             out.Message.RetryOf,
		"retried_by":           out.Message.RetriedBy,
		"acknowledged":         out.Message.State == model.MessageAcknowledged,
		"duplicate":            out.Duplicate,
		"message_pruned":       out.MessagePruned,
		"idempotency_key":      out.IdempotencyKey,
		"evidence":             out.Evidence,
	}
}

func (a *App) cmdReply(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	useStdin := fs.Bool("stdin", false, "read the body from stdin")
	file := fs.String("file", "", "read the body from a file")
	text := fs.String("text", "", "inline body")
	as := fs.String("as", "", "sender alias")
	key := fs.String("idempotency-key", "", "idempotency key")
	expect := fs.Bool("expect-reply", false, "append a reply hint")
	accept := fs.Bool("accept-unknown-delivery", false, "accept that Claude holds/refusals are unobservable")
	pos, err := parseCommand(fs, args, map[string]bool{"file": true, "text": true, "as": true, "idempotency-key": true})
	if err != nil || len(pos) != 1 {
		return a.fail("reply", model.NewError(model.ErrUsage, "usage: amgo reply <message-id> [--stdin|--file|--text]"), nil)
	}
	src := ""
	if *useStdin {
		src = "stdin"
	}
	body, err := a.readBody(ctx, src, *file, *text)
	if err != nil {
		return a.fail("reply", err, nil)
	}
	if err := a.maybeRecover(); err != nil {
		return a.fail("reply", err, nil)
	}
	out, err := a.Svc.Reply(ctx, message.ReplyInput{
		MessageID:             pos[0],
		SenderAlias:           *as,
		Body:                  body,
		IdempotencyKey:        *key,
		ExpectReply:           *expect,
		AcceptUnknownDelivery: *accept,
	})
	if err != nil {
		return a.failSend("reply", err, out, pos[0], *key)
	}
	return a.ok("reply", sendData(out), func() {
		fmt.Fprintf(a.Out, "replied %s -> %s (%s, native %s) key=%s\n", out.Message.ID, render.Safe(pos[0]), out.Submission, out.NativeDisposition, render.Safe(out.IdempotencyKey))
	})
}

// ---- inbox / read / ack / wait ----

func (a *App) cmdInbox(args []string) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	sessionAlias := fs.String("session", "", "session alias")
	limit := fs.Int("limit", 20, "page size")
	_, err := parseCommand(fs, args, map[string]bool{"session": true, "limit": true})
	if err != nil {
		return a.fail("inbox", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	msgs, err := a.Svc.Inbox(*sessionAlias, *limit)
	if err != nil {
		return a.fail("inbox", err, nil)
	}
	return a.ok("inbox", map[string]any{"messages": msgs}, func() {
		for _, m := range msgs {
			fmt.Fprintf(a.Out, "%s  %s  %-12s %s\n", m.ID, m.CreatedAt.Format(time.RFC3339), m.State, firstLine(m.Body))
		}
	})
}

func (a *App) cmdRead(args []string) int {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	pos, err := parseCommand(fs, args, nil)
	if err != nil || len(pos) != 1 {
		return a.fail("read", model.NewError(model.ErrUsage, "usage: amgo read <message-id>"), nil)
	}
	rec, err := a.Svc.Read(pos[0])
	if err != nil {
		return a.fail("read", err, nil)
	}
	return a.ok("read", rec, func() {
		fmt.Fprintf(a.Out, "message %s  state=%s  native=%s\n", rec.Message.ID, rec.Message.State, rec.Message.NativeDisposition)
		fmt.Fprintf(a.Out, "from %s to %s  conversation %s\n", render.Safe(rec.Message.SenderLabel), render.Safe(rec.Message.RecipientSessionID), rec.Message.ConversationID)
		fmt.Fprintf(a.Out, "created %s  expires %s\n", rec.Message.CreatedAt.Format(time.RFC3339), rec.Message.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintln(a.Out, "---")
		fmt.Fprintln(a.Out, render.Safe(rec.Message.Body))
		fmt.Fprintln(a.Out, "--- events")
		for _, ev := range rec.Events {
			fmt.Fprintf(a.Out, "%d  %s  %s\n", ev.Seq, ev.At.Format(time.RFC3339), ev.Kind)
		}
		if rec.Message.NativeDisposition == model.DispositionUnknown {
			fmt.Fprintln(a.Out, "note: the native delivery outcome is unknown; the recipient may not have seen this message")
		}
	})
}

func (a *App) cmdAck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	as := fs.String("as", "", "acknowledging alias")
	pos, err := parseCommand(fs, args, map[string]bool{"as": true})
	if err != nil || len(pos) != 1 {
		return a.fail("ack", model.NewError(model.ErrUsage, "usage: amgo ack <message-id> [--as <alias>]"), nil)
	}
	if err := a.maybeRecover(); err != nil {
		return a.fail("ack", err, nil)
	}
	msg, receipts, err := a.Svc.Ack(ctx, message.AckInput{MessageID: pos[0], AsAlias: *as})
	if err != nil {
		return a.fail("ack", err, nil)
	}
	return a.ok("ack", map[string]any{"message": msg, "receipts": receipts}, func() {
		fmt.Fprintf(a.Out, "acknowledged %s\n", msg.ID)
	})
}

func (a *App) cmdWait(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	until := fs.String("until", "acknowledged", "acknowledged or replied")
	pos, err := parseCommand(fs, args, map[string]bool{"until": true})
	if err != nil || len(pos) != 1 {
		return a.fail("wait", model.NewError(model.ErrUsage, "usage: amgo wait <message-id> --until acknowledged|replied"), nil)
	}
	res, err := a.Svc.Wait(ctx, message.WaitInput{MessageID: pos[0], Until: *until, Timeout: a.Timeout})
	if err != nil {
		return a.fail("wait", err, map[string]any{"message_id": res.Message.ID, "state": res.Message.State})
	}
	return a.ok("wait", res, func() {
		fmt.Fprintf(a.Out, "%s  %s\n", res.Message.ID, res.Message.State)
	})
}

// ---- conversations ----

func (a *App) cmdConversation(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail("conversation", model.NewError(model.ErrUsage, "conversation create|show|close"), nil)
	}
	sub := args[0]
	switch sub {
	case "create":
		fs := flag.NewFlagSet("conversation create", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		max := fs.Int("max-messages", 12, "message budget")
		expires := fs.Duration("expires", time.Hour, "conversation lifetime")
		pos, err := parseCommand(fs, args[1:], map[string]bool{"max-messages": true, "expires": true})
		if err != nil || len(pos) != 1 {
			return a.fail("conversation create", model.NewError(model.ErrUsage, "usage: amgo conversation create <name> --max-messages N --expires DUR"), nil)
		}
		if err := a.maybeRecover(); err != nil {
			return a.fail("conversation create", err, nil)
		}
		c, err := a.Svc.CreateConversation(pos[0], *max, *expires)
		if err != nil {
			return a.fail("conversation create", err, nil)
		}
		return a.ok("conversation create", c, func() { fmt.Fprintf(a.Out, "created %s (%s)\n", render.Safe(c.Name), c.ID) })
	case "show":
		fs := flag.NewFlagSet("conversation show", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		pos, err := parseCommand(fs, args[1:], nil)
		if err != nil || len(pos) != 1 {
			return a.fail("conversation show", model.NewError(model.ErrUsage, "usage: amgo conversation show <name>"), nil)
		}
		c, msgs, err := a.Svc.ShowConversation(pos[0])
		if err != nil {
			return a.fail("conversation show", err, nil)
		}
		return a.ok("conversation show", map[string]any{"conversation": c, "messages": msgs}, func() {
			fmt.Fprintf(a.Out, "%s  %s  spent=%d refunded=%d max=%d\n", render.Safe(c.Name), c.ID, c.SpentCount, c.RefundedCount, c.MaxMessages)
			for _, m := range msgs {
				fmt.Fprintf(a.Out, "  %s  %-12s %s\n", m.ID, m.State, firstLine(m.Body))
			}
		})
	case "close":
		fs := flag.NewFlagSet("conversation close", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		pos, err := parseCommand(fs, args[1:], nil)
		if err != nil || len(pos) != 1 {
			return a.fail("conversation close", model.NewError(model.ErrUsage, "usage: amgo conversation close <name>"), nil)
		}
		if err := a.maybeRecover(); err != nil {
			return a.fail("conversation close", err, nil)
		}
		c, err := a.Svc.CloseConversation(pos[0])
		if err != nil {
			return a.fail("conversation close", err, nil)
		}
		return a.ok("conversation close", c, func() { fmt.Fprintf(a.Out, "closed %s\n", render.Safe(c.Name)) })
	default:
		return a.fail("conversation", model.NewError(model.ErrUsage, "conversation create|show|close"), nil)
	}
}

// ---- export / prune / recover ----

func (a *App) cmdExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	conv := fs.String("conversation", "", "conversation name")
	format := fs.String("format", "jsonl", "export format")
	_, err := parseCommand(fs, args, map[string]bool{"conversation": true, "format": true})
	if err != nil || *format != "jsonl" {
		return a.fail("export", model.NewError(model.ErrUsage, "usage: amgo export [--conversation NAME] --format jsonl"), nil)
	}
	if err := a.Svc.ExportJSONL(a.Out, *conv); err != nil {
		return a.fail("export", err, nil)
	}
	return 0
}

func (a *App) cmdPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	yes := fs.Bool("yes", false, "delete eligible records")
	dry := fs.Bool("dry-run", false, "show the plan without deleting")
	idem := fs.Bool("idempotency", false, "also delete old idempotency membership")
	compact := fs.Bool("compact", false, "rebuild the database after pruning")
	_, err := parseCommand(fs, args, nil)
	if err != nil {
		return a.fail("prune", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	dryRun := !*yes || *dry
	if dryRun {
		if _, statErr := os.Stat(config.DBPath(a.Home)); statErr != nil {
			if !os.IsNotExist(statErr) {
				return a.fail("prune", model.WrapError(model.ErrInternal, "cannot inspect state", statErr), nil)
			}
			rep := store.PruneReport{DryRun: true}
			return a.ok("prune", rep, func() {
				fmt.Fprintf(a.Out, "prune dry_run=true messages=0 body_bytes=0 idempotency_deleted=0 compacted=false bytes_after=0\n")
			})
		}
	} else if err := a.maybeRecover(); err != nil {
		return a.fail("prune", err, nil)
	}
	var st *store.Store
	if dryRun {
		st, err = store.OpenReadOnly(config.DBPath(a.Home), 2*time.Second)
	} else {
		st, err = store.Open(config.DBPath(a.Home), 2*time.Second)
	}
	if err != nil {
		return a.fail("prune", err, nil)
	}
	defer st.Close()
	now := time.Now().UTC()
	rep, err := st.Prune(now, now.Add(-30*24*time.Hour), dryRun, *idem)
	if err != nil {
		return a.fail("prune", err, nil)
	}
	if *compact && *yes && !*dry {
		_ = st.Close()
		size, cerr := store.Compact(config.DBPath(a.Home))
		if cerr != nil {
			return a.fail("prune", cerr, rep)
		}
		rep.Compacted = true
		rep.BytesAfter = size
	}
	return a.ok("prune", rep, func() {
		fmt.Fprintf(a.Out, "prune dry_run=%v messages=%d body_bytes=%d idempotency_deleted=%d compacted=%v bytes_after=%d\n", rep.DryRun, rep.Messages, rep.BodyBytes, rep.IdempotencyDeleted, rep.Compacted, rep.BytesAfter)
	})
}

func (a *App) cmdRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	_, err := parseCommand(fs, args, nil)
	if err != nil {
		return a.fail("recover", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	imported, jerr := a.Svc.ImportJournal()
	if jerr != nil {
		return a.fail("recover", jerr, map[string]any{"journal_imported": imported})
	}
	n, err := a.Svc.RecoverAll()
	if err != nil {
		return a.fail("recover", err, map[string]any{"reconciled": n, "journal_imported": imported})
	}
	return a.ok("recover", map[string]any{"reconciled": n, "journal_imported": imported}, func() {
		fmt.Fprintf(a.Out, "reconciled %d attempts; imported %d journal outcomes\n", n, imported)
	})
}

func setCheckout(target *model.Target, flag string) {
	if target.Cwd == "" {
		if flag != "" {
			target.Cwd = flag
		} else if wd, err := os.Getwd(); err == nil {
			target.Cwd = wd
		}
	}
	if target.Cwd != "" {
		target.CheckoutID, target.RepoID = session.CheckoutIDs(target.Cwd)
	}
}

func observedClaudeCapabilities(version string) model.Capabilities {
	return model.Capabilities{
		Adapter:         model.AdapterClaude,
		Maturity:        "observed",
		MinKnownVersion: "2.1.248",
		TestedVersions:  []string{version},
		LiveMessaging:   true,
		NativeReceipts:  false,
		Constraints:     []string{"target validated from its registry, socket and key; binary version probe unavailable"},
	}
}

func observedCodexCapabilities(version string) model.Capabilities {
	return model.Capabilities{
		Adapter:         model.AdapterCodex,
		Maturity:        "observed",
		MinKnownVersion: "0.163.0",
		TestedVersions:  []string{version},
		LiveMessaging:   true,
		NativeReceipts:  true,
		Constraints:     []string{"target validated from the shared daemon; CLI version probe unavailable"},
	}
}

func firstLine(s string) string {
	s = render.Safe(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		r := []rune(s)
		if len(r) > 80 {
			s = string(r[:80]) + "…"
		}
	}
	return s
}
