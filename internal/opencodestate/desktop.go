package opencodestate

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readDesktopStore(path string) (map[string]json.RawMessage, error) {
	top := map[string]json.RawMessage{}
	if filepath.Ext(path) != ".sqlite" {
		data, err := readRegular(path, metadataMaxBytes)
		if err != nil {
			return nil, err
		}
		if json.Unmarshal(data, &top) != nil {
			return nil, errSource
		}
		return top, nil
	}
	// Ignore unrelated store keys before loading their values. Values may be
	// arbitrary strings, not JSON, and Desktop can store large drafts here.
	rows, err := readSQLite(path, "state", `SELECT key, value FROM state
 WHERE name = 'opencode.global.dat' AND
 (CAST(key AS BLOB) IN (CAST('server' AS BLOB), CAST('globalSync.project' AS BLOB), CAST('layout.page' AS BLOB))
 OR instr(CAST(key AS BLOB), x'006c61796f75742e70616765') > 0)
 AND length(CAST(key AS BLOB)) <= 4096 AND length(CAST(value AS BLOB)) <= 1048576 LIMIT 10001`)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		top[row[0]] = json.RawMessage(row[1])
	}
	return top, nil
}

func desktopProjects(port int) ([]string, map[string]bool, []SkippedWorktree) {
	var paths []string
	ids := map[string]bool{}
	var diagnostics []SkippedWorktree
	seen := map[string]bool{}
	for _, path := range DesktopStorePaths() {
		if seen[path] || (filepath.Base(path) != "drafts.sqlite" && filepath.Base(path) != "opencode.global.dat") {
			continue
		}
		seen[path] = true
		top, err := readDesktopStore(path)
		if err != nil {
			if !os.IsNotExist(err) {
				diagnostics = append(diagnostics, SkippedWorktree{Path: path, Reason: SkipInvalidSource})
			}
			continue
		}
		var sync struct {
			Value []projectRecord `json:"value"`
		}
		if json.Unmarshal(unwrapValue(top["globalSync.project"]), &sync) == nil {
			for _, rec := range sync.Value {
				paths = append(paths, rec.Worktree)
			}
		}
		for key, raw := range top {
			server, name, scoped := strings.Cut(key, "\x00")
			if key != "layout.page" && !(scoped && name == "layout.page" && desktopServer(server, port)) {
				continue
			}
			var page struct {
				Sessions map[string]struct {
					Directory string `json:"directory"`
				} `json:"lastProjectSession"`
			}
			if json.Unmarshal(unwrapValue(raw), &page) == nil {
				for dir, session := range page.Sessions {
					if session.Directory != "" {
						dir = session.Directory
					}
					paths = append(paths, dir)
				}
			}
		}
		var server struct {
			Projects map[string][]projectRecord `json:"projects"`
			Used     map[string][]string        `json:"usedProjectIDs"`
			Last     map[string]string          `json:"lastProjectID"`
		}
		if json.Unmarshal(unwrapValue(top["server"]), &server) == nil {
			for key, records := range server.Projects {
				if desktopServer(key, port) {
					for _, rec := range records {
						paths = append(paths, rec.Worktree)
					}
				}
			}
			for key, used := range server.Used {
				if desktopServer(key, port) {
					for _, id := range used {
						ids[id] = true
					}
				}
			}
			for key, id := range server.Last {
				if desktopServer(key, port) {
					ids[id] = true
				}
			}
		}
	}
	delete(ids, "")
	delete(ids, "global")
	return paths, ids, diagnostics
}

func desktopServer(server string, port int) bool {
	if server == "local" || server == "sidecar" {
		return true
	}
	u, err := url.Parse(server)
	if err != nil || port <= 0 || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	serverPort := u.Port()
	if serverPort == "" {
		if u.Scheme == "https" {
			serverPort = "443"
		} else {
			serverPort = "80"
		}
	}
	ip := net.ParseIP(u.Hostname())
	return serverPort == strconv.Itoa(port) && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}
