package gitservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

func validOID(s string) bool {
	if len(s) != 40 || s == zeroOID {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func validBranch(s string) bool {
	if s == "" || s == "HEAD" || len(s) > 255 || strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") || strings.Contains(s, "..") || strings.Contains(s, "//") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._/", c)) {
			return false
		}
	}
	return true
}

func (s *Service) state(ctx context.Context) (State, error) {
	out := State{Repository: s.root, Session: s.session, Branches: map[string]string{}}
	branch, err := s.run(ctx, false, false, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return out, fmt.Errorf("prototype requires an attached local branch: %w", err)
	}
	ref := strings.TrimSpace(string(branch))
	out.Branch = strings.TrimPrefix(ref, "refs/heads/")
	if !strings.HasPrefix(ref, "refs/heads/") || !validBranch(out.Branch) {
		return out, fmt.Errorf("unsupported HEAD branch name")
	}
	refs, err := s.run(ctx, false, false, nil, "for-each-ref", "--format=%(refname:strip=2) %(objectname)", "refs/heads/")
	if err != nil {
		return out, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		if line == "" {
			continue
		}
		name, oid, ok := strings.Cut(line, " ")
		if !ok || !validOID(oid) {
			return out, fmt.Errorf("invalid branch reference")
		}
		out.Branches[name] = oid
	}
	out.Head = out.Branches[out.Branch]
	index, err := s.run(ctx, false, false, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return out, err
	}
	if _, err := indexPaths(index); err != nil {
		return out, err
	}
	hash := sha256.Sum256(index)
	out.Index = hex.EncodeToString(hash[:])
	status, err := s.run(ctx, false, false, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return out, err
	}
	out.Dirty = len(status) != 0
	return out, nil
}

func indexPaths(data []byte) ([]string, error) {
	var paths []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
		if line == "" {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" || (fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000") {
			return nil, fmt.Errorf("submodules, sparse indexes and unmerged entries are not supported")
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (s *Service) checkAttributes(ctx context.Context, paths []string, source string) error {
	if len(paths) == 0 {
		return nil
	}
	args := []string{"check-attr", "-z", "--stdin"}
	if source == "index" {
		args = append(args, "--cached")
	} else if source != "" {
		args = append(args, "--source="+source)
	}
	args = append(args, "filter", "working-tree-encoding")
	data, err := s.run(ctx, false, false, []byte(strings.Join(paths, "\x00")+"\x00"), args...)
	if err != nil {
		return err
	}
	items := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(items)%3 != 0 {
		return fmt.Errorf("invalid Git attributes response")
	}
	for i := 0; i < len(items); i += 3 {
		if items[i+2] != "unspecified" && items[i+2] != "unset" {
			return fmt.Errorf("unsupported %s attribute on %q", items[i+1], items[i])
		}
	}
	return nil
}

func (s *Service) mutate(ctx context.Context, action string, req Request, before State) (State, bool, error) {
	changed := false
	run := func(args ...string) ([]byte, error) {
		changed = true
		return s.run(ctx, true, action == "switch", nil, args...)
	}
	switch action {
	case "stage":
		for _, p := range req.Paths {
			if err := s.validatePath(p, false); err != nil {
				return before, false, err
			}
		}
		if err := s.checkAttributes(ctx, req.Paths, ""); err != nil {
			return before, false, err
		}
		if _, err := run(append([]string{"add", "--"}, req.Paths...)...); err != nil {
			return before, changed, err
		}
		diff, err := s.run(ctx, false, false, nil, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "--"}, req.Paths...)...)
		if err != nil || len(diff) != 0 {
			return before, true, fmt.Errorf("staging did not produce the requested working-tree state; inspect status")
		}
	case "commit":
		index, err := s.run(ctx, false, false, nil, "ls-files", "--stage", "-z")
		if err != nil {
			return before, false, err
		}
		paths, err := indexPaths(index)
		if err != nil {
			return before, false, err
		}
		for _, p := range paths {
			if err := s.validatePath(p, true); err != nil {
				return before, false, err
			}
		}
		if err := s.checkAttributes(ctx, paths, "index"); err != nil {
			return before, false, err
		}
		tree, err := run("write-tree")
		if err != nil {
			return before, changed, err
		}
		treeID := strings.TrimSpace(string(tree))
		if !validOID(treeID) {
			return before, changed, fmt.Errorf("invalid staged tree")
		}
		if before.Head != "" {
			oldTree, err := s.run(ctx, false, false, nil, "rev-parse", before.Head+"^{tree}")
			if err != nil {
				return before, changed, err
			}
			if strings.TrimSpace(string(oldTree)) == treeID {
				return before, false, fmt.Errorf("nothing staged to commit")
			}
		}
		message := req.Message
		if req.Signoff {
			message = strings.TrimRight(message, "\n") + "\n\nSigned-off-by: " + s.name + " <" + s.email + ">\n"
		}
		args := []string{"commit-tree", treeID, "-m", message}
		if before.Head != "" {
			args = append(args, "-p", before.Head)
		}
		commit, err := run(args...)
		if err != nil {
			return before, changed, err
		}
		oid := strings.TrimSpace(string(commit))
		if !validOID(oid) {
			return before, true, fmt.Errorf("invalid commit ID")
		}
		old := before.Head
		if old == "" {
			old = zeroOID
		}
		if _, err := run("update-ref", "-m", "commit (omac)", "refs/heads/"+before.Branch, oid, old); err != nil {
			return before, changed, err
		}
		state, err := s.state(ctx)
		if err != nil || state.Head != oid || state.Index != before.Index || state.Branch != before.Branch {
			return state, true, fmt.Errorf("commit postcondition failed; inspect status")
		}
		return state, true, nil
	case "branches":
		if _, exists := before.Branches[req.Branch]; exists {
			return before, false, fmt.Errorf("branch already exists")
		}
		if _, err := s.run(ctx, false, false, nil, "cat-file", "-e", req.Target+"^{commit}"); err != nil {
			return before, false, err
		}
		if _, err := run("update-ref", "refs/heads/"+req.Branch, req.Target, zeroOID); err != nil {
			return before, changed, err
		}
	case "switch":
		if before.Dirty {
			return before, false, fmt.Errorf("switch requires a clean working tree and index, including untracked files")
		}
		if before.Branches[req.Branch] != req.Target {
			return before, false, fmt.Errorf("target branch changed or does not exist")
		}
		data, err := s.run(ctx, false, false, nil, "ls-tree", "-r", "-z", req.Target)
		if err != nil {
			return before, false, err
		}
		var paths []string
		for _, entry := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			meta, p, ok := strings.Cut(entry, "\t")
			if !ok || !strings.Contains(meta, " blob ") {
				return before, false, fmt.Errorf("target contains unsupported entries")
			}
			if err := s.validatePath(p, true); err != nil {
				return before, false, err
			}
			paths = append(paths, p)
		}
		if err := s.checkAttributes(ctx, paths, req.Target); err != nil {
			return before, false, err
		}
		if _, err := run("switch", "--no-guess", "--no-overwrite-ignore", req.Branch); err != nil {
			return before, changed, err
		}
	}
	after, err := s.state(ctx)
	if err != nil {
		return before, changed, err
	}
	if action == "branches" && (after.Branches[req.Branch] != req.Target || after.Head != before.Head || after.Branch != before.Branch || after.Index != before.Index) {
		return after, true, fmt.Errorf("branch postcondition failed; inspect status")
	}
	if action == "switch" && (after.Branch != req.Branch || after.Head != req.Target || after.Dirty) {
		return after, true, fmt.Errorf("switch postcondition failed; inspect status")
	}
	return after, changed, nil
}
