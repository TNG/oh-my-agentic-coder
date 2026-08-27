// Package keychain is a thin wrapper over github.com/zalando/go-keyring.
//
// Naming convention (matches oh-my-agentic-coder.md §16.3):
//
//	service = "omac/<skill-name>"                       (legacy / global skill)
//	service = "omac/<workdir-id>/<skill-name>"          (serve-mode, per-workdir)
//	service = "omac/__defaults__/<skill-name>"          (remembered defaults)
//	account = <secret-name>
//
// The unscoped form (omac/<skill>) is what single-workdir `omac start` and
// user-global skills use; it is the backward-compatible default. Serve mode
// isolates workdir-local skills by keying on a persistent workdir-id
// (sha256 of the absolute workdir) so two directories holding a same-named
// skill — or two versions of it — don't share a credential. See
// docs/MULTI_DIR_DESKTOP.md §4.3/§8.2.
//
// The backend (macOS Keychain, Secret Service, Windows Credential Manager)
// is selected by go-keyring based on the host OS. A file-based fallback for
// headless Linux (age-encrypted secrets.age, oh-my-agentic-coder.md §16.2)
// is not implemented; on WSL / headless Linux without a running Secret
// Service provider, keychain operations fail — see IsUnavailable and
// docs/INSTALLATION.md#prerequisites for the actionable fix.
package keychain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// ErrNotFound is returned when a secret is not present in the keychain.
var ErrNotFound = errors.New("keychain: secret not found")

// ErrUnavailable reports that the keychain BACKEND itself is missing (no
// Secret Service daemon on headless Linux/WSL, no keychain daemon on macOS),
// as opposed to the secret being absent from a working keychain.
//
// Read-path errors wrap ErrNotFound AND ErrUnavailable together, so a caller
// that only checks errors.Is(err, ErrNotFound) keeps behaving exactly as
// before, while one that wants to tell a broken environment from an unset
// secret checks ErrUnavailable first. Callers that report the difference to a
// human should attach UnavailableHint.
var ErrUnavailable = errors.New("keychain: backend unavailable")

// DefaultsScope is the reserved workdir-id under which "last-known-good"
// default secret values are mirrored (docs/MULTI_DIR_DESKTOP.md §4.4). It
// is never a real workdir.
const DefaultsScope = "__defaults__"

// WorkdirID returns the persistent identity for a workdir used to scope
// secrets in serve mode: the hex sha256 of the absolute path.
func WorkdirID(absWorkdir string) string {
	sum := sha256.Sum256([]byte(absWorkdir))
	return hex.EncodeToString(sum[:])
}

// Service returns the unscoped service identifier for a skill name
// (omac/<skill>). Used by single-workdir start and by user-global skills.
func Service(skillName string) string {
	return "omac/" + skillName
}

// ScopedService returns the service identifier for a (scope, skill) pair.
// An empty scope yields the unscoped Service form, so callers that don't
// opt into scoping behave exactly as before. A non-empty scope (a
// workdir-id or DefaultsScope) yields "omac/<scope>/<skill>".
func ScopedService(scope, skillName string) string {
	if scope == "" {
		return Service(skillName)
	}
	return "omac/" + scope + "/" + skillName
}

// Set stores a secret for (skill, name) in the unscoped service.
// Overwrites any existing value.
func Set(skillName, name string, value secrets.Secret) error {
	return SetScoped("", skillName, name, value)
}

// Get retrieves a secret for (skill, name) from the unscoped service.
// Returns ErrNotFound if absent.
func Get(skillName, name string) (secrets.Secret, error) {
	return GetScoped("", skillName, name)
}

// Has returns true if an unscoped secret is present for (skill, name).
func Has(skillName, name string) (bool, error) {
	return HasScoped("", skillName, name)
}

// Delete removes an unscoped secret for (skill, name). Missing entries are
// not an error.
func Delete(skillName, name string) error {
	return DeleteScoped("", skillName, name)
}

// SetScoped stores a secret under (scope, skill). scope="" is the unscoped
// (legacy/global) form; a workdir-id isolates per-workdir; DefaultsScope
// mirrors the remembered default.
func SetScoped(scope, skillName, name string, value secrets.Secret) error {
	if mock := mockBackend(); mock != nil {
		return mock.set(scope, skillName, name, value.ExposeString())
	}
	svc := ScopedService(scope, skillName)
	if err := keyring.Set(svc, name, value.ExposeString()); err != nil {
		return fmt.Errorf("keychain set %s/%s: %w", svc, name, err)
	}
	return nil
}

// GetScoped retrieves a secret under (scope, skill). Returns ErrNotFound if
// absent, or if the OS keychain backend is unavailable (e.g. headless Linux
// with no Secret Service daemon). The latter is treated as "not found"
// because the secret cannot exist in a keychain that doesn't run; callers
// relying on env_passthrough or optional secrets still work.
//
// An unavailable backend additionally wraps ErrUnavailable, so a caller that
// has exhausted its fallbacks can report "no Secret Service provider" instead
// of the misleading "required secret missing — run omac secrets set". Check
// ErrNotFound first if you only care whether a value is usable; check
// ErrUnavailable only once no fallback satisfied the secret.
func GetScoped(scope, skillName, name string) (secrets.Secret, error) {
	if mock := mockBackend(); mock != nil {
		v, err := mock.get(scope, skillName, name)
		if err != nil {
			return secrets.Secret{}, err
		}
		return secrets.NewSecretString(v), nil
	}
	svc := ScopedService(scope, skillName)
	v, err := keyring.Get(svc, name)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return secrets.Secret{}, ErrNotFound
		}
		if IsUnavailable(err) {
			// Both sentinels: ErrNotFound preserves every pre-existing caller,
			// ErrUnavailable lets new ones diagnose the backend. The raw error
			// is wrapped rather than formatted so BackendCause can recover it
			// for a message that leads with the actual condition instead of
			// "secret not found: backend unavailable".
			return secrets.Secret{}, fmt.Errorf("%w: %w: %w", ErrNotFound, ErrUnavailable, err)
		}
		return secrets.Secret{}, fmt.Errorf("keychain get %s/%s: %w", svc, name, err)
	}
	return secrets.NewSecretString(v), nil
}

// IsUnavailable reports whether err indicates the OS keychain backend
// itself is missing (no Secret Service daemon on headless Linux, no
// keychain daemon on macOS, etc.), as opposed to a per-secret failure. Read
// paths (GetScoped/HasScoped) map such errors to ErrNotFound so optional
// secrets and env_passthrough fallbacks still work in CI; write-path
// callers (register, secrets set) use it instead to attach an actionable,
// OS-specific hint rather than surfacing the raw backend error verbatim.
func IsUnavailable(err error) bool {
	msg := err.Error()
	// Linux: org.freedesktop.secrets not provided by any .service files
	// (dbus.ServiceUnknown when no Secret Service implementation is running).
	if strings.Contains(msg, "org.freedesktop.secrets") {
		return true
	}
	// Linux: dbus connection failures (no session bus on headless runners).
	if strings.Contains(msg, "dbus") || strings.Contains(msg, "D-Bus") {
		return true
	}
	// Linux: the session-bus socket named by DBUS_SESSION_BUS_ADDRESS has
	// been torn down while the env var still points at it — the WSL2 /
	// `Linger=no` case where systemd removes /run/user/<uid> when the login
	// session ends. go-keyring surfaces the raw dial failure ("dial unix
	// <path>: connect: no such file or directory" / "connection refused"),
	// which carries no dbus marker but is an environment-level unavailability,
	// not a per-secret error.
	if strings.Contains(msg, "dial unix") &&
		(strings.Contains(msg, "no such file or directory") ||
			strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "permission denied")) {
		return true
	}
	return false
}

// BackendCause returns the raw backend error underneath a classified read
// error, stripping the ErrNotFound/ErrUnavailable markers GetScoped attaches.
// Returns err unchanged when there is nothing to strip.
//
// Callers use it for the human-facing text: the full chain reads "keychain:
// secret not found: keychain: backend unavailable: dial unix …", whose first
// clause contradicts its second, while the cause alone ("dial unix
// /run/user/1000/bus: connect: no such file or directory") names the actual
// problem — and, usefully, which socket.
func BackendCause(err error) error {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		if errs := multi.Unwrap(); len(errs) > 0 {
			return BackendCause(errs[len(errs)-1])
		}
	}
	return err
}

// isUnavailableErr reports backend unavailability however it is expressed:
// already classified as ErrUnavailable (read path, see GetScoped) or still a
// raw backend error that only string-sniffing recognizes (write path, where
// keyring.Set/Delete errors reach the caller verbatim).
func isUnavailableErr(err error) bool {
	return errors.Is(err, ErrUnavailable) || IsUnavailable(err)
}

// Ping probes whether the OS keychain backend itself is reachable, for
// diagnostics (`omac doctor`). It looks up a secret that will never exist:
// a not-found result means the backend answered (available), while a
// backend-level error is returned as-is so callers can classify it with
// IsUnavailable and attach a hint.
func Ping() error {
	_, err := keyring.Get("omac/__doctor_probe__", "probe")
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// GetWithFallback retrieves a secret under (scope, skill), falling back to
// the unscoped key (omac/<skill>) when the scoped key is absent. This lets
// readers (start, serve) find secrets whether they were stored scoped
// (per-workdir, written by serve-aware register) or unscoped (legacy /
// global). An empty scope is just the unscoped lookup. Returns ErrNotFound
// only when neither key exists — additionally wrapping ErrUnavailable when
// the reason neither key could be read is a missing backend.
func GetWithFallback(scope, skillName, name string) (secrets.Secret, error) {
	if scope != "" {
		v, err := GetScoped(scope, skillName, name)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return secrets.Secret{}, err
		}
	}
	return GetScoped("", skillName, name)
}

// HasScoped reports whether a secret is present under (scope, skill).
// Returns false (not an error) when the keychain backend is unavailable.
func HasScoped(scope, skillName, name string) (bool, error) {
	if mock := mockBackend(); mock != nil {
		return mock.has(scope, skillName, name)
	}
	svc := ScopedService(scope, skillName)
	_, err := keyring.Get(svc, name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, keyring.ErrNotFound) || IsUnavailable(err) {
		return false, nil
	}
	return false, fmt.Errorf("keychain probe %s/%s: %w", svc, name, err)
}

// DeleteScoped removes a secret under (scope, skill). Missing entries are
// not an error.
func DeleteScoped(scope, skillName, name string) error {
	if mock := mockBackend(); mock != nil {
		return mock.delete(scope, skillName, name)
	}
	svc := ScopedService(scope, skillName)
	err := keyring.Delete(svc, name)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keychain delete %s/%s: %w", svc, name, err)
}

// SetWithDefault stores a secret under (scope, skill) AND mirrors it into
// the DefaultsScope so a future registration elsewhere can reuse it as a
// suggested value (docs/MULTI_DIR_DESKTOP.md §4.4). A failure to mirror is
// not fatal to the primary write — defaults are best-effort convenience.
func SetWithDefault(scope, skillName, name string, value secrets.Secret) error {
	if err := SetScoped(scope, skillName, name, value); err != nil {
		return err
	}
	if scope != DefaultsScope {
		_ = SetScoped(DefaultsScope, skillName, name, value)
	}
	return nil
}

// GetDefault returns the remembered default secret for (skill, name), or
// ErrNotFound.
func GetDefault(skillName, name string) (secrets.Secret, error) {
	return GetScoped(DefaultsScope, skillName, name)
}

// SetScopedDefaultMirror writes value only into the remembered-defaults
// scope (omac/__defaults__/<skill>), without touching any per-workdir or
// unscoped key. Used to backfill the default from an already-stored secret
// so `register --defaults` can reuse it later.
func SetScopedDefaultMirror(skillName, name string, value secrets.Secret) error {
	return SetScoped(DefaultsScope, skillName, name, value)
}

// DeleteAll removes every declared unscoped secret for a skill. Secrets not
// listed are left in place (go-keyring has no list-by-service primitive).
func DeleteAll(skillName string, names []string) error {
	return DeleteAllScoped("", skillName, names)
}

// DeleteAllScoped removes every declared secret for (scope, skill).
func DeleteAllScoped(scope, skillName string, names []string) error {
	for _, n := range names {
		if err := DeleteScoped(scope, skillName, n); err != nil {
			return err
		}
	}
	return nil
}

// --- File-backed mock (OMAC_KEYCHAIN_MOCK) -------------------------------
//
// When OMAC_KEYCHAIN_MOCK points to a file path, all keychain operations
// read/write a JSON file there instead of calling the OS keychain. This
// lets e2e tests exercise the real keychain code path without a Secret
// Service daemon, and persists across the register/start subprocesses that
// share the same file. Test-only; never set in production.

type fileMock struct {
	path string
	mu   sync.Mutex
}

type mockEntry struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Value   string `json:"value"`
}

func mockBackend() *fileMock {
	p := os.Getenv("OMAC_KEYCHAIN_MOCK")
	if p == "" {
		return nil
	}
	return &fileMock{path: p}
}

func (m *fileMock) load() ([]mockEntry, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []mockEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (m *fileMock) save(entries []mockEntry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0o600)
}

func (m *fileMock) set(scope, skill, name, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.load()
	if err != nil {
		return err
	}
	svc := ScopedService(scope, skill)
	for i, e := range entries {
		if e.Service == svc && e.Name == name {
			entries[i].Value = value
			return m.save(entries)
		}
	}
	entries = append(entries, mockEntry{Service: svc, Name: name, Value: value})
	return m.save(entries)
}

func (m *fileMock) get(scope, skill, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.load()
	if err != nil {
		return "", err
	}
	svc := ScopedService(scope, skill)
	for _, e := range entries {
		if e.Service == svc && e.Name == name {
			return e.Value, nil
		}
	}
	return "", ErrNotFound
}

func (m *fileMock) has(scope, skill, name string) (bool, error) {
	_, err := m.get(scope, skill, name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (m *fileMock) delete(scope, skill, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.load()
	if err != nil {
		return err
	}
	svc := ScopedService(scope, skill)
	for i, e := range entries {
		if e.Service == svc && e.Name == name {
			entries = append(entries[:i], entries[i+1:]...)
			return m.save(entries)
		}
	}
	return nil
}
