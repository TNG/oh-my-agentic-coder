package gitservice

import (
	"fmt"
	"strconv"
	"strings"
)

func workerConfig(name, email string, filemode bool) string {
	return fmt.Sprintf(`[core]
	repositoryformatversion = 0
	bare = false
	filemode = %t
	hooksPath = /omac/hooks
	fsmonitor = false
	untrackedCache = false
	attributesFile = /dev/null
	protectHFS = true
	protectNTFS = true
[user]
	name = %s
	email = %s
[commit]
	gpgsign = false
[tag]
	gpgsign = false
[gc]
	auto = 0
[maintenance]
	auto = false
[protocol]
	allow = never
`, filemode, strconv.Quote(name), strconv.Quote(email))
}

func supportedConfig(data []byte) (bool, error) {
	filemode := true
	formatSeen := false
	for _, item := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
		key, value, _ := strings.Cut(item, "\n")
		key = strings.ToLower(key)
		switch {
		case key == "core.repositoryformatversion":
			if value != "0" {
				return false, fmt.Errorf("unsupported Git repository format %q", value)
			}
			formatSeen = true
		case strings.HasPrefix(key, "extensions."):
			return false, fmt.Errorf("Git extensions are not supported by the prototype: %s", key)
		case key == "include.path" || strings.HasPrefix(key, "includeif.") || key == "core.attributesfile":
			return false, fmt.Errorf("prototype cannot preserve external configuration or attributes: %s", key)
		case key == "core.worktree" || key == "core.sparsecheckout" || key == "core.sparsecheckoutcone" || strings.HasSuffix(key, ".promisor") || strings.HasSuffix(key, ".partialclonefilter"):
			return false, fmt.Errorf("unsupported Git setting: %s", key)
		case key == "core.bare" || key == "core.ignorecase" || key == "core.autocrlf":
			if value != "false" {
				return false, fmt.Errorf("prototype requires %s=false", key)
			}
		case key == "core.filemode":
			if value != "true" && value != "false" {
				return false, fmt.Errorf("invalid core.filemode")
			}
			filemode = value == "true"
		case key == "core.symlinks":
			if value != "true" {
				return false, fmt.Errorf("prototype requires core.symlinks=true")
			}
		case key == "core.eol":
			if value != "lf" && value != "native" {
				return false, fmt.Errorf("prototype supports only LF line endings")
			}
		}
	}
	if !formatSeen {
		return false, fmt.Errorf("missing core.repositoryformatversion")
	}
	return filemode, nil
}
