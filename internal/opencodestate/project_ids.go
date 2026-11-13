package opencodestate

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// only if the canonical path or repository identity matches that trusted ID.
// Directory hashes bind path spelling, not contents; their authority comes
// from the protected Desktop store. Git IDs describe a repository, not one
// particular clone's path.
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

func verifiedProjectRecords(ids []string, records []projectRecord) []string {
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
	for _, rec := range records {
		if !want[rec.ID] {
			continue
		}
		canonical, err := canonicalWorktree(rec.Worktree)
		if err == nil && !seen[canonical] && idMatchesWorktree(rec.ID, canonical) {
			seen[canonical] = true
			out = append(out, canonical)
		}
	}
	return out
}

// projectRecords returns every id/worktree pair from OpenCode's
// storage JSON files and opencode.db. These sources are agent-writable,
// so callers must verify a record before trusting it.
func projectRecords() []projectRecord {
	records, _, _ := readProjectSources()
	return records
}

// dbProjectRecords reads the id/worktree pairs of opencode.db's project
// table, read-only. Any error yields nil.
func dbProjectRecords(path string) []projectRecord {
	records, _ := readDBProjectRecords(path)
	return records
}

// idMatchesWorktree reports whether OpenCode would derive project ID id
// for folder wt. OpenCode (packages/core/src/project.ts) uses, in order:
//
//   - git: sha1("git-remote:<host>/<path>") of the "origin" remote, else
//     the ID cached in <git common dir>/opencode, else the root commit;
//   - no git, or git discovery failed: sha1("directory:" + wt).
//
// The directory form hashes canonical path spelling. Git metadata may live
// outside wt through legitimate gitdir/commondir references. The root-commit
// form is not checked: it would need git to run in a folder named by
// agent-writable data, and git executes hooks and config from the repo.
// Such projects keep needing an explicit profile grant.
//
// Validation and the eventual grant both refer to the resolved directory.
func idMatchesWorktree(id, wt string) bool {
	if id == "" || id == "global" {
		return false
	}
	wt, err := canonicalWorktree(wt)
	if err != nil {
		return false
	}
	if sha1Hex("directory:"+wt) == id {
		return true
	}
	common := gitCommonDir(wt)
	if common == "" {
		return false
	}
	if data, err := readMetadata(filepath.Join(common, "opencode")); err == nil {
		if cached := strings.TrimSpace(string(data)); cached != "" && cached == id {
			return true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
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
	fi, err := os.Lstat(dotGit)
	if err != nil {
		return ""
	}
	gitDir := dotGit
	if !fi.IsDir() {
		data, err := readMetadata(dotGit)
		if err != nil {
			return ""
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return ""
		}
		gitDir = strings.TrimSpace(target)
		if gitDir == "" {
			return ""
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(wt, gitDir)
		}
	}
	gitDir = filepath.Clean(gitDir)
	if !isMetadataDir(gitDir) {
		return ""
	}
	if data, err := readMetadata(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(data))
		if c == "" {
			return ""
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		c = filepath.Clean(c)
		if !isMetadataDir(c) {
			return ""
		}
		return c
	} else if !errors.Is(err, os.ErrNotExist) {
		return ""
	}
	return gitDir
}

// Git references may leave the worktree, but not traverse symlink metadata.
func isMetadataDir(path string) bool {
	canonical, err := canonicalWorktree(path)
	return err == nil && canonical == path
}

// originURL returns the url of [remote "origin"] in a git config file,
// or "". Only the plain form OpenCode's `git remote get-url origin`
// usually sees is supported; url.<base>.insteadOf rewrites are not
// applied.
func originURL(configPath string) string {
	data, err := readMetadata(configPath)
	if err != nil {
		return ""
	}
	inOrigin := false
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 4096), metadataMaxBytes+1)
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
