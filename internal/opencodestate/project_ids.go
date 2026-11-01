package opencodestate

import (
	"bufio"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Newer Desktop builds no longer store folder paths for an extra server
// (e.g. the `omac serve` sandbox on http://localhost:4095): its
// "server" -> "projects" map stays empty and the store keeps only
// project IDs ("usedProjectIDs", "lastProjectID"). The ID -> folder
// mapping lives in OpenCode's own state (opencode.db, storage JSON),
// which the confined agent can write, so a looked-up path is granted
// only if the folder's own files reproduce the ID the same way OpenCode
// derives it. The agent cannot change files in a folder it has no
// access to, so it cannot make a forged path pass this check.
//
// Two accepted limits: a second clone of a repository with the same
// "origin" remote verifies too (OpenCode gives clones one ID), and a
// linked git worktree is checked through its main repository's .git, so
// it verifies whenever that repository's files do.

// globalStoreProjectIDs returns the project IDs the Desktop store
// records for local servers: "server" -> usedProjectIDs.<url> and
// "server" -> lastProjectID.<url>. Remote servers are ignored, like in
// globalStoreWorktrees.
func globalStoreProjectIDs(top map[string]json.RawMessage) []string {
	raw, ok := top["server"]
	if !ok {
		return nil
	}
	var srv struct {
		Used map[string][]string `json:"usedProjectIDs"`
		Last map[string]string   `json:"lastProjectID"`
	}
	if json.Unmarshal(unwrapValue(raw), &srv) != nil {
		return nil
	}
	var out []string
	for server, ids := range srv.Used {
		if isLocalServer(server) {
			out = append(out, ids...)
		}
	}
	for server, id := range srv.Last {
		if isLocalServer(server) {
			out = append(out, id)
		}
	}
	return out
}

// verifiedProjectWorktrees resolves trusted project IDs to folders via
// OpenCode's project records and keeps only the folders whose files
// reproduce the ID (see idMatchesWorktree).
func verifiedProjectWorktrees(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		if id != "" && id != "global" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, rec := range projectRecords() {
		if !want[rec.ID] || seen[rec.Worktree] {
			continue
		}
		if idMatchesWorktree(rec.ID, rec.Worktree) {
			seen[rec.Worktree] = true
			out = append(out, rec.Worktree)
		}
	}
	return out
}

// projectRecords returns every id/worktree pair from OpenCode's
// storage JSON files and opencode.db. These sources are agent-writable,
// so callers must verify a record before trusting it.
func projectRecords() []projectRecord {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []projectRecord
	if dir, derr := storageProjectDir(); derr == nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				continue
			}
			var rec projectRecord
			if json.Unmarshal(data, &rec) == nil {
				out = append(out, rec)
			}
		}
	}
	return append(out, dbProjectRecords(filepath.Join(home, ".local", "share", "opencode", "opencode.db"))...)
}

// dbProjectRecords reads the id/worktree pairs of opencode.db's project
// table, read-only. Any error yields nil.
func dbProjectRecords(path string) []projectRecord {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout=500")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, worktree FROM project`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []projectRecord
	for rows.Next() {
		var rec projectRecord
		if rows.Scan(&rec.ID, &rec.Worktree) == nil {
			out = append(out, rec)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// idMatchesWorktree reports whether OpenCode would derive project ID id
// for folder wt. OpenCode (packages/core/src/project.ts) uses, in order:
//
//   - git: sha1("git-remote:<host>/<path>") of the "origin" remote, else
//     the ID cached in <git common dir>/opencode, else the root commit;
//   - no git, or git discovery failed: sha1("directory:" + wt).
//
// Each candidate is computed from files inside wt only. The root-commit
// form is not checked: it would need git to run in a folder named by
// agent-writable data, and git executes hooks and config from the repo.
// Such projects keep needing an explicit profile grant.
//
// wt must be a clean absolute path to a real directory, not a symlink:
// OpenCode stores resolved paths, so a symlink in its place means the
// folder was replaced after OpenCode recorded it.
func idMatchesWorktree(id, wt string) bool {
	if id == "" || id == "global" || !filepath.IsAbs(wt) || filepath.Clean(wt) != wt {
		return false
	}
	fi, err := os.Lstat(wt)
	if err != nil || !fi.IsDir() {
		return false
	}
	if sha1Hex("directory:"+wt) == id {
		return true
	}
	common := gitCommonDir(wt)
	if common == "" {
		return false
	}
	if data, err := os.ReadFile(filepath.Join(common, "opencode")); err == nil {
		if cached := strings.TrimSpace(string(data)); cached != "" && cached == id {
			return true
		}
	}
	if n := normalizeRemote(originURL(filepath.Join(common, "config"))); n != "" {
		return sha1Hex("git-remote:"+n) == id
	}
	return false
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// gitCommonDir returns the git common directory of repository root wt:
// wt/.git for a normal repo, or the commondir a linked worktree's
// ".git" file points at. Returns "" when wt is not a repository root.
func gitCommonDir(wt string) string {
	dotGit := filepath.Join(wt, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	gitDir := dotGit
	if !fi.IsDir() {
		data, err := os.ReadFile(dotGit)
		if err != nil {
			return ""
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return ""
		}
		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(wt, gitDir)
		}
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(data))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		return filepath.Clean(c)
	}
	return gitDir
}

// originURL returns the url of [remote "origin"] in a git config file,
// or "". Only the plain form OpenCode's `git remote get-url origin`
// usually sees is supported; url.<base>.insteadOf rewrites are not
// applied.
func originURL(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "url" {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

var (
	urlSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	scpRemoteRe = regexp.MustCompile(`^([^@/:]+@)?([^/:]+):(.+)$`)
)

// normalizeRemote mirrors OpenCode's url()/parts(): "<host>/<path>"
// with the host lowercased and a trailing ".git" and slashes removed.
// Examples: "git@github.com:TNG/x.git" and "https://github.com/TNG/x"
// both become "github.com/TNG/x". file:// remotes yield "".
func normalizeRemote(remote string) string {
	v := strings.TrimSpace(remote)
	if v == "" {
		return ""
	}
	var host, name string
	if urlSchemeRe.MatchString(v) {
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		host, name = u.Hostname(), u.EscapedPath()
	} else if m := scpRemoteRe.FindStringSubmatch(v); m != nil {
		host, name = m[2], m[3]
	} else {
		return ""
	}
	name = strings.TrimLeft(name, "/")
	name = strings.TrimSuffix(strings.TrimSuffix(name, "/"), ".git")
	name = strings.TrimRight(name, "/")
	if host == "" || name == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + name
}
