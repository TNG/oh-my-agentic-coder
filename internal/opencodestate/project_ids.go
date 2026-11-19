package opencodestate

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A Desktop ID authorizes a directory hash or repository identity, not an
// arbitrary database mapping. Git clones/worktrees can legitimately share IDs.
// Root-commit-only repositories need an explicit grant; never run git in a
// folder selected by agent-writable data just to establish its identity.
func idMatchesWorktree(id, path string) bool {
	if id == "" || id == "global" || !filepath.IsAbs(path) {
		return false
	}
	if projectHash("directory:"+path) == id {
		return true
	}
	gitdir := filepath.Join(path, ".git")
	fi, err := os.Lstat(gitdir)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		data, err := readRegular(gitdir, metadataMaxBytes)
		if err != nil {
			return false
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return false
		}
		gitdir = relativeMetadataPath(path, target)
	}
	if common, err := readRegular(filepath.Join(gitdir, "commondir"), metadataMaxBytes); err == nil {
		gitdir = relativeMetadataPath(gitdir, string(common))
	} else if !os.IsNotExist(err) {
		return false
	}
	if data, err := readRegular(filepath.Join(gitdir, "opencode"), metadataMaxBytes); err == nil {
		if strings.TrimSpace(string(data)) == id {
			return true
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	data, err := readRegular(filepath.Join(gitdir, "config"), metadataMaxBytes)
	if err != nil {
		return false
	}
	inOrigin := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if key, value, ok := strings.Cut(line, "="); inOrigin && ok && strings.TrimSpace(key) == "url" {
			remote := normalizedRemote(strings.Trim(strings.TrimSpace(value), `"`))
			return remote != "" && projectHash("git-remote:"+remote) == id
		}
	}
	return false
}

func relativeMetadataPath(base, target string) string {
	target = strings.TrimSpace(target)
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(base, target)
}

func projectHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

var scpRemote = regexp.MustCompile(`^(?:[^@/:]+@)?([^/:]+):(.+)$`)

func normalizedRemote(remote string) string {
	var host, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		host, path = u.Hostname(), u.EscapedPath()
	} else if parts := scpRemote.FindStringSubmatch(remote); parts != nil {
		host, path = parts[1], parts[2]
	}
	path = strings.TrimRight(strings.TrimSuffix(strings.Trim(strings.TrimSpace(path), "/"), ".git"), "/")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + path
}
