package mcpinject

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// configHome is the directory a harness keeps its user config in: the
// directory its HomeEnv names, or its default under the user's home.
func configHome(host Host, harness string) string {
	h, _ := config.LookupHarness(harness)
	if h.HomeEnv != "" {
		if dir := host.env(h.HomeEnv); dir != "" {
			return absolute(host.Workdir, dir)
		}
	}
	return filepath.Join(host.Home, h.UserConfigHome)
}

// projectDirs lists workdir and its parents up to the git root. Without a git
// root only workdir counts.
func projectDirs(workdir string) []string {
	var out []string
	for _, dir := range ancestors(workdir) {
		out = append(out, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return out
		}
	}
	return out[:1]
}

// singleFlagValues returns the value of each occurrence of a flag that takes
// exactly one, as in "--flag value" or "--flag=value".
func singleFlagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], flag+"="):
			out = append(out, strings.TrimPrefix(args[i], flag+"="))
		case args[i] == flag && i+1 < len(args):
			i++
			out = append(out, args[i])
		}
	}
	return out
}

func parsedFileSource(file string, parse func([]byte) ([]Existing, error)) source {
	return source{file: file, load: readSource(file, parse)}
}

// walkFiles calls visit for every file under root and skips directories that
// hold dependencies. A missing root is not an error.
func walkFiles(root string, visit func(path string)) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git"):
			return filepath.SkipDir
		case !d.IsDir():
			visit(path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func failingSource(file string, err error) source {
	return source{file: file, pluginProvided: true, load: func() ([]Existing, error) { return nil, err }}
}

// pluginFileSources lists one source per file called name under root, for
// files that plugins provide. A directory that cannot be walked becomes a
// source that reports the error.
func pluginFileSources(root, name string, parse func([]byte) ([]Existing, error)) []source {
	var out []source
	err := walkFiles(root, func(path string) {
		if filepath.Base(path) == name {
			src := parsedFileSource(path, parse)
			src.pluginProvided = true
			out = append(out, src)
		}
	})
	if err != nil {
		out = append(out, failingSource(root, err))
	}
	return out
}

// genericEntry covers the shapes of Copilot CLI, pi and CodeWhale server
// entries. Env and header values are decoded raw so they are dropped unread.
type genericEntry struct {
	Type       string                     `json:"type"`
	Transport  string                     `json:"transport"`
	Command    string                     `json:"command"`
	Args       []string                   `json:"args"`
	Env        map[string]json.RawMessage `json:"env"`
	URL        string                     `json:"url"`
	Headers    map[string]json.RawMessage `json:"headers"`
	EnvHeaders map[string]json.RawMessage `json:"env_headers"`
	EnvHTTP    map[string]json.RawMessage `json:"env_http_headers"`
	BearerEnv  string                     `json:"bearer_token_env_var"`
}

func decodeGenericEntry(raw json.RawMessage) Existing {
	var g genericEntry
	if err := json.Unmarshal(raw, &g); err != nil {
		return Existing{}
	}
	transport := g.Type
	if transport == "" {
		transport = g.Transport
	}
	switch {
	case transport == "local":
		transport = "stdio"
	case transport == "" && g.URL != "":
		transport = "http"
	case transport == "" && g.Command != "":
		transport = "stdio"
	}
	envKeys := sortedKeys(g.Env)
	if g.BearerEnv != "" {
		envKeys = append(envKeys, g.BearerEnv)
	}
	headerKeys := sortedKeys(g.Headers)
	headerKeys = append(headerKeys, sortedKeys(g.EnvHeaders)...)
	headerKeys = append(headerKeys, sortedKeys(g.EnvHTTP)...)
	return Existing{Transport: transport, Command: g.Command, Args: g.Args, EnvKeys: envKeys, HeaderKeys: headerKeys}
}

// parseServersUnderKeys reads the server maps under any of keys. A file with
// none of them defines no server.
func parseServersUnderKeys(raw []byte, keys ...string) ([]Existing, error) {
	doc, err := parseDoc(raw)
	if err != nil {
		return nil, err
	}
	var out []Existing
	for _, key := range keys {
		value, ok := doc[key]
		if !ok {
			continue
		}
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(value, &servers); err != nil {
			return nil, fmt.Errorf("%w: %q is not an object", errParse, key)
		}
		out = append(out, entriesOf(servers, decodeGenericEntry)...)
	}
	return out, nil
}
