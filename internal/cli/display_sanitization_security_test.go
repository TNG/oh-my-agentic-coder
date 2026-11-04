package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/manifest"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
)

// Each forbidden rune is supplied, not merely searched for in the output.
func displayForbiddenRunes() []rune {
	runes := []rune{'\u061c', '\u200b', '\u200c', '\u200d', '\u200e', '\u200f', '\u2028', '\u2029', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2060', '\u2066', '\u2067', '\u2068', '\u2069', '\ufeff'}
	for r := rune(0); r <= 0x9f; r++ {
		if r < 0x20 || r >= 0x7f {
			runes = append(runes, r)
		}
	}
	return runes
}

func TestSkillRefusalForbiddenRunesAndInputIdentity(t *testing.T) {
	for _, kind := range []skillstate.ProblemKind{skillstate.MetaBroken, skillstate.BundleDrift, skillstate.MissingSecret, skillstate.InvalidSecret, skillstate.InvalidConfig, skillstate.KeychainUnavailable, skillstate.MissingField} {
		for _, r := range displayForbiddenRunes() {
			t.Run(fmt.Sprintf("%s/U+%04X", kind, r), func(t *testing.T) {
				payload := "café 東京 عربي left" + string(r) + "right"
				cause := errors.New(payload)
				input := []skillstate.Problem{{Kind: kind, Skill: payload, Field: payload, Detail: payload, Fix: payload, Cause: cause, Optional: true}}
				before := append([]skillstate.Problem(nil), input...)
				clean := sanitizeProblems(input)
				want := "café 東京 عربي leftright"
				if clean[0].Skill != want || clean[0].Field != want || clean[0].Detail != want || clean[0].Fix != want {
					t.Errorf("not every problem field is safe: %+v", clean[0])
				}
				if clean[0].Kind != kind || clean[0].Cause != cause || !clean[0].Optional {
					t.Fatal("problem semantics changed")
				}
				var out bytes.Buffer
				if renderSkillRefusal(&out, "omac start", input) == ExitOK || !strings.Contains(out.String(), want) || strings.Contains(out.String(), payload) {
					t.Errorf("unsafe or missing refusal: %q", out.String())
				}
				if !reflect.DeepEqual(input, before) {
					t.Fatal("caller-owned problems changed")
				}
			})
		}
	}
}

func invalidDisplayEnumMeta(r rune) string {
	// YAML decodes the escape before validating the enum default.
	return fmt.Sprintf("name: benign\nsidecar:\n  command: [true]\n  config:\n    - name: MODE\n      type: enum\n      choices: [\"café 東京 عربي left\\u%04xright\"]\n      default: mismatch\n", r)
}

func TestSkillMetadataToRefusalUnicode(t *testing.T) {
	for _, r := range displayForbiddenRunes() {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, config.MetaFileName), []byte(invalidDisplayEnumMeta(r)), 0600); err != nil {
				t.Fatal(err)
			}
			_, problems := skillstate.New(skillstate.Options{}).Inspect(registry.Entry{Name: "benign"}, dir)
			if len(problems) != 1 || !strings.ContainsRune(problems[0].Detail, r) {
				t.Fatalf("metadata did not reach the boundary: %+v", problems)
			}
			var out bytes.Buffer
			renderSkillRefusal(&out, "omac start", problems)
			if strings.Contains(out.String(), "left"+string(r)+"right") || !strings.Contains(out.String(), "café 東京 عربي leftright") || !strings.Contains(out.String(), "omac register benign --force") {
				t.Errorf("metadata-to-refusal output: %q", out.String())
			}
		})
	}
}

func TestSkillStartRefusalUnicode(t *testing.T) {
	isolateHome(t)
	wd := stageWorkdir(t, "benign")
	dir := filepath.Join(wd, ".opencode", "skills", "benign")
	if err := os.WriteFile(filepath.Join(dir, config.MetaFileName), []byte(invalidDisplayEnumMeta('\u202e')), 0600); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(wd, &registry.Registry{Version: registry.SchemaVersion, Registered: []registry.Entry{{Name: "benign", SkillDir: ".opencode/skills/benign"}}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	env := makeEnv(wd)
	env.Stderr = f
	code := runStart([]string{"--inner", "/usr/bin/true"}, env)
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if code != ExitConfigInvalid || !bytes.Contains(raw, []byte("omac register benign --force")) || bytes.Contains(raw, []byte("\u202e")) || !bytes.Contains(raw, []byte("café 東京 عربي leftright")) {
		t.Fatalf("start refusal: code=%d stderr=%q", code, raw)
	}
}

func TestSkillMetadataToActivationBridges(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	for i, r := range displayForbiddenRunes() {
		dir := filepath.Join(wd, ".opencode", "skills", fmt.Sprintf("benign-%d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, config.MetaFileName), []byte(invalidDisplayEnumMeta(r)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(map[string]string{"dir": wd})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/__omac__/activate", bytes.NewReader(body))
	addServeTestToken(req)
	w := httptest.NewRecorder()
	s.controlMux().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Skills []struct{ Detail string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Skills) != len(displayForbiddenRunes()) {
		t.Fatal("activation lost broken skills")
	}
	for _, r := range displayForbiddenRunes() {
		found := false
		for _, sk := range response.Skills {
			found = found || strings.Contains(sk.Detail, "left"+string(r)+"right")
		}
		if !found {
			t.Fatalf("activation fixture lacks raw U+%04X", r)
		}
	}
	check := func(t *testing.T, text string) {
		t.Helper()
		if strings.Count(text, "— BROKEN:") != len(response.Skills) || !strings.Contains(text, "café 東京 عربي leftright") {
			t.Fatalf("missing manifest rows/content: %q", text)
		}
		for _, r := range displayForbiddenRunes() {
			if strings.Contains(text, "left"+string(r)+"right") {
				t.Errorf("U+%04X survived live sink", r)
			}
		}
	}
	t.Run("go", func(t *testing.T) { check(t, manifest.Render(w.Body.String(), ".opencode/skills")) })
	fixture := filepath.Join(t.TempDir(), "activation.json")
	if err := os.WriteFile(fixture, w.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("opencode-and-pi-renderers", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("Node with TypeScript stripping is required")
		}
		cmd := exec.Command("node", "../bridge/testdata/display-sinks.mjs", fixture)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bridge runtime: %v\n%s", err, out)
		}
	})
	// The real authenticated handler is used again by each shell hook, not a curl stub.
	srv := httptest.NewServer(s.controlMux())
	defer srv.Close()
	for _, harness := range []string{"claude", "codex", "copilot"} {
		t.Run(harness, func(t *testing.T) {
			cmd := exec.Command("bash", "../../."+harness+"/hooks/omac-bridge.sh")
			t.Setenv("OMAC_CONTROL_BASE", srv.URL)
			t.Setenv("OMAC_CONTROL_TOKEN", s.controlToken)
			cmd.Env = os.Environ()
			payload, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "cwd": wd})
			cmd.Stdin = bytes.NewReader(payload)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				HookSpecificOutput struct{ AdditionalContext string }
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("hook output %q: %v", out, err)
			}
			check(t, result.HookSpecificOutput.AdditionalContext)
		})
	}
}
