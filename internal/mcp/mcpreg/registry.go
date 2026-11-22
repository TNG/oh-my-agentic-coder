package mcpreg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/keychain"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

const (
	recordFile   = "record.json"
	snapshotDir  = "snapshot"
	snapshotFile = "mcp.yaml"
)

// ErrNotRegistered is returned when no registration of that name exists in the scope.
var ErrNotRegistered = errors.New("mcp server not registered")

// GlobalScope is the scope of registrations that apply to every workdir.
func GlobalScope() Scope {
	return Scope{Key: GlobalScopeKey, DisplayPath: GlobalScopeKey}
}

// ScopeFor returns the project scope of workdir, keyed by the symlink-resolved path.
func ScopeFor(workdir string) Scope {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		abs = workdir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return Scope{Key: keychain.WorkdirID(abs), DisplayPath: abs}
}

func rootDir() (string, error) {
	g := registry.GlobalDir()
	if g == "" {
		return "", errors.New("cannot determine the omac config directory")
	}
	return filepath.Join(g, "mcp"), nil
}

func serverDir(s Scope, name string) (string, error) {
	if err := config.ValidSkillName(name); err != nil {
		return "", err
	}
	root, err := rootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, s.Key, name), nil
}

// IsSnapshotPath reports whether dir lies inside a registry snapshot.
func IsSnapshotPath(dir string) bool {
	root, err := rootDir()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(dir))
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) >= 3 && parts[0] != ".." && parts[2] == snapshotDir
}

// Register parses the definition file and registers it like RegisterDefinition.
func Register(s Scope, defPath string, harnesses []string) (*Entry, error) {
	raw, err := os.ReadFile(defPath)
	if err != nil {
		return nil, err
	}
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, err
	}
	return RegisterDefinition(s, def, harnesses)
}

// RegisterDefinition validates def, writes its canonical serialization into
// the host-only snapshot named by its hash and writes the record.
// Re-registering a name keeps its SecretsStored list and its older snapshots,
// which a running gateway may still use.
func RegisterDefinition(s Scope, def *Definition, harnesses []string) (*Entry, error) {
	if len(harnesses) == 0 {
		return nil, errors.New("at least one harness is required")
	}
	canonical, err := canonicalize(s, def)
	if err != nil {
		return nil, err
	}
	dir, err := serverDir(s, def.Name)
	if err != nil {
		return nil, err
	}
	rec := Record{
		Name:         def.Name,
		ScopeKey:     s.Key,
		DisplayPath:  s.DisplayPath,
		Harnesses:    slices.Clone(harnesses),
		RegisteredAt: time.Now().UTC(),
	}
	if prev, err := readRecord(dir); err == nil {
		rec.SecretsStored = prev.SecretsStored
	}
	return install(dir, rec, canonical)
}

// Update applies fn to a copy of the registered definition, validates the
// result and installs it as a new snapshot. Older snapshots stay, because a
// running gateway may still start session children in them.
func Update(s Scope, name string, fn func(*Definition) error) (*Entry, error) {
	cur, err := Load(s, name)
	if err != nil {
		return nil, err
	}
	raw, err := marshalDefinition(cur.Def)
	if err != nil {
		return nil, err
	}
	next, err := ParseDefinition(raw)
	if err != nil {
		return nil, err
	}
	if err := fn(next); err != nil {
		return nil, err
	}
	if next.Name != name {
		return nil, fmt.Errorf("update must not rename %q to %q", name, next.Name)
	}
	canonical, err := canonicalize(s, next)
	if err != nil {
		return nil, err
	}
	dir, err := serverDir(s, name)
	if err != nil {
		return nil, err
	}
	return install(dir, cur.Record, canonical)
}

func marshalDefinition(def *Definition) ([]byte, error) {
	return yaml.Marshal(def)
}

// canonicalize validates def and returns its canonical serialization.
func canonicalize(s Scope, def *Definition) ([]byte, error) {
	raw, err := marshalDefinition(def)
	if err != nil {
		return nil, err
	}
	checked, err := ParseDefinition(raw)
	if err != nil {
		return nil, err
	}
	if s.Key != GlobalScopeKey {
		if err := CheckCommandOutsideWorkdir(checked, s.DisplayPath); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// install writes the snapshot for canonical and then points the record at it.
func install(dir string, rec Record, canonical []byte) (*Entry, error) {
	def, err := ParseDefinition(canonical)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	rec.BundleHash = hex.EncodeToString(sum[:])
	snap := filepath.Join(dir, snapshotDir, rec.BundleHash)
	if err := os.MkdirAll(snap, 0o700); err != nil {
		return nil, err
	}
	defPath := filepath.Join(snap, snapshotFile)
	if err := os.WriteFile(defPath, canonical, 0o600); err != nil {
		return nil, err
	}
	if err := writeRecord(dir, rec); err != nil {
		return nil, err
	}
	return &Entry{Record: rec, Def: def, SnapshotDir: snap, DefPath: defPath}, nil
}

// Load returns the registration with its definition, re-hashing the
// snapshot and failing when it no longer matches the recorded hash.
func Load(s Scope, name string) (*Entry, error) {
	dir, err := serverDir(s, name)
	if err != nil {
		return nil, err
	}
	rec, err := readRecord(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotRegistered, name)
		}
		return nil, err
	}
	if rec.Name != name {
		return nil, fmt.Errorf("record of %q names %q", name, rec.Name)
	}
	if strings.ContainsAny(rec.BundleHash, `/\.`) || rec.BundleHash == "" {
		return nil, fmt.Errorf("record of %q has an invalid bundle hash", name)
	}
	snap := filepath.Join(dir, snapshotDir, rec.BundleHash)
	defPath := filepath.Join(snap, snapshotFile)
	raw, err := os.ReadFile(defPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot of %q: %w", name, err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != rec.BundleHash {
		return nil, fmt.Errorf("snapshot of %q does not match its recorded hash; re-register it", name)
	}
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, fmt.Errorf("snapshot of %q: %w", name, err)
	}
	return &Entry{Record: *rec, Def: def, SnapshotDir: snap, DefPath: defPath}, nil
}

// InspectResult is the load result of one registration.
type InspectResult struct {
	Name  string
	Entry *Entry
	Err   error
}

// Inspect loads every registration of the scope, sorted by name, and reports
// each result, so one corrupt registration does not hide the others.
func Inspect(s Scope) []InspectResult {
	root, err := rootDir()
	if err != nil {
		return []InspectResult{{Err: err}}
	}
	dirs, err := os.ReadDir(filepath.Join(root, s.Key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []InspectResult{{Err: err}}
	}
	var out []InspectResult
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		e, err := Load(s, d.Name())
		out = append(out, InspectResult{Name: d.Name(), Entry: e, Err: err})
	}
	return out
}

// List loads every registration of the scope, sorted by name. A registration
// that fails verification fails the whole call.
func List(s Scope) ([]*Entry, error) {
	var out []*Entry
	for _, r := range Inspect(s) {
		if r.Err != nil {
			return nil, r.Err
		}
		out = append(out, r.Entry)
	}
	return out, nil
}

// AllScopes returns every scope that holds registrations, the global scope first.
func AllScopes() ([]Scope, error) {
	root, err := rootDir()
	if err != nil {
		return nil, err
	}
	keys, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Scope
	for _, k := range keys {
		if !k.IsDir() {
			continue
		}
		sc := Scope{Key: k.Name()}
		if k.Name() == GlobalScopeKey {
			sc = GlobalScope()
		} else if path, ok := displayPathOf(filepath.Join(root, k.Name())); ok {
			sc.DisplayPath = path
		}
		out = append(out, sc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Key == GlobalScopeKey && out[j].Key != GlobalScopeKey
	})
	return out, nil
}

func displayPathOf(scopeDir string) (string, bool) {
	servers, err := os.ReadDir(scopeDir)
	if err != nil {
		return "", false
	}
	for _, d := range servers {
		if rec, err := readRecord(filepath.Join(scopeDir, d.Name())); err == nil {
			return rec.DisplayPath, true
		}
	}
	return "", false
}

// Effective returns the registrations that apply to workdir for harness:
// global and project entries merged, sorted by name. A project entry wins
// over a global one of the same name, and the names it shadows are returned.
// An entry whose Harnesses does not contain harness is excluded.
func Effective(workdir, harness string) (entries []*Entry, shadowed []string, err error) {
	global, err := List(GlobalScope())
	if err != nil {
		return nil, nil, err
	}
	project, err := List(ScopeFor(workdir))
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]*Entry{}
	for _, e := range global {
		if slices.Contains(e.Harnesses, harness) {
			byName[e.Name] = e
		}
	}
	for _, e := range project {
		if !slices.Contains(e.Harnesses, harness) {
			continue
		}
		if _, ok := byName[e.Name]; ok {
			shadowed = append(shadowed, e.Name)
		}
		byName[e.Name] = e
	}
	for _, e := range byName {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	sort.Strings(shadowed)
	return entries, shadowed, nil
}

// AppendSecretsStored adds names to the record's append-only SecretsStored list.
func AppendSecretsStored(s Scope, name string, names []string) error {
	dir, err := serverDir(s, name)
	if err != nil {
		return err
	}
	rec, err := readRecord(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !slices.Contains(rec.SecretsStored, n) {
			rec.SecretsStored = append(rec.SecretsStored, n)
		}
	}
	return writeRecord(dir, *rec)
}

// SetSecret records the secret name in the registration, then stores the
// value in the keychain, so Deregister can always find what to delete.
func SetSecret(s Scope, server, name string, value secrets.Secret) error {
	if err := AppendSecretsStored(s, server, []string{name}); err != nil {
		return err
	}
	return keychain.SetMCP(s.Key, server, name, value)
}

// GetSecret reads a secret of the registration from the keychain.
func GetSecret(s Scope, server, name string) (secrets.Secret, error) {
	return keychain.GetMCP(s.Key, server, name)
}

// Deregister deletes the keychain secrets recorded in SecretsStored, then
// the registration directory.
func Deregister(s Scope, name string) error {
	dir, err := serverDir(s, name)
	if err != nil {
		return err
	}
	rec, err := readRecord(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotRegistered, name)
		}
		return err
	}
	for _, n := range rec.SecretsStored {
		if err := keychain.DeleteMCP(s.Key, name, n); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

func readRecord(dir string) (*Record, error) {
	raw, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, recordFile), err)
	}
	return &rec, nil
}

func writeRecord(dir string, rec Record) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, recordFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, recordFile))
}
