//go:build linux

package gitservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusHandlesLargeUntrackedTrees(t *testing.T) {
	for _, folder := range []string{"build", ""} {
		t.Run("folder="+folder, func(t *testing.T) {
			dir := gitFixture(t)
			s := newTestService(t, dir)
			for i := 0; i < 18000; i++ {
				writeFile(t, filepath.Join(dir, folder, fmt.Sprintf("%06d-%s", i, strings.Repeat("x", 235))), "")
			}
			state := status(t, s)
			if !state.Dirty {
				t.Fatal("untracked files missed")
			}
			writeFile(t, filepath.Join(dir, "source"), "source")
			mutate(t, s, "stage", Request{Paths: []string{"source"}})
		})
	}
}

func TestMutationResultsHaveCompactStableReplay(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	writeFile(t, filepath.Join(dir, "source"), "one")
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	mutate(t, s, "commit", Request{Message: "initial"})
	state := status(t, s)
	req := Request{ID: fmt.Sprintf("%s:%032x", state.Session, 100), Expected: &state, Branch: "topic", Target: state.Head}
	code, first := call(t, s, "branches", &req)
	if code != 200 || first.State == nil || first.State.Branches != nil {
		t.Fatalf("mutation response: %d %+v", code, first)
	}
	if len(status(t, s).Branches) != 2 {
		t.Fatal("status lost branches")
	}
	for _, rec := range s.operations {
		if rec.result.State != nil && rec.result.State.Branches != nil {
			t.Fatal("ledger retains branch map")
		}
	}
	for _, action := range []string{"branches", "operations/" + req.ID} {
		var request *Request
		if action == "branches" {
			request = &req
		}
		code, replay := call(t, s, action, request)
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(replay)
		if code != 200 || string(a) != string(b) {
			t.Fatalf("unstable replay: %d %s != %s", code, a, b)
		}
	}
}

func TestDirtyStatusIncludesIndexWorktreeAndUntracked(t *testing.T) {
	dir := gitFixture(t)
	s := newTestService(t, dir)
	if status(t, s).Dirty {
		t.Fatal("empty repository dirty")
	}
	writeFile(t, filepath.Join(dir, "source"), "one")
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
	if !status(t, s).Dirty {
		t.Fatal("staged changes missed")
	}
	mutate(t, s, "commit", Request{Message: "initial"})
	writeFile(t, filepath.Join(dir, ".gitignore"), "ignored\n")
	mutate(t, s, "stage", Request{Paths: []string{".gitignore"}})
	mutate(t, s, "commit", Request{Message: "ignore"})
	writeFile(t, filepath.Join(dir, "ignored"), "hidden")
	if status(t, s).Dirty {
		t.Fatal("ignored file makes repository dirty")
	}
	writeFile(t, filepath.Join(dir, "source"), "two")
	if !status(t, s).Dirty {
		t.Fatal("working-tree change missed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.state(ctx); err == nil {
		t.Fatal("worker error ignored")
	}
}

func TestStatusOutputPresenceDoesNotHideWorkerErrors(t *testing.T) {
	s := newTestService(t, gitFixture(t))
	var output outputPresence
	err := s.runTo(context.Background(), false, false, nil, &output, "-c", "alias.partial=!printf dirty; exit 1", "partial")
	if err == nil || !output.nonempty {
		t.Fatalf("partial output hid worker failure: output=%t err=%v", output.nonempty, err)
	}
}
