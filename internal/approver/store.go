// Package approver decides sidecar requests the facade gate hands it: it
// consults the stored verdicts, raises the native dialog when none applies,
// and records the answer. The always-verdicts are persisted host-side next to
// the skill approvals (outside every sandbox grant).
package approver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
)

const (
	fileName      = "endpoint-verdicts.json"
	schemaVersion = 1
)

// Verdict is one stored "allow always" answer.
type Verdict struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"` // what the verdict is bound to: a workdir hash
	Skill     string    `json:"skill"`
	Match     string    `json:"match"` // the declared pattern, e.g. "POST /tools/conversations_add_message"
	Approval  string    `json:"approval"`
	CreatedAt time.Time `json:"created_at"`
}

type file struct {
	Version  int       `json:"version"`
	Verdicts []Verdict `json:"verdicts"`
}

// Store persists always-verdicts in <dir>/endpoint-verdicts.json. A missing
// file is an empty store; an unreadable one is an error and the caller treats
// it as "no verdict" (the dialog is raised instead).
type Store struct {
	dir string
}

// NewStore returns the store under dir; "" means the user-global omac config
// directory (~/.config/omac), which the sandbox cannot read or write.
func NewStore(dir string) *Store {
	if dir == "" {
		dir = registry.GlobalDir()
	}
	return &Store{dir: dir}
}

// VerdictID is the stable identity of a verdict: scope, skill and the
// declared entry. A changed declaration (different match) is a different
// verdict, so an edit to the approved skill cannot inherit an old answer.
func VerdictID(scope, skill, match, approval string) string {
	h := sha256.Sum256([]byte(scope + "\x00" + skill + "\x00" + match + "\x00" + approval))
	return hex.EncodeToString(h[:8])
}

func (s *Store) path() string { return filepath.Join(s.dir, fileName) }

func (s *Store) load() (*file, error) {
	if s.dir == "" {
		return &file{Version: schemaVersion}, nil
	}
	raw, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return &file{Version: schemaVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read endpoint verdicts: %w", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse endpoint verdicts: %w", err)
	}
	if f.Version == 0 {
		f.Version = schemaVersion
	}
	return &f, nil
}

// Has reports whether an always-verdict with this id exists.
func (s *Store) Has(id string) (bool, error) {
	f, err := s.load()
	if err != nil {
		return false, err
	}
	for _, v := range f.Verdicts {
		if v.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// List returns every stored verdict, ordered by skill and match.
func (s *Store) List() ([]Verdict, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Verdicts, nil
}

// Add stores v (replacing one with the same id).
func (s *Store) Add(v Verdict) error {
	if s.dir == "" {
		return errors.New("endpoint verdicts: no host-only config directory available (set $HOME or $XDG_CONFIG_HOME)")
	}
	return s.mutate(func(f *file) {
		for i := range f.Verdicts {
			if f.Verdicts[i].ID == v.ID {
				f.Verdicts[i] = v
				return
			}
		}
		f.Verdicts = append(f.Verdicts, v)
	})
}

// Revoke removes the verdict with this id and reports whether it existed.
func (s *Store) Revoke(id string) (bool, error) {
	found := false
	err := s.mutate(func(f *file) {
		kept := f.Verdicts[:0]
		for _, v := range f.Verdicts {
			if v.ID == id {
				found = true
				continue
			}
			kept = append(kept, v)
		}
		f.Verdicts = kept
	})
	return found, err
}

func (s *Store) mutate(fn func(*file)) error {
	if s.dir == "" {
		return errors.New("endpoint verdicts: no host-only config directory available")
	}
	return registry.WithLockAt(s.dir, filepath.Join(s.dir, fileName+".lock"), func() error {
		f, err := s.load()
		if err != nil {
			return err
		}
		fn(f)
		return s.save(f)
	})
}

func (s *Store) save(f *file) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("ensure verdicts dir: %w", err)
	}
	sort.SliceStable(f.Verdicts, func(i, j int) bool {
		if f.Verdicts[i].Skill != f.Verdicts[j].Skill {
			return f.Verdicts[i].Skill < f.Verdicts[j].Skill
		}
		return f.Verdicts[i].Match < f.Verdicts[j].Match
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, fileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(e error) error { tmp.Close(); os.Remove(name); return e }
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path()); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
