package herdr

import (
	"os"
	"path/filepath"
	"strings"
)

// FileProbe implements Probe by stat'ing a fixed set of paths every
// call: plain files (a session db) and directories whose entries are
// stat'ed individually (a per-project dir of session files). Any mtime
// or size difference counts as activity, and new directory entries are
// surfaced as candidate session ids via the registered ID extractor.
//
// It is intentionally dumb: no parsing, no hashing, just stat calls.
// A missing path is a valid observation (the store may not exist yet
// for a fresh workdir), and a path appearing later is a change.
//
// Not concurrency-safe by itself; the bridge calls it from one
// goroutine only.
type FileProbe struct {
	// paths are the files and dirs to watch, in order.
	paths []string
	// idOf maps a watched dir to the extractor turning an entry name
	// into a candidate session id ("" ignores the entry).
	idOf map[string]func(entry string) string

	// snap holds the last observed stamp per file path (key: path) and
	// per dir entry (key: path + "\x00" + entry).
	snap      map[string]stamp
	baselined bool
}

// stamp is what FileProbe compares: existence plus mtime and size.
type stamp struct {
	exists  bool
	modNano int64
	size    int64
}

// NewFileProbe builds a probe over the given files and directories.
func NewFileProbe(paths ...string) *FileProbe {
	return &FileProbe{
		paths: paths,
		idOf:  map[string]func(string) string{},
		snap:  map[string]stamp{},
	}
}

// DirIDs registers an id extractor for a directory path. The extractor
// receives a raw entry name (e.g. "ses_abc.jsonl") and returns the
// candidate session id ("" to ignore the entry).
func (p *FileProbe) DirIDs(dir string, f func(entry string) string) {
	p.idOf[dir] = f
}

// Probe advances the snapshot. The first call baselines and reports no
// change; every later call reports changed=true when any watched stamp
// differs (mtime, size, or existence) and ids for entries that appeared
// since the previous call.
func (p *FileProbe) Probe() (changed bool, ids []string) {
	// Collect observations into a fresh map so a vanished path/entry is
	// detected as a removal against the previous snapshot.
	next := map[string]stamp{}
	for _, path := range p.paths {
		info, err := os.Stat(path)
		if err != nil {
			continue // missing is an observation, recorded below
		}
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				continue
			}
			for _, e := range entries {
				ei, err := e.Info()
				if err != nil {
					continue
				}
				next[path+"\x00"+e.Name()] = stamp{exists: true, modNano: ei.ModTime().UnixNano(), size: ei.Size()}
			}
			continue
		}
		next[path] = stamp{exists: true, modNano: info.ModTime().UnixNano(), size: info.Size()}
	}

	defer func() { p.snap = next; p.baselined = true }()
	if !p.baselined {
		return false, nil
	}
	for key, st := range next {
		if prev, ok := p.snap[key]; !ok || prev != st {
			changed = true
			// New entries in a dir with an id extractor are candidate
			// session ids; plain file changes are just activity.
			if !ok {
				if i := strings.IndexByte(key, 0); i >= 0 {
					dir, entry := key[:i], key[i+1:]
					if extract, watched := p.idOf[dir]; watched {
						if id := extract(entry); id != "" {
							ids = append(ids, id)
						}
					}
				}
			}
		}
	}
	// A path/entry that vanished is also activity.
	for key := range p.snap {
		if _, ok := next[key]; !ok {
			changed = true
		}
	}
	return changed, ids
}

// IDsFromJSONL turns a claude session-store entry name into the session
// id herdr should resume: the base name without its .jsonl extension.
// Non-jsonl entries yield "".
func IDsFromJSONL(entry string) string {
	name := filepath.Base(entry)
	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(name, ".jsonl")
}
