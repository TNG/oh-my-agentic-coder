package mcpreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/keychain"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OMAC_KEYCHAIN_MOCK", filepath.Join(t.TempDir(), "keychain.json"))
}

func writeDef(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func defYAML(name string) string {
	return "name: " + name + "\nserver:\n  command: [npx, -y, some-server]\nsecrets: [API_TOKEN]\n"
}

func TestScopeForSymlinkedWorkdir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	a, b := ScopeFor(real), ScopeFor(link)
	if a.Key != b.Key || a.DisplayPath != b.DisplayPath {
		t.Fatalf("scopes differ: %+v vs %+v", a, b)
	}
	if a.Key == GlobalScopeKey {
		t.Fatal("project scope must not use the global key")
	}
}

func TestParseDefinitionRefusals(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "name: a\nserver:\n  command: [x]\nbogus: 1\n",
		"unknown nested":   "name: a\nserver:\n  command: [x]\n  cwd: /tmp\n",
		"bad name":         "name: A/b\nserver:\n  command: [x]\n",
		"empty command":    "name: a\nserver:\n  command: []\n",
		"relative command": "name: a\nserver:\n  command: [./run.sh]\n",
		"reserved secret":  "name: a\nserver:\n  command: [x]\nsecrets: [OMAC_SIDECAR_SECRET]\n",
		"reserved env":     "name: a\nserver:\n  command: [x]\n  env: {OMAC_SIDECAR_SECRET: v}\n",
		"empty":            "",
	}
	for name, body := range cases {
		if _, err := ParseDefinition([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := ParseDefinition([]byte(defYAML("ok-1"))); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterRefusesCommandInsideWorkdir(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	def := writeDef(t, "name: a\nserver:\n  command: ["+filepath.Join(wd, "bin", "srv")+"]\n")
	if _, err := Register(ScopeFor(wd), def, []string{"opencode"}); err == nil || !strings.Contains(err.Error(), "inside the workdir") {
		t.Fatalf("got %v", err)
	}
	outside := writeDef(t, "name: a\nserver:\n  command: [/usr/bin/env]\n")
	if _, err := Register(ScopeFor(wd), outside, []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterRequiresHarness(t *testing.T) {
	isolate(t)
	if _, err := Register(GlobalScope(), writeDef(t, defYAML("a")), nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestEffectiveProjectWinsAndHarnessFilter(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	g, p := GlobalScope(), ScopeFor(wd)
	for _, c := range []struct {
		s    Scope
		name string
		h    []string
	}{
		{g, "shared", []string{"opencode"}},
		{p, "shared", []string{"opencode"}},
		{g, "only-global", []string{"opencode", "claude-code"}},
		{p, "claude-only", []string{"claude-code"}},
		{g, "kept-global", []string{"opencode"}},
		{p, "kept-global", []string{"claude-code"}},
	} {
		if _, err := Register(c.s, writeDef(t, defYAML(c.name)), c.h); err != nil {
			t.Fatal(err)
		}
	}
	entries, shadowed, err := Effective(wd, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name+"@"+e.ScopeKey)
	}
	want := []string{"kept-global@global", "only-global@global", "shared@" + p.Key}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if len(shadowed) != 1 || shadowed[0] != "shared" {
		t.Fatalf("shadowed = %v", shadowed)
	}
	if entries, _, _ := Effective(wd, "other"); len(entries) != 0 {
		t.Fatalf("unknown harness matched %d entries", len(entries))
	}
}

func TestEmptyHarnessesNeverMatch(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	e, err := Register(GlobalScope(), writeDef(t, defYAML("a")), []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.Record
	rec.Harnesses = nil
	dir, _ := serverDir(GlobalScope(), "a")
	if err := writeRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"opencode", ""} {
		if entries, _, _ := Effective(wd, h); len(entries) != 0 {
			t.Fatalf("harness %q matched an entry with no harnesses", h)
		}
	}
}

func TestLoadDetectsSnapshotTamper(t *testing.T) {
	isolate(t)
	s := GlobalScope()
	e, err := Register(s, writeDef(t, defYAML("a")), []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if !IsSnapshotPath(e.SnapshotDir) {
		t.Fatalf("%s should be a snapshot path", e.SnapshotDir)
	}
	if _, err := Load(s, "a"); err != nil {
		t.Fatal(err)
	}
	tampered := "name: a\nserver:\n  command: [/bin/evil]\n"
	if err := os.WriteFile(e.DefPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s, "a"); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := Effective(t.TempDir(), "opencode"); err == nil {
		t.Fatal("Effective must fail on a tampered snapshot")
	}
}

func TestLoadRejectsTraversalName(t *testing.T) {
	isolate(t)
	if _, err := Load(GlobalScope(), "../x"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDeregisterRemovesSecretsAndDir(t *testing.T) {
	isolate(t)
	s := ScopeFor(t.TempDir())
	if _, err := Register(s, writeDef(t, defYAML("a")), []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	if err := SetSecret(s, "a", "API_TOKEN", secrets.NewSecretString("hunter2")); err != nil {
		t.Fatal(err)
	}
	v, err := GetSecret(s, "a", "API_TOKEN")
	if err != nil || v.ExposeString() != "hunter2" {
		t.Fatalf("get: %v", err)
	}
	if ok, _ := keychain.HasScoped(keychain.DefaultsScope, "a", "API_TOKEN"); ok {
		t.Fatal("secret was mirrored into defaults")
	}
	dir, _ := serverDir(s, "a")
	if err := Deregister(s, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetSecret(s, "a", "API_TOKEN"); err == nil {
		t.Fatal("secret survived deregister")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir survived: %v", err)
	}
	if err := Deregister(s, "a"); err == nil {
		t.Fatal("second deregister should fail")
	}
}

func TestSecretsNeverInRecord(t *testing.T) {
	isolate(t)
	s := GlobalScope()
	if _, err := Register(s, writeDef(t, defYAML("a")), []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	if err := SetSecret(s, "a", "API_TOKEN", secrets.NewSecretString("s3cr3t-value")); err != nil {
		t.Fatal(err)
	}
	dir, _ := serverDir(s, "a")
	raw, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cr3t-value") {
		t.Fatal("secret value in record.json")
	}
	if !strings.Contains(string(raw), "API_TOKEN") {
		t.Fatal("secret name missing from SecretsStored")
	}
}

func TestReRegisterKeepsSecretsStored(t *testing.T) {
	isolate(t)
	s := GlobalScope()
	def := writeDef(t, defYAML("a"))
	if _, err := Register(s, def, []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendSecretsStored(s, "a", []string{"API_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	e, err := Register(s, def, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.SecretsStored) != 1 {
		t.Fatalf("SecretsStored = %v", e.SecretsStored)
	}
}

func TestAllScopes(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	if _, err := Register(ScopeFor(wd), writeDef(t, defYAML("a")), []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(GlobalScope(), writeDef(t, defYAML("b")), []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	scopes, err := AllScopes()
	if err != nil || len(scopes) != 2 {
		t.Fatalf("%v %v", scopes, err)
	}
	if scopes[0].Key != GlobalScopeKey || scopes[1].DisplayPath != ScopeFor(wd).DisplayPath {
		t.Fatalf("scopes = %+v", scopes)
	}
}

func TestLoadSnapshotFileVerifiesTheHash(t *testing.T) {
	isolate(t)
	e, err := Register(GlobalScope(), writeDef(t, defYAML("a")), []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	def, err := LoadSnapshotFile(e.DefPath)
	if err != nil || def.Name != "a" {
		t.Fatalf("intact snapshot: %v %v", def, err)
	}
	if err := os.WriteFile(e.DefPath, []byte("name: a\nserver:\n  command: [/bin/evil]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshotFile(e.DefPath); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("tampered snapshot: %v", err)
	}
	other := writeDef(t, defYAML("a"))
	if _, err := LoadSnapshotFile(other); err == nil {
		t.Fatal("a definition outside a hash-named snapshot directory must be refused")
	}
}

func TestSnapshotWriteLeavesNoTemporaryFiles(t *testing.T) {
	isolate(t)
	e, err := Register(GlobalScope(), writeDef(t, defYAML("a")), []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Register(GlobalScope(), writeDef(t, defYAML("a")), []string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(e.SnapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != snapshotFile {
		t.Fatalf("snapshot dir holds %v", files)
	}
	if info, _ := os.Stat(e.DefPath); info.Mode().Perm() != 0o600 {
		t.Errorf("snapshot mode %v", info.Mode().Perm())
	}
}
