package gitservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type State struct {
	Repository string            `json:"repository"`
	Session    string            `json:"session"`
	Head       string            `json:"head"`
	Branch     string            `json:"branch"`
	Index      string            `json:"index"`
	Dirty      bool              `json:"dirty"`
	Branches   map[string]string `json:"branches"`
}

type Request struct {
	ID       string   `json:"id"`
	Expected *State   `json:"expected"`
	Paths    []string `json:"paths,omitempty"`
	Message  string   `json:"message,omitempty"`
	Signoff  bool     `json:"signoff,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	Target   string   `json:"target,omitempty"`
}

type Result struct {
	ID      string `json:"id,omitempty"`
	State   *State `json:"state,omitempty"`
	Error   string `json:"error,omitempty"`
	Unknown bool   `json:"outcome_unknown,omitempty"`
}

type record struct {
	hash   [32]byte
	result Result
	status int
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	answer := func(code int, result Result) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(result) }
	if r.URL.RawQuery != "" {
		answer(400, Result{Error: "query parameters are not supported"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	var req Request
	if r.Method == http.MethodPost {
		if path != "stage" && path != "commit" && path != "branches" && path != "switch" {
			answer(404, Result{Error: "unsupported Git operation"})
			return
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil {
			answer(400, Result{Error: "invalid request: " + err.Error()})
			return
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			answer(400, Result{Error: "expected one JSON object"})
			return
		}
	} else if r.Method != http.MethodGet || (path != "status" && !strings.HasPrefix(path, "operations/")) {
		answer(404, Result{Error: "unsupported Git operation"})
		return
	}
	if !s.mu.TryLock() {
		answer(503, Result{Error: "repository busy; retry status, not a new mutation"})
		return
	}
	defer s.mu.Unlock()
	if s.closed {
		answer(503, Result{Error: "Git service closed"})
		return
	}
	if !s.ready {
		answer(503, Result{Error: "Git service is waiting for the resolved sandbox policy"})
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, operationTimeout)
	defer cancel()
	if r.Method == http.MethodGet {
		if path == "status" {
			if err := s.validateRepository(); err != nil {
				answer(409, Result{Error: err.Error()})
				return
			}
			state, err := s.state(ctx)
			if err != nil {
				answer(500, Result{Error: err.Error()})
				return
			}
			answer(200, Result{State: &state, Unknown: s.uncertain})
			return
		}
		if rec, ok := s.operations[strings.TrimPrefix(path, "operations/")]; ok {
			answer(rec.status, rec.result)
		} else {
			answer(404, Result{Error: "unknown operation; inspect status before retrying", Unknown: true})
		}
		return
	}
	if err := s.validateRequest(path, req); err != nil {
		answer(400, Result{Error: err.Error()})
		return
	}
	body, _ := json.Marshal(req)
	hash := sha256.Sum256(append([]byte(path+"\x00"), body...))
	if rec, ok := s.operations[req.ID]; ok {
		if rec.hash != hash {
			answer(409, Result{Error: "operation ID was already used for another request"})
		} else {
			answer(rec.status, rec.result)
		}
		return
	}
	if s.uncertain {
		answer(409, Result{Error: "a previous mutation has an uncertain outcome; inspect Git state and relaunch", Unknown: true})
		return
	}
	if len(s.operations) >= maxOperations {
		answer(503, Result{Error: "operation ledger full; relaunch before more mutations"})
		return
	}
	result := Result{ID: req.ID}
	status := http.StatusOK
	if err := s.validateRepository(); err != nil {
		result.Error = err.Error()
		status = 409
	} else {
		before, err := s.state(ctx)
		if err != nil {
			result.Error = err.Error()
			status = 500
		} else if before.Head != req.Expected.Head || before.Branch != req.Expected.Branch || before.Index != req.Expected.Index {
			result.Error = "repository changed since the request; inspect status"
			status = 409
		} else {
			state, changed, err := s.mutate(ctx, path, req, before)
			result.State = &state
			if err != nil {
				result.Error = err.Error()
				result.Unknown = changed
				if changed {
					result.State = nil
				}
				s.uncertain = changed
				status = 409
			}
		}
	}
	s.operations[req.ID] = record{hash: hash, result: result, status: status}
	if s.audit != nil {
		head := ""
		if result.State != nil {
			head = result.State.Head
		}
		outcome := "ok"
		if status != 200 {
			outcome = "rejected"
		}
		if result.Unknown {
			outcome = "unknown"
		}
		s.audit(path, outcome, head)
	}
	answer(status, result)
}

func (s *Service) validateRequest(action string, r Request) error {
	prefix := s.session + ":"
	if !strings.HasPrefix(r.ID, prefix) || len(r.ID) != len(prefix)+32 {
		return fmt.Errorf("operation ID must use the current session and a 32-digit hexadecimal nonce")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(r.ID, prefix)); err != nil {
		return fmt.Errorf("invalid operation ID")
	}
	if r.Expected == nil || r.Expected.Session != s.session {
		return fmt.Errorf("expected state from this session is required")
	}
	if (action != "stage" && len(r.Paths) != 0) || (action != "commit" && (r.Message != "" || r.Signoff)) || ((action == "stage" || action == "commit") && (r.Branch != "" || r.Target != "")) {
		return fmt.Errorf("fields do not belong to this operation")
	}
	if action == "stage" && (len(r.Paths) == 0 || len(r.Paths) > 256) {
		return fmt.Errorf("stage requires 1 to 256 explicit paths")
	}
	if action == "commit" && (strings.TrimSpace(r.Message) == "" || len(r.Message) > 65536 || strings.ContainsRune(r.Message, 0)) {
		return fmt.Errorf("commit requires a message of at most 65536 bytes without NUL")
	}
	if action == "branches" || action == "switch" {
		if !validBranch(r.Branch) || !validOID(r.Target) {
			return fmt.Errorf("a plain local branch name and a full SHA-1 target commit are required")
		}
	}
	return nil
}
