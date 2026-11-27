//go:build linux

package gitservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, s *Service, action string, r *Request) (int, Result) {
	t.Helper()
	var body []byte
	method := "GET"
	if r != nil {
		body, _ = json.Marshal(r)
		method = "POST"
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(method, "/"+action, bytes.NewReader(body)))
	var result Result
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("bad response: %s", w.Body.String())
	}
	return w.Code, result
}

func status(t *testing.T, s *Service) State {
	t.Helper()
	code, r := call(t, s, "status", nil)
	if code != 200 || r.State == nil {
		t.Fatalf("status: %d %+v", code, r)
	}
	return *r.State
}

func mutate(t *testing.T, s *Service, action string, r Request) Result {
	t.Helper()
	state := status(t, s)
	r.Expected = &state
	r.ID = fmt.Sprintf("%s:%032x", state.Session, len(s.operations)+1)
	code, result := call(t, s, action, &r)
	if code != 200 {
		t.Fatalf("%s: %d %+v", action, code, result)
	}
	return result
}

func TestLocalWorkflowAndReplay(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	writeFile(t, filepath.Join(dir, "source"), "one\n")
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	state := status(t, s)
	req := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 99), Expected: &state, Message: "initial", Signoff: true}
	code, first := call(t, s, "commit", &req)
	if code != 200 {
		t.Fatalf("commit: %d %+v", code, first)
	}
	code, again := call(t, s, "commit", &req)
	if code != 200 || again.State.Head != first.State.Head {
		t.Fatal("commit was replayed")
	}
	req.Message = "different"
	code, _ = call(t, s, "commit", &req)
	if code != 409 {
		t.Fatal("reused ID accepted different request")
	}
	if !strings.Contains(fixtureGit(t, dir, "log", "-1", "--format=%B"), "Signed-off-by: Test <test@example.invalid>") {
		t.Fatal("missing signoff")
	}
	mutate(t, s, "branches", Request{Branch: "topic", Target: first.State.Head})
	mutate(t, s, "switch", Request{Branch: "topic", Target: first.State.Head})
	writeFile(t, filepath.Join(dir, "source"), "two\n")
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	second := mutate(t, s, "commit", Request{Message: "topic"})
	if second.State.Dirty || second.State.Head == first.State.Head {
		t.Fatal("bad commit state")
	}
	mutate(t, s, "switch", Request{Branch: "main", Target: first.State.Head})
	data, _ := os.ReadFile(filepath.Join(dir, "source"))
	if string(data) != "one\n" {
		t.Fatal("switch retained wrong contents")
	}
	if err := os.Remove(filepath.Join(dir, "source")); err != nil {
		t.Fatal(err)
	}
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	mutate(t, s, "commit", Request{Message: "delete"})
	if got := fixtureGit(t, dir, "ls-tree", "HEAD"); got != "" {
		t.Fatal(got)
	}
}

func TestStageRejectsPathsFiltersAndStaleRequests(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir, filepath.Join(dir, ".env"))
	writeFile(t, filepath.Join(dir, ".env"), "secret")
	writeFile(t, filepath.Join(dir, "source"), "safe")
	writeFile(t, filepath.Join(dir, ".gitattributes"), "filtered filter=lfs\n")
	writeFile(t, filepath.Join(dir, "filtered"), "unfiltered payload")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{".env", "../escape", ".git/config", ".gitmodules", "outside/file", "filtered", "."} {
		state := status(t, s)
		r := Request{ID: fmt.Sprintf("%s:%032x", state.Session, i+100), Expected: &state, Paths: []string{path}}
		code, result := call(t, s, "stage", &r)
		if code == 200 || result.Unknown {
			t.Fatalf("stage %q: %d %+v", path, code, result)
		}
		if after := status(t, s); after.Index != state.Index {
			t.Fatal("rejected request changed index")
		}
	}
	state := status(t, s)
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	r := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 500), Expected: &state, Message: "stale"}
	if code, _ := call(t, s, "commit", &r); code != 409 {
		t.Fatal("stale request was accepted")
	}
}

func TestSwitchUpdatesTrackedHooksWithoutExecutingThem(t *testing.T) {
	dir := gitFixture(t)
	writeFile(t, filepath.Join(dir, ".githooks", "post-checkout"), "#!/bin/sh\necho bad > executed\n")
	if err := os.Chmod(filepath.Join(dir, ".githooks", "post-checkout"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".githooks", "pre-commit"), "#!/bin/sh\necho bad > executed\n")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-qm", "base")
	base := fixtureGit(t, dir, "rev-parse", "HEAD")
	fixtureGit(t, dir, "switch", "-qc", "topic")
	writeFile(t, filepath.Join(dir, ".githooks", "pre-commit"), "#!/bin/sh\necho worse > executed\n")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-qm", "topic")
	topic := fixtureGit(t, dir, "rev-parse", "HEAD")
	fixtureGit(t, dir, "switch", "-q", "main")
	fixtureGit(t, dir, "config", "core.hooksPath", ".githooks")
	s := newTestService(t, dir)
	mutate(t, s, "switch", Request{Branch: "topic", Target: topic})
	data, _ := os.ReadFile(filepath.Join(dir, ".githooks", "pre-commit"))
	if !strings.Contains(string(data), "worse") || status(t, s).Dirty {
		t.Fatal("tracked hooks not updated cleanly")
	}
	mutate(t, s, "switch", Request{Branch: "main", Target: base})
	if _, err := os.Stat(filepath.Join(dir, "executed")); !os.IsNotExist(err) {
		t.Fatal("hook executed")
	}
}

func TestUnsupportedLayoutAndStateRefused(t *testing.T) {
	for _, name := range []string{"commondir", "shallow", "objects/info/alternates", "MERGE_HEAD", "config.worktree"} {
		t.Run(name, func(t *testing.T) {
			dir := gitFixture(t)
			writeFile(t, filepath.Join(dir, ".git", name), ".\n")
			if name == "config.worktree" {
				fixtureGit(t, dir, "config", "extensions.worktreeConfig", "true")
				requireWorker(t)
			}
			if s, err := New(Options{Workdir: dir, Name: "Test", Email: "test@example.invalid"}); err == nil {
				s.Close()
				t.Fatal("unsupported state accepted")
			}
		})
	}
}

func TestDeniedCallsAndUnknownFieldsNeverMutate(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	before := status(t, s)
	for _, body := range []string{`{"argv":["config","core.hooksPath","bad"]}`, `{"env":{"GIT_DIR":"elsewhere"}}`, `{"paths":["source"]} {}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/stage", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("accepted body %s: %d", body, w.Code)
		}
	}
	if after := status(t, s); after.Index != before.Index || after.Head != before.Head {
		t.Fatal("invalid request changed repository")
	}
}

func TestEnrollmentLockAndReplacedRoot(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	if second, err := New(Options{Workdir: dir, Name: "Test", Email: "test@example.invalid"}); err == nil {
		second.Close()
		t.Fatal("second enrollment accepted")
	}
	if err := os.Rename(filepath.Join(dir, ".git"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "init", "-q", "-b", "main")
	if code, _ := call(t, s, "status", nil); code != 409 {
		t.Fatal("replaced gitdir accepted")
	}
}

func TestWorkerEnvironmentIsNotInherited(t *testing.T) {
	dir := gitFixture(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", "touch /repo/.git/injected")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-not-for-git")
	s := newTestService(t, dir)
	if _, err := s.run(context.Background(), false, false, nil, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "injected")); !os.IsNotExist(err) {
		t.Fatal("inherited Git config executed")
	}
}

func TestRejectedMutationIsNotReplayedAndServiceRecovers(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	writeFile(t, filepath.Join(dir, "source"), "new")
	state := status(t, s)
	writeFile(t, filepath.Join(dir, ".git", "index.lock"), "unrelated lock")
	r := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 1), Expected: &state, Paths: []string{"source"}}
	code, failed := call(t, s, "stage", &r)
	if code != 409 || failed.Unknown {
		t.Fatalf("lock conflict: %d %+v", code, failed)
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".git", "index.lock")); err != nil || string(data) != "unrelated lock" {
		t.Fatal("service removed or modified someone else's lock")
	}
	if err := os.Remove(filepath.Join(dir, ".git", "index.lock")); err != nil {
		t.Fatal(err)
	}
	code, again := call(t, s, "stage", &r)
	if code != 409 || again.Error != failed.Error || again.Unknown {
		t.Fatal("retried a failed operation")
	}
	r.ID = fmt.Sprintf("%s:%032x", state.Session, 2)
	if code, result := call(t, s, "stage", &r); code != 200 || result.Unknown {
		t.Fatalf("rejected request blocked later staging: %d %+v", code, result)
	}

}

func TestOperationLedgerLimitAndBusyService(t *testing.T) {
	s := newTestService(t, gitFixture(t))
	state := status(t, s)
	s.mu.Lock()
	if code, _ := call(t, s, "status", nil); code != 503 {
		t.Error("busy service accepted a concurrent request")
	}
	s.mu.Unlock()
	for i := range maxOperations {
		s.operations[fmt.Sprint(i)] = record{}
	}
	r := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 1), Expected: &state, Paths: []string{"source"}}
	if code, _ := call(t, s, "stage", &r); code != 503 || len(s.operations) != maxOperations {
		t.Fatal("full ledger evicted history or admitted a mutation")
	}
	s.Close()
	if code, _ := call(t, s, "status", nil); code != 503 {
		t.Fatal("closed service still available")
	}
}

func TestWorkerCancellationAndOutputLimit(t *testing.T) {
	s := newTestService(t, gitFixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.run(ctx, true, false, nil, "add", "--", "missing"); err == nil {
		t.Fatal("canceled worker succeeded")
	}
	var b boundedBuffer
	if _, err := b.Write(make([]byte, maxOutput+1)); err == nil || b.buffer.Len() != 0 {
		t.Fatal("worker output was not bounded")
	}
}

func TestGitPolicyMustBeConfiguredOnceBeforeRequests(t *testing.T) {
	requireWorker(t)
	dir := gitFixture(t)
	s, err := New(Options{Workdir: dir, Name: "Test", Email: "test@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if code, _ := call(t, s, "status", nil); code != 503 {
		t.Fatal("service admitted requests before final policy")
	}
	for _, denied := range []string{"relative", filepath.Join(dir, ".git", "config"), filepath.Dir(dir)} {
		if err := s.ConfigureDenies([]string{denied}); err == nil {
			t.Fatalf("accepted incompatible deny %s", denied)
		}
	}
	if err := s.ConfigureDenies([]string{filepath.Join(dir, ".env")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureDenies(nil); err == nil {
		t.Fatal("allowed a live policy replacement")
	}
}

func TestSwitchRefusesTargetFiltersAndIgnoredFileOverwrite(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprint(filtered), func(t *testing.T) {
			dir := gitFixture(t)
			writeFile(t, filepath.Join(dir, ".gitignore"), "local\n")
			fixtureGit(t, dir, "add", ".gitignore")
			fixtureGit(t, dir, "commit", "-qm", "base")
			fixtureGit(t, dir, "switch", "-qc", "topic")
			writeFile(t, filepath.Join(dir, "local"), "tracked on topic")
			fixtureGit(t, dir, "add", "-f", "local")
			if filtered {
				writeFile(t, filepath.Join(dir, ".gitattributes"), "local filter=lfs\n")
				fixtureGit(t, dir, "add", ".gitattributes")
			}
			fixtureGit(t, dir, "commit", "-qm", "topic")
			topic := fixtureGit(t, dir, "rev-parse", "HEAD")
			fixtureGit(t, dir, "switch", "-q", "main")
			if !filtered {
				writeFile(t, filepath.Join(dir, "local"), "must be preserved")
			}
			s := newTestService(t, dir)
			state := status(t, s)
			r := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 1), Expected: &state, Branch: "topic", Target: topic}
			code, result := call(t, s, "switch", &r)
			if code != 409 || (filtered && !strings.Contains(result.Error, "filter")) {
				t.Fatalf("unsupported switch: %d %+v", code, result)
			}
			if !filtered {
				if data, err := os.ReadFile(filepath.Join(dir, "local")); err != nil || string(data) != "must be preserved" {
					t.Fatal("ignored file overwritten")
				}
			}
			if got := status(t, s); got.Head != state.Head || got.Branch != "main" {
				t.Fatal("refused switch changed HEAD")
			}
		})
	}
}

func TestStageLiteralNamesAndSymlinks(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	for _, name := range []string{"file with space", ":(glob)*", "-option"} {
		writeFile(t, filepath.Join(dir, name), "literal content")
		mutate(t, s, "stage", Request{Paths: []string{name}})
	}
	if err := os.Symlink("/outside/never-read", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	mutate(t, s, "stage", Request{Paths: []string{"link"}})
	mutate(t, s, "commit", Request{Message: "literal names"})
	if got := fixtureGit(t, dir, "show", "HEAD:link"); got != "/outside/never-read" {
		t.Fatalf("symlink target was followed: %q", got)
	}
}

func TestCanceledClientRetainsKnownCommitOutcome(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	writeFile(t, filepath.Join(dir, "source"), "content")
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	state := status(t, s)
	req := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 100), Expected: &state, Message: "disconnected"}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/commit", bytes.NewReader(body)).WithContext(ctx))
	if w.Code != 200 {
		t.Fatalf("disconnected operation: %d %s", w.Code, w.Body)
	}
	if code, result := call(t, s, "operations/"+req.ID, nil); code != 200 || result.State == nil || result.Unknown {
		t.Fatalf("lost known outcome: %d %+v", code, result)
	}
	if code, _ := call(t, s, "commit", &req); code != 200 {
		t.Fatal("same-ID recovery failed")
	}
	if got := fixtureGit(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("lost response replayed commit: %s", got)
	}
}

func TestFailedStageOutcomeDependsOnIndex(t *testing.T) {
	for _, paths := range [][]string{{"missing"}, {"ignored"}, {"source", "ignored"}} {
		t.Run(strings.Join(paths, "+"), func(t *testing.T) {
			dir := gitFixture(t)
			writeFile(t, filepath.Join(dir, ".gitignore"), "ignored\n")
			writeFile(t, filepath.Join(dir, "ignored"), "ignored contents")
			writeFile(t, filepath.Join(dir, "source"), "valid contents")
			s := newTestService(t, dir)
			before := status(t, s)
			r := Request{ID: fmt.Sprintf("%s:%032x", s.session, 1), Expected: &before, Paths: paths}
			code, failed := call(t, s, "stage", &r)
			after := status(t, s)
			partial := len(paths) > 1
			if code != 409 || failed.Unknown != partial || (before.Index != after.Index) != partial {
				t.Fatalf("stage: %d %+v, index changed=%t", code, failed, before.Index != after.Index)
			}
			code, replay := call(t, s, "stage", &r)
			if code != 409 || replay.Error != failed.Error || replay.Unknown != partial {
				t.Fatal("failed request was replayed")
			}
			r.ID = fmt.Sprintf("%s:%032x", s.session, 2)
			r.Expected, r.Paths = &after, []string{"source"}
			code, next := call(t, s, "stage", &r)
			if (code == 200) == partial || next.Unknown != partial {
				t.Fatalf("follow-up: %d %+v", code, next)
			}
		})
	}
}

func TestIndexFailureRecoveryRequiresVerifiableCompletion(t *testing.T) {
	s := newTestService(t, gitFixture(t))
	before := status(t, s)
	_, failure := s.run(context.Background(), true, false, nil, "add", "--", "missing")
	if failure == nil || s.indexMayHaveChanged(context.Background(), before, failure) {
		t.Fatal("completed unchanged failure not recoverable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !s.indexMayHaveChanged(ctx, before, failure) {
		t.Fatal("cancellation treated as known outcome")
	}
	if !s.indexMayHaveChanged(context.Background(), before, fmt.Errorf("unverifiable worker failure")) {
		t.Fatal("unverifiable worker treated as completed")
	}
	if err := os.Remove(filepath.Join(s.root, ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if !s.indexMayHaveChanged(context.Background(), before, failure) {
		t.Fatal("unreadable repository treated as unchanged")
	}
}
