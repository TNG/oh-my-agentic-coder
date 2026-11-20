//go:build e2e || e2e_fast

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The approvals E2E tests drive the compiled omac binary: `omac register`
// reads a skill's approvals block, `omac serve` and `omac start` put the
// facade gate in front of a real sidecar process, and the approval dialog
// is a fake zenity on PATH. The fake records the argv omac passes (so the
// tests read the dialog text the user would see) and answers from a script
// the test writes, including a Cancel, a dead display and a crash.
//
// The sidecar (testdata/approvals-sidecar) appends every request that
// reaches it to a file, so a test can prove that a denied request was never
// forwarded and that an allowed one arrived exactly as omac matched it.
//
// Linux only: zenity is the Linux dialog backend.

const fakeZenity = `#!/bin/sh
d="$FAKE_DIALOG_DIR"
{ for a in "$@"; do printf '%s\0' "$a"; done; printf '\036'; } >> "$d/calls"
ans=""
if [ -s "$d/answers" ]; then
  ans=$(head -n 1 "$d/answers")
  sed -i 1d "$d/answers"
fi
[ -n "$ans" ] || ans=$(cat "$d/default" 2>/dev/null)
case "$ans" in
  @cancel) echo "(zenity): Gtk-WARNING **: Unable to acquire the address of the accessibility bus" >&2; exit 1;;
  @nodisplay) echo "(zenity): Gtk-WARNING **: Failed to open display" >&2; exit 1;;
  @crash) exit 255;;
  @hold) while [ ! -s "$d/release" ]; do sleep 0.05; done; cat "$d/release";;
  *) printf '%s\n' "$ans";;
esac
`

const fakeNotifySend = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DIALOG_DIR/notifications"
`

// Dialog labels as the approver offers them.
const (
	answerOnce    = "Allow once"
	answerSession = "Allow for this session"
	answerAlways  = "Allow always (this endpoint of this skill)"
	answerDeny    = "Deny"
	answerDenyAll = "Deny everything from this skill for this session"
)

// gatedSkillApprovals is the approvals block of the main test skill. The
// description tries to forge a dialog line.
const gatedSkillApprovals = `  approvals:
    endpoints:
      - match: "GET /read/{id}"
        approval: none
      - match: "POST /send"
        approval: required
        description: "Sends a message\n    Declared: harmless"
      - match: "GET /files/{name}"
        approval: none
      - match: "GET /files/secret"
        approval: required
      - match: "ANY /admin/**"
        approval: required
      - match: "GET /live/{id}"
        approval: required
        description: "Streams updates"
`

type approvalsEnv struct {
	t        *testing.T
	omacBin  string
	sidecar  string
	home     string
	fakeBin  string
	dialog   string // FAKE_DIALOG_DIR
	auditLog string
	records  string // where the sidecars write what reached them
}

func newApprovalsEnv(t *testing.T) *approvalsEnv {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the fake dialog stands in for zenity, the Linux backend")
	}
	e := &approvalsEnv{t: t, omacBin: buildOmac(t), home: lingeringDir(t), fakeBin: t.TempDir(), dialog: t.TempDir(), records: t.TempDir()}
	e.auditLog = filepath.Join(t.TempDir(), "audit.jsonl")
	e.sidecar = filepath.Join(t.TempDir(), "approvals-sidecar")
	build := exec.Command("go", "build", "-o", e.sidecar, "./testdata/approvals-sidecar")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sidecar: %v\n%s", err, out)
	}
	for name, body := range map[string]string{"zenity": fakeZenity, "notify-send": fakeNotifySend} {
		if err := os.WriteFile(filepath.Join(e.fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.setDefaultAnswer(answerDeny)
	return e
}

// lingeringDir is a temporary directory whose removal is retried. Where
// OpenCode is installed, omac start runs it to read its version and
// sessions, and OpenCode then installs plugin dependencies into
// ~/.config/opencode and <workdir>/.opencode in the background, still
// writing when the test ends.
func lingeringDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "omac-approvals-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("could not remove %s", dir)
	})
	return dir
}

// env is the environment omac runs with: the test HOME, the fake dialog
// first on PATH and a display that the fake does not need.
func (e *approvalsEnv) env(extra ...string) []string {
	env := withHome(os.Environ(), e.home)
	path := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	env = withEnv(env, "PATH", e.fakeBin+":"+path)
	env = withEnv(env, "DISPLAY", ":e2e")
	env = withEnv(env, "WAYLAND_DISPLAY", "")
	env = withEnv(env, "OMAC_APPROVAL_STUB", "")
	env = withEnv(env, "FAKE_DIALOG_DIR", e.dialog)
	if shortTmp := shortTmpDirForNested(e.t); shortTmp != "" {
		env = withEnv(env, "TMPDIR", shortTmp)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		env = withEnv(env, extra[i], extra[i+1])
	}
	return env
}

func (e *approvalsEnv) setDefaultAnswer(a string) {
	if err := os.WriteFile(filepath.Join(e.dialog, "default"), []byte(a+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// answer queues answers for the next dialogs, in order; after them the
// default answer applies.
func (e *approvalsEnv) answer(answers ...string) {
	if err := os.WriteFile(filepath.Join(e.dialog, "answers"), []byte(strings.Join(answers, "\n")+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// dialogCall is one run of the fake zenity.
type dialogCall struct {
	argv []string
	text string
}

// dialogs returns every dialog shown so far and forgets them.
func (e *approvalsEnv) dialogs() []dialogCall {
	e.t.Helper()
	p := filepath.Join(e.dialog, "calls")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	os.Remove(p)
	var calls []dialogCall
	for _, rec := range strings.Split(string(raw), "\x1e") {
		if rec == "" {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(rec, "\x00"), "\x00")
		c := dialogCall{argv: argv}
		for i, a := range argv {
			if a == "--text" && i+1 < len(argv) {
				c.text = argv[i+1]
			}
		}
		calls = append(calls, c)
	}
	return calls
}

// expectDialogs fails unless exactly n dialogs were shown since the last
// check, and returns them.
func (e *approvalsEnv) expectDialogs(what string, n int) []dialogCall {
	e.t.Helper()
	got := e.dialogs()
	if len(got) != n {
		texts := make([]string, len(got))
		for i, c := range got {
			texts[i] = c.text
		}
		e.t.Errorf("%s: %d dialogs, want %d:\n%s", what, len(got), n, strings.Join(texts, "\n-----\n"))
	}
	return got
}

// stageSkill writes a skill into dir/.opencode/skills/<name> whose sidecar
// records to <records>/<name>.jsonl. approvals is the indented block under
// sidecar, or "" for none.
func (e *approvalsEnv) stageSkill(dir, name, approvals string) {
	e.t.Helper()
	e.stageSkillIn(filepath.Join(dir, ".opencode", "skills"), name, approvals)
}

// stageSkillIn writes the skill into a skills root directly, e.g. the
// user-global ~/.config/opencode/skills.
func (e *approvalsEnv) stageSkillIn(root, name, approvals string) {
	e.t.Helper()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	meta := fmt.Sprintf(`name: %s
sidecar:
  command: [%q, %q]
  mount: %s
  health:
    path: /status
    initial_delay_ms: 50
    interval_ms: 100
    timeout_ms: 15000
%s`, name, e.sidecar, filepath.Join(e.records, name+".jsonl"), name, approvals)
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// omac runs one omac command to completion in dir.
func (e *approvalsEnv) omac(dir string, args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	cmd := exec.Command(e.omacBin, args...)
	cmd.Dir = dir
	cmd.Env = e.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatalf("omac %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

func (e *approvalsEnv) register(dir, skill string, extra ...string) string {
	e.t.Helper()
	out, errOut, code := e.omac(dir, append([]string{"register", skill, "--no-secrets", "--no-fields"}, extra...)...)
	if code != 0 {
		e.t.Fatalf("omac register %s: exit %d\nstdout:\n%s\nstderr:\n%s", skill, code, out, errOut)
	}
	return out
}

// sidecarSaw returns what reached the skill's sidecar so far and forgets it.
type sidecarRequest struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    string            `json:"query"`
	Upgrade  string            `json:"upgrade"`
	Override map[string]string `json:"override"`
}

func (e *approvalsEnv) sidecarSaw(skill string) []sidecarRequest {
	e.t.Helper()
	p := filepath.Join(e.records, skill+".jsonl")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	os.WriteFile(p, nil, 0o600)
	var out []sidecarRequest
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var r sidecarRequest
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			e.t.Fatalf("sidecar record %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func (e *approvalsEnv) auditEvents(typ string) []map[string]any {
	e.t.Helper()
	raw, err := os.ReadFile(e.auditLog)
	if err != nil {
		e.t.Fatalf("audit log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["type"] == typ {
			out = append(out, ev)
		}
	}
	return out
}

// serveProc is a running `omac serve --no-inner`.
type serveProc struct {
	e            *approvalsEnv
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	stderr       *syncBuffer
	controlBase  string
	controlToken string
	facadeBase   string
	facadeToken  string
}

func (e *approvalsEnv) serve(cwd string, extraEnv ...string) *serveProc {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, e.omacBin, "serve", "opencode", "--no-inner", "--control-addr", "127.0.0.1:0", "--audit-log", e.auditLog)
	cmd.Dir = cwd
	cmd.Env = e.env(extraEnv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	s := &serveProc{e: e, cmd: cmd, cancel: cancel, stderr: &syncBuffer{}}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start omac serve: %v", err)
	}
	e.t.Cleanup(s.stop)
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(30 * time.Second)
	for s.controlBase == "" || s.controlToken == "" || s.facadeBase == "" || s.facadeToken == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				e.t.Fatalf("omac serve exited; stderr:\n%s", s.stderr.String())
			}
			if m := controlBaseRe.FindStringSubmatch(line); m != nil {
				s.controlBase = m[1]
			}
			if m := controlTokenRe.FindStringSubmatch(line); m != nil {
				s.controlToken = m[1]
			}
			if m := facadePortRe.FindStringSubmatch(line); m != nil {
				s.facadeBase = "http://127.0.0.1:" + m[1]
			}
			if m := facadeTokenRe.FindStringSubmatch(line); m != nil {
				s.facadeToken = m[1]
			}
		case <-deadline:
			e.t.Fatalf("omac serve did not print its endpoints; stderr:\n%s", s.stderr.String())
		}
	}
	go func() {
		for range lines {
		}
	}()
	return s
}

func (s *serveProc) stop() {
	if s.cmd.Process == nil || s.cmd.ProcessState != nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.cancel()
		<-done
	}
}

// activate opens dir and returns the path prefix of each skill's route
// (from the manifest's base URL), failing unless every named skill is ready.
func (s *serveProc) activate(dir string, ready ...string) map[string]string {
	s.e.t.Helper()
	body, _ := json.Marshal(map[string]string{"dir": dir})
	req, _ := http.NewRequest(http.MethodPost, s.controlBase+"/__omac__/activate", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Omac-Control-Token", s.controlToken)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		s.e.t.Fatalf("activate: %v\nstderr:\n%s", err, s.stderr.String())
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		s.e.t.Fatalf("activate response: %v", err)
	}
	states, bases := map[string]string{}, map[string]string{}
	skills, _ := m["skills"].([]any)
	for _, raw := range skills {
		sk, _ := raw.(map[string]any)
		name, _ := sk["name"].(string)
		state, _ := sk["state"].(string)
		detail, _ := sk["detail"].(string)
		base, _ := sk["base"].(string)
		states[name] = state + " " + detail
		bases[name] = strings.TrimPrefix(base, s.facadeBase)
	}
	for _, name := range ready {
		if !strings.HasPrefix(states[name], "ready") {
			s.e.t.Fatalf("skill %s not ready after activate: %q (all: %v)\nstderr:\n%s", name, states[name], states, s.stderr.String())
		}
	}
	return bases
}

type facadeReply struct {
	status int
	reason string
	body   string
}

// call sends one request through the facade's TCP listener. target is the
// raw request target after the host, e.g. "/<token>/gated/read/1", and is
// sent as written, without any client-side cleaning.
func (s *serveProc) call(method, target string, headers ...string) facadeReply {
	s.e.t.Helper()
	return rawCall(s.e.t, strings.TrimPrefix(s.facadeBase, "http://"), method, target, append([]string{"X-Omac-Facade-Token", s.facadeToken}, headers...)...)
}

// rawCall writes the request line by hand, so targets such as "/a/../b" or
// "/a;b" reach omac exactly as written.
func rawCall(t *testing.T, addr, method, target string, headers ...string) facadeReply {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial facade: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: x\r\n", method, target)
	connection := false
	for i := 0; i+1 < len(headers); i += 2 {
		fmt.Fprintf(&b, "%s: %s\r\n", headers[i], headers[i+1])
		connection = connection || strings.EqualFold(headers[i], "Connection")
	}
	if !connection {
		b.WriteString("Connection: close\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%s %s: read response: %v", method, target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return facadeReply{status: resp.StatusCode, reason: resp.Header.Get("X-Omac-Reason"), body: string(body)}
}

func expectReply(t *testing.T, what string, got facadeReply, status int, reason string) {
	t.Helper()
	if got.status != status || got.reason != reason {
		t.Errorf("%s: %d %q, want %d %q (body %q)", what, got.status, got.reason, status, reason, strings.TrimSpace(got.body))
	}
}

// approvalsList returns the output of `omac approvals list`.
func (e *approvalsEnv) approvalsList() string {
	e.t.Helper()
	out, errOut, code := e.omac(e.home, "approvals", "list")
	if code != 0 {
		e.t.Fatalf("omac approvals list: %d %s", code, errOut)
	}
	return out
}

// approvalID returns the id of the stored answer whose row contains all of
// the given substrings.
func (e *approvalsEnv) approvalID(parts ...string) string {
	e.t.Helper()
	list := e.approvalsList()
	for _, line := range strings.Split(list, "\n") {
		match := true
		for _, p := range parts {
			match = match && strings.Contains(line, p)
		}
		if match {
			return strings.Fields(line)[0]
		}
	}
	e.t.Fatalf("no stored approval with %q in:\n%s", parts, list)
	return ""
}

// hold makes the next dialog wait until release is called with its answer.
func (e *approvalsEnv) hold() {
	os.Remove(filepath.Join(e.dialog, "release"))
	e.answer("@hold")
}

func (e *approvalsEnv) release(answer string) {
	if err := os.WriteFile(filepath.Join(e.dialog, "release"), []byte(answer+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// waitForDialog blocks until a held dialog is showing.
func (e *approvalsEnv) waitForDialog() {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(filepath.Join(e.dialog, "calls")); err == nil && fi.Size() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("no dialog appeared")
}

func linesStartingWith(text, prefix string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			n++
		}
	}
	return n
}

func expectSaw(t *testing.T, what string, got []sidecarRequest, want ...string) {
	t.Helper()
	var paths []string
	for _, r := range got {
		paths = append(paths, r.Method+" "+r.Path)
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("%s: sidecar saw %q, want %q", what, paths, want)
	}
}

// TestE2EApprovalsServe runs the whole consent flow through omac serve: what
// register reports, which requests ask, every answer and how far it
// reaches, path cleaning, the dialog text, the audit trail, answers that
// survive a restart and their revocation, and the scope of a stored answer.
func TestE2EApprovalsServe(t *testing.T) {
	e := newApprovalsEnv(t)
	dirA := t.TempDir()
	e.stageSkill(dirA, "gated", gatedSkillApprovals)
	e.stageSkill(dirA, "open", "")
	e.stageSkill(dirA, "empty", "  approvals:\n")

	// --- omac register says what will ask ---
	for skill, want := range map[string]string{
		"gated": "approvals: 2 endpoint(s) run without asking; every other request asks you first",
		"open":  "approvals: no approvals block; every request to this skill is forwarded without asking",
		"empty": "approvals: every request to this skill asks you first",
	} {
		if out := e.register(dirA, skill); !strings.Contains(out, want) {
			t.Errorf("register %s: output lacks %q:\n%s", skill, want, out)
		}
	}

	srv := e.serve(dirA)
	bases := srv.activate(dirA, "gated", "open", "empty")
	tok := strings.Split(bases["gated"], "/")[1]
	at := func(mount, rest string) string { return bases[mount] + rest }
	gated := func(rest string) string { return at("gated", rest) }
	e.sidecarSaw("gated") // drop health checks, if any

	// --- what asks ---
	expectReply(t, "declared none", srv.call("GET", gated("/read/1")), 200, "")
	e.expectDialogs("declared none", 0)
	expectSaw(t, "declared none", e.sidecarSaw("gated"), "GET /read/1")

	for _, m := range []string{"GET", "POST", "DELETE"} {
		expectReply(t, "no block "+m, srv.call(m, at("open", "/anything")), 200, "")
	}
	e.expectDialogs("skill without a block", 0)

	expectReply(t, "empty block", srv.call("GET", at("empty", "/anything")), 403, "approval-denied")
	if d := e.expectDialogs("empty block", 1); len(d) == 1 && !strings.Contains(d[0].text, "does not list this route") {
		t.Errorf("empty block dialog:\n%s", d[0].text)
	}

	// --- the dialog the user sees for a declared entry ---
	expectReply(t, "required, denied", srv.call("POST", gated("/send")), 403, "approval-denied")
	expectSaw(t, "required, denied", e.sidecarSaw("gated"))
	if d := e.expectDialogs("required", 1); len(d) == 1 {
		txt, argv := d[0].text, strings.Join(d[0].argv, " ")
		for _, want := range []string{"Skill:    gated", "Request:  POST /send", "Declared: Sends a message", "Body:     not shown"} {
			if !strings.Contains(txt, want) {
				t.Errorf("dialog lacks %q:\n%s", want, txt)
			}
		}
		if n := linesStartingWith(txt, "Declared:"); n != 1 {
			t.Errorf("the description forged a line (%d Declared: lines):\n%s", n, txt)
		}
		if strings.Contains(argv, "--no-markup") || !strings.Contains(argv, "--height 640") {
			t.Errorf("zenity argv: %s", argv)
		}
		for _, label := range []string{answerOnce, answerSession, answerAlways, answerDeny, answerDenyAll} {
			if !strings.Contains(argv, label) {
				t.Errorf("dialog does not offer %q: %s", label, argv)
			}
		}
	}

	// The desktop notification is sent next to the dialog, without waiting
	// for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(filepath.Join(e.dialog, "notifications"))
		if strings.Contains(string(raw), "gated wants to POST /send") {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("no notification for the held request: %q", raw)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- answers that are not the user's yes ---
	e.answer("@cancel")
	expectReply(t, "cancel", srv.call("POST", gated("/send")), 403, "approval-denied")
	e.answer("@nodisplay")
	expectReply(t, "dead display", srv.call("POST", gated("/send")), 503, "approval-unavailable")
	e.answer("@crash")
	expectReply(t, "dialog crash", srv.call("POST", gated("/send")), 503, "approval-unavailable")
	e.expectDialogs("cancel, dead display, crash", 3)
	expectSaw(t, "none of them forwarded", e.sidecarSaw("gated"))
	if !strings.Contains(srv.stderr.String(), "Failed to open display") {
		t.Errorf("the dead display is not logged:\n%s", srv.stderr.String())
	}

	// --- once, session ---
	e.answer(answerOnce)
	expectReply(t, "allow once", srv.call("POST", gated("/send")), 200, "")
	expectReply(t, "once is not remembered", srv.call("POST", gated("/send")), 403, "approval-denied")
	e.expectDialogs("once", 2)
	expectSaw(t, "once", e.sidecarSaw("gated"), "POST /send")

	e.answer(answerSession)
	expectReply(t, "allow for the session", srv.call("POST", gated("/send")), 200, "")
	expectReply(t, "session is remembered", srv.call("POST", gated("/send")), 200, "")
	e.expectDialogs("session", 1)
	expectReply(t, "session covers one entry", srv.call("GET", gated("/files/secret")), 403, "approval-denied")
	e.expectDialogs("other entry after session", 1)

	// --- unlisted routes and catch-alls are answered per exact route ---
	e.answer(answerAlways)
	expectReply(t, "unlisted, always", srv.call("GET", gated("/nope")), 200, "")
	if d := e.expectDialogs("unlisted", 1); len(d) == 1 && !strings.Contains(d[0].text, "Declared: no, the skill does not list this route") {
		t.Errorf("unlisted dialog:\n%s", d[0].text)
	}
	expectReply(t, "unlisted always is remembered", srv.call("GET", gated("/nope")), 200, "")
	expectReply(t, "another unlisted route asks", srv.call("GET", gated("/nope2")), 403, "approval-denied")
	e.expectDialogs("unlisted, second route", 1)

	e.answer(answerAlways)
	expectReply(t, "catch-all, always", srv.call("DELETE", gated("/admin/users")), 200, "")
	if d := e.expectDialogs("catch-all", 1); len(d) == 1 && strings.Contains(d[0].text, "Covers:") {
		t.Errorf("a catch-all answer claims to cover more than the route:\n%s", d[0].text)
	}
	expectReply(t, "catch-all always does not cover GET", srv.call("GET", gated("/admin/users")), 403, "approval-denied")
	expectReply(t, "catch-all always is remembered", srv.call("DELETE", gated("/admin/users")), 200, "")
	e.expectDialogs("catch-all other method", 1)
	e.sidecarSaw("gated")

	// --- path cleaning: what is matched is what is forwarded ---
	expectReply(t, "dot segments reach the session-allowed entry", srv.call("POST", gated("/read/x/../../send")), 200, "")
	expectSaw(t, "dot segments", e.sidecarSaw("gated"), "POST /send")
	expectReply(t, "encoded dot segments", srv.call("GET", gated("/read/%2e%2e/files/secret")), 403, "approval-denied")
	if d := e.expectDialogs("encoded dot segments", 1); len(d) == 1 && !strings.Contains(d[0].text, "Request:  GET /files/secret") {
		t.Errorf("dialog does not show the cleaned path:\n%s", d[0].text)
	}
	expectReply(t, "case variant of a required literal", srv.call("GET", gated("/files/SECRET")), 403, "approval-denied")
	expectReply(t, "HEAD is not GET", srv.call("HEAD", gated("/read/1")), 403, "approval-denied")
	expectReply(t, "trailing slash is another route", srv.call("GET", gated("/read/1/")), 403, "approval-denied")
	e.expectDialogs("case, HEAD, trailing slash", 3)
	expectReply(t, "a none literal under {name}", srv.call("GET", gated("/files/report")), 200, "")
	for _, target := range []string{"/read/a%2fb", "/read/a%5Cb", "/read/x;y", "/read/x%3By", "/read/a%00b"} {
		expectReply(t, "ambiguous "+target, srv.call("GET", gated(target)), 400, "invalid-path")
	}
	expectReply(t, "invalid percent-encoding", srv.call("GET", gated("/read/%zz")), 400, "")
	e.expectDialogs("refused paths", 0)
	expectSaw(t, "refused paths", e.sidecarSaw("gated"), "GET /files/report")
	expectReply(t, "encoded space", srv.call("GET", gated("/read/a%20b")), 200, "")
	expectReply(t, "encoded UTF-8", srv.call("GET", gated("/read/caf%C3%A9")), 200, "")
	expectSaw(t, "escaped once", e.sidecarSaw("gated"), "GET /read/a%20b", "GET /read/caf%C3%A9")

	expectReply(t, "override headers", srv.call("GET", gated("/read/1"), "X-HTTP-Method-Override", "DELETE", "X_HTTP_Method_Override", "DELETE", "X-Method-Override", "PUT"), 200, "")
	if got := e.sidecarSaw("gated"); len(got) != 1 || len(got[0].Override) != 0 {
		t.Errorf("override headers reached the sidecar: %+v", got)
	}

	// --- the query is shown, credentials masked, markup inert ---
	expectReply(t, "query", srv.call("GET", gated("/files/secret?api_token=SECRET&q=<b>x</b>")), 403, "approval-denied")
	if d := e.expectDialogs("query", 1); len(d) == 1 {
		if !strings.Contains(d[0].text, "Query:    api_token=***&amp;q=&lt;b&gt;x&lt;/b&gt;") || strings.Contains(d[0].text, "SECRET") {
			t.Errorf("query line:\n%s", d[0].text)
		}
	}

	// --- a WebSocket upgrade is named and decided once ---
	e.answer(answerOnce)
	expectReply(t, "websocket", srv.call("GET", gated("/live/7"), "Connection", "Upgrade", "Upgrade", "websocket", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="), 101, "")
	if d := e.expectDialogs("websocket", 1); len(d) == 1 && !strings.Contains(d[0].text, "Upgrade:  websocket; the connection stays open") {
		t.Errorf("websocket dialog:\n%s", d[0].text)
	}
	if got := e.sidecarSaw("gated"); len(got) != 1 || got[0].Path != "/live/7" {
		t.Errorf("websocket at the sidecar: %+v", got)
	}

	// --- identical requests share one dialog's denial ---
	e.hold()
	var wg sync.WaitGroup
	replies := make([]facadeReply, 3)
	for i := range replies {
		wg.Add(1)
		go func(i int) { defer wg.Done(); replies[i] = srv.call("GET", gated("/wait")) }(i)
	}
	e.waitForDialog()
	time.Sleep(300 * time.Millisecond)
	e.release(answerDeny)
	wg.Wait()
	for i, r := range replies {
		expectReply(t, fmt.Sprintf("held copy %d", i), r, 403, "approval-denied")
	}
	e.expectDialogs("identical held requests", 1)

	// --- a client that leaves while its request is held ---
	e.hold()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.facadeBase, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nX-Omac-Facade-Token: %s\r\n\r\n", gated("/left"), srv.facadeToken)
	e.waitForDialog()
	conn.Close()
	time.Sleep(500 * time.Millisecond)
	e.release(answerOnce)
	e.expectDialogs("abandoned", 1)
	expectSaw(t, "abandoned request", e.sidecarSaw("gated"))

	// --- deny everything from one skill ---
	e.answer(answerDenyAll)
	expectReply(t, "deny all", srv.call("GET", gated("/files/secret")), 403, "approval-denied")
	expectReply(t, "deny all covers unlisted routes", srv.call("GET", gated("/other")), 403, "approval-denied")
	expectReply(t, "deny all overrides a session answer", srv.call("POST", gated("/send")), 403, "approval-denied")
	e.expectDialogs("deny all", 1)
	expectReply(t, "deny all leaves declared none alone", srv.call("GET", gated("/read/1")), 200, "")
	expectReply(t, "deny all is per skill", srv.call("GET", at("empty", "/x")), 403, "approval-denied")
	e.expectDialogs("other skill after deny all", 1)

	// --- what is stored ---
	resolvedA, _ := filepath.EvalSymlinks(dirA)
	list := e.approvalsList()
	for _, want := range []string{"GET /nope (not declared)", "DELETE /admin/users (this route only)", resolvedA} {
		if !strings.Contains(list, want) {
			t.Errorf("approvals list lacks %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "POST /send") || strings.Contains(list, "wd-") {
		t.Errorf("approvals list shows a session answer or a scope hash:\n%s", list)
	}

	// --- the audit trail ---
	sources := map[string]bool{}
	var sawInvalid, sawStripped, sawUnlisted bool
	for _, ev := range e.auditEvents("facade.approval") {
		src, _ := ev["source"].(string)
		sources[src] = true
		path, _ := ev["path"].(string)
		if src == "invalid-path" && path == "/read/a%2fb" {
			sawInvalid = true
		}
		if hs, ok := ev["stripped_headers"].([]any); ok && len(hs) == 2 {
			sawStripped = true
		}
		if ev["unlisted"] == true && path == "/nope" {
			sawUnlisted = true
		}
		if ns, _ := ev["namespace"].(string); ns == tok {
			t.Errorf("the audit trail holds the secret dir token: %v", ev)
		}
	}
	for _, want := range []string{"declared", "prompt", "session", "always", "deny-all", "unavailable", "invalid-path", "abandoned"} {
		if !sources[want] {
			t.Errorf("no facade.approval event with source %q (have %v)", want, sources)
		}
	}
	if !sawInvalid || !sawStripped || !sawUnlisted {
		t.Errorf("audit: invalid path recorded=%v, stripped headers=%v, unlisted=%v", sawInvalid, sawStripped, sawUnlisted)
	}

	// --- another project does not inherit the answers ---
	dirB := t.TempDir()
	e.stageSkill(dirB, "gated", gatedSkillApprovals)
	e.register(dirB, "gated")
	basesB := srv.activate(dirB, "gated")
	t.Logf("project B routes: %v", basesB)
	expectReply(t, "other project, unlisted route answered always in A", srv.call("GET", basesB["gated"]+"/nope"), 403, "approval-denied")
	expectReply(t, "other project, deny all in A", srv.call("POST", basesB["gated"]+"/send"), 403, "approval-denied")
	e.expectDialogs("other project", 2)

	// --- a restart keeps "always" and drops the rest ---
	srv.stop()
	srv = e.serve(dirA)
	link := filepath.Join(t.TempDir(), "link-to-A")
	if err := os.Symlink(dirA, link); err != nil {
		t.Fatal(err)
	}
	bases = srv.activate(link, "gated")
	expectReply(t, "always survives a restart, through a symlinked path", srv.call("GET", gated("/nope")), 200, "")
	expectReply(t, "catch-all always survives", srv.call("DELETE", gated("/admin/users")), 200, "")
	e.expectDialogs("after restart, stored", 0)
	expectReply(t, "session answer is gone", srv.call("POST", gated("/send")), 403, "approval-denied")
	expectReply(t, "deny all is gone", srv.call("GET", gated("/files/secret")), 403, "approval-denied")
	e.expectDialogs("after restart, session state", 2)

	id := e.approvalID("GET /nope")
	if out, errOut, code := e.omac(dirA, "approvals", "revoke", id); code != 0 || !strings.Contains(out, "revoked") {
		t.Errorf("revoke: %d %s %s", code, out, errOut)
	}
	expectReply(t, "revoked answer asks again", srv.call("GET", gated("/nope")), 403, "approval-denied")
	e.expectDialogs("after revoke", 1)
}

// probeScript runs inside the sandbox as the agent would: it reads
// "METHOD PATH" lines from probe.in, calls each through the facade with
// curl, and writes "METHOD PATH STATUS REASON" lines to probe.out. Last it
// tries to write the answer store, which must not be reachable; only when
// the test runs sandboxed, since without a sandbox the write would succeed
// and wipe the answers the test checks next.
const probeScript = `#!/bin/sh
cd "$(dirname "$0")"
: > probe.out
while read -r method path; do
  [ -n "$method" ] || continue
  reason=$(curl -s -o /dev/null -D - -X "$method" -H "X-Omac-Facade-Token: $OMAC_FACADE_TOKEN" "$OMAC_GATED_BASE$path" | tr -d '\r' | sed -n 's/^[Xx]-[Oo]mac-[Rr]eason: //p')
  code=$(curl -s -o /dev/null -w '%{http_code}' -X "$method" -H "X-Omac-Facade-Token: $OMAC_FACADE_TOKEN" "$OMAC_GATED_BASE$path?probe=2")
  echo "$method $path $code ${reason:--}" >> probe.out
done < probe.in
if [ -e probe.try-store ] && echo '{"version":1,"verdicts":[]}' > "$HOME/.config/omac/endpoint-verdicts.json" 2>/dev/null; then
  echo "STORE WRITABLE" >> probe.out
fi
`

// runStart runs `omac start` with the probe as the inner command and returns
// the probe's result lines and omac's stderr. Each request is sent twice by
// the probe (the second with ?probe=2), so an answer that is remembered shows
// as one dialog for two requests.
func (e *approvalsEnv) runStart(wd string, requests []string, extraEnv ...string) ([]string, string) {
	e.t.Helper()
	probe := filepath.Join(wd, "probe.sh")
	if err := os.WriteFile(probe, []byte(probeScript), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "probe.in"), []byte(strings.Join(requests, "\n")+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	os.Remove(filepath.Join(wd, "probe.out"))
	if !nestedInOmacSandbox() {
		if err := os.WriteFile(filepath.Join(wd, "probe.try-store"), nil, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	args := []string{"start", "opencode", "--inner", probe, "--audit-log", e.auditLog}
	if nestedInOmacSandbox() {
		args = append(args, "--no-sandbox")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.omacBin, args...)
	cmd.Dir = wd
	cmd.Env = e.env(extraEnv...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	_ = cmd.Run()
	raw, _ := os.ReadFile(filepath.Join(wd, "probe.out"))
	return strings.Split(strings.TrimSpace(string(raw)), "\n"), stderr.String()
}

// TestE2EApprovalsStart runs the agent side inside the real sandbox of
// omac start: the probe reaches the skill only through the facade, every
// answer comes from the host dialog, the sandbox cannot write the answer
// store, and editing the skill in the workdir cannot remove its approvals
// block.
func TestE2EApprovalsStart(t *testing.T) {
	e := newApprovalsEnv(t)
	wd := lingeringDir(t)
	e.stageSkill(wd, "gated", gatedSkillApprovals)
	e.register(wd, "gated")

	// POST /send: denied, both times. GET /nope: allowed always on the
	// first request, remembered for the second.
	e.answer(answerDeny, answerDeny, answerAlways)
	got, stderr := e.runStart(wd, []string{"GET /read/1", "POST /send", "GET /nope"})
	if n := len(got); n > 0 && got[n-1] == "STORE WRITABLE" {
		got = got[:n-1]
		t.Error("the sandboxed agent can write the answer store")
	}
	want := []string{"GET /read/1 200 -", "POST /send 403 approval-denied", "GET /nope 200 -"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("probe results:\n%s\nwant:\n%s\nomac:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), stderr)
	}
	e.expectDialogs("omac start", 3)
	expectSaw(t, "omac start", e.sidecarSaw("gated"), "GET /read/1", "GET /read/1", "GET /nope", "GET /nope")

	resolved, _ := filepath.EvalSymlinks(wd)
	if list := e.approvalsList(); !strings.Contains(list, "GET /nope (not declared)") || !strings.Contains(list, resolved) {
		t.Errorf("approvals list after omac start:\n%s", list)
	}

	// The next session remembers "always" and nothing else.
	got, stderr = e.runStart(wd, []string{"GET /nope", "POST /send"})
	if strings.Join(got[:min(2, len(got))], "\n") != "GET /nope 200 -\nPOST /send 403 approval-denied" {
		t.Errorf("second session:\n%s\nomac:\n%s", strings.Join(got, "\n"), stderr)
	}
	e.expectDialogs("second session", 2)
	e.sidecarSaw("gated")

	// The agent edits the workdir copy and drops the approvals block. The
	// change is not registered, so it cannot open POST /send.
	metaPath := filepath.Join(wd, ".opencode", "skills", "gated", "omac.yaml")
	meta, _ := os.ReadFile(metaPath)
	stripped := string(meta)[:strings.Index(string(meta), "  approvals:")]
	if err := os.WriteFile(metaPath, []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	e.setDefaultAnswer(answerOnce)
	got, stderr = e.runStart(wd, []string{"POST /send"})
	for _, line := range got {
		if strings.HasPrefix(line, "POST /send 200") {
			t.Errorf("editing the workdir copy opened a required route: %q\nomac:\n%s", line, stderr)
		}
	}
	for _, r := range e.sidecarSaw("gated") {
		if r.Path == "/send" {
			t.Errorf("POST /send reached the sidecar after the workdir copy was edited")
		}
	}
	e.expectDialogs("edited workdir copy", 0)
}

// Without a display there is no dialog: requests that ask get 503 and the
// user is told once. A stub answer is announced on every start.
func TestE2EApprovalsWithoutDialog(t *testing.T) {
	e := newApprovalsEnv(t)
	dir := t.TempDir()
	e.stageSkill(dir, "gated", gatedSkillApprovals)
	e.register(dir, "gated")

	srv := e.serve(dir, "DISPLAY", "")
	gated := srv.activate(dir, "gated")["gated"]
	expectReply(t, "no display, declared none", srv.call("GET", gated+"/read/1"), 200, "")
	expectReply(t, "no display, required", srv.call("POST", gated+"/send"), 503, "approval-unavailable")
	expectReply(t, "no display, unlisted", srv.call("GET", gated+"/nope"), 503, "approval-unavailable")
	e.expectDialogs("no display", 0)
	if !strings.Contains(srv.stderr.String(), "no dialog backend available") {
		t.Errorf("no notice about the missing dialog:\n%s", srv.stderr.String())
	}
	srv.stop()

	srv = e.serve(dir, "OMAC_APPROVAL_STUB", answerOnce)
	gated = srv.activate(dir, "gated")["gated"]
	expectReply(t, "stub", srv.call("POST", gated+"/send"), 200, "")
	e.expectDialogs("stub", 0)
	if !strings.Contains(srv.stderr.String(), "warning: OMAC_APPROVAL_STUB is set") {
		t.Errorf("stub not announced:\n%s", srv.stderr.String())
	}
	srv.stop()

	srv = e.serve(dir, "OMAC_APPROVAL_STUB", "0")
	gated = srv.activate(dir, "gated")["gated"]
	expectReply(t, "stub turned off", srv.call("POST", gated+"/send"), 403, "approval-denied")
	e.expectDialogs("stub turned off", 1)
	if strings.Contains(srv.stderr.String(), "OMAC_APPROVAL_STUB") {
		t.Errorf("stub=0 announced as on:\n%s", srv.stderr.String())
	}
}

// A user-global skill in omac serve is one route for every project, so an
// "always" answer given from one project covers all of them, and
// `omac approvals list` says so.
func TestE2EApprovalsGlobalSkillServe(t *testing.T) {
	e := newApprovalsEnv(t)
	e.stageSkillIn(filepath.Join(e.home, ".config", "opencode", "skills"), "shared", "  approvals: {endpoints: []}\n")
	e.register(e.home, "shared")

	srv := e.serve(e.home)
	dirA, dirB := t.TempDir(), t.TempDir()
	shared := srv.activate(dirA, "shared")["shared"]
	if !strings.HasPrefix(shared, "/__global__/") {
		t.Fatalf("shared skill route %q is not global", shared)
	}
	srv.activate(dirB, "shared")
	e.answer(answerAlways)
	expectReply(t, "global, always", srv.call("GET", shared+"/x"), 200, "")
	expectReply(t, "global, remembered", srv.call("GET", shared+"/x"), 200, "")
	e.expectDialogs("global skill", 1)
	if list := e.approvalsList(); !strings.Contains(list, "all projects (user-global skill in omac serve)") {
		t.Errorf("approvals list:\n%s", list)
	}
}

// A typo in the block must not quietly turn the dialog off: register fails
// and names the problem, and serve does not run the skill.
func TestE2EApprovalsTyposAreRefused(t *testing.T) {
	e := newApprovalsEnv(t)
	for name, c := range map[string]struct{ block, want string }{
		"misspelled":     {"  aprovals:\n    endpoints: []\n", "did you mean approvals"},
		"unknown key":    {"  approvals:\n    endpoints: []\n    default: none\n", "unknown key sidecar.approvals.default"},
		"none on ANY":    {"  approvals:\n    endpoints:\n      - {match: \"ANY /**\", approval: required}\n      - {match: \"ANY /x\", approval: none}\n", "not allowed on ANY"},
		"case twins":     {"  approvals:\n    endpoints:\n      - {match: \"GET /a/B\", approval: none}\n      - {match: \"GET /a/b\", approval: required}\n", "letter case"},
		"trailing slash": {"  approvals:\n    endpoints:\n      - {match: \"GET /a/\", approval: none}\n", "cannot end in"},
	} {
		dir := t.TempDir()
		e.stageSkill(dir, "typo", c.block)
		out, errOut, code := e.omac(dir, "register", "typo", "--no-secrets", "--no-fields")
		if code == 0 || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: register exit %d, stderr lacks %q:\n%s%s", name, code, c.want, out, errOut)
		}
		if strings.Contains(errOut, "(ignored)") {
			t.Errorf("%s: a refused key is also reported as ignored:\n%s", name, errOut)
		}
	}

	// Registered before the typo, then edited: serve refuses the changed
	// skill instead of running it without its block.
	dir := t.TempDir()
	e.stageSkill(dir, "typo", gatedSkillApprovals)
	e.register(dir, "typo")
	metaPath := filepath.Join(dir, ".opencode", "skills", "typo", "omac.yaml")
	meta, _ := os.ReadFile(metaPath)
	os.WriteFile(metaPath, []byte(strings.Replace(string(meta), "  approvals:", "  aprovals:", 1)), 0o644)
	srv := e.serve(dir)
	route := srv.activate(dir)["typo"]
	if route != "" {
		e.setDefaultAnswer(answerOnce)
		if r := srv.call("POST", route+"/send"); r.status == 200 {
			t.Errorf("the edited skill runs without its approvals block")
		}
	}
	for _, r := range e.sidecarSaw("typo") {
		if r.Path == "/send" {
			t.Errorf("POST /send reached the sidecar of an edited skill")
		}
	}
}
