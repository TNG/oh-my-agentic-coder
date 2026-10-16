package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
)

// fakeSecretInputHidden makes secretPromptNote see a terminal (hidden=true)
// or a pipe (hidden=false) for one test.
func fakeSecretInputHidden(t *testing.T, hidden bool) {
	t.Helper()
	orig := secretInputHidden
	secretInputHidden = func() bool { return hidden }
	t.Cleanup(func() { secretInputHidden = orig })
}

// helpSkillDir returns a skill directory that contains SKILL.md, the file
// the docs references in these tests point to.
func helpSkillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const tokenHint = "How to get it:\n  1. Open https://app.example.com and log in.\n  2. Copy the token (starts with xoxc-).\n"

func TestRegisterSecretShowsHintDocsAndPromptNote(t *testing.T) {
	fakeSecretInputHidden(t, true)
	env, _, errBuf, drain := newPipeEnv(t, "")
	prompts := fakeSecretInput(t, "xoxc-123")
	dir := helpSkillDir(t)
	spec := config.SecretSpec{
		Name:        "SLACK_TOKEN",
		Description: "Slack session token",
		Pattern:     "^xoxc-.+",
		Hint:        tokenHint,
		Docs:        "SKILL.md#obtaining-tokens",
	}

	if _, err := handleOneSecret(env, "", "help-secret", dir, spec, true, nil, nil, false, "linux"); err != nil {
		t.Fatalf("handleOneSecret: %v", err)
	}
	drain()

	want := "  SLACK_TOKEN Slack session token\n" +
		"\n" +
		"    How to get it:\n" +
		"      1. Open https://app.example.com and log in.\n" +
		"      2. Copy the token (starts with xoxc-).\n" +
		"\n" +
		"    More: " + filepath.Join(dir, "SKILL.md") + ` (section "obtaining-tokens")` + "\n" +
		"\n" +
		"  stored SLACK_TOKEN\n"
	if got := errBuf.String(); got != want {
		t.Errorf("stderr =\n%s\nwant\n%s", got, want)
	}
	if got, want := prompts.String(), "  ? SLACK_TOKEN (input hidden; must match ^xoxc-.+): \n"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

func TestRegisterSecretRepeatsHintAfterPatternMismatch(t *testing.T) {
	fakeSecretInputHidden(t, true)
	env, _, errBuf, drain := newPipeEnv(t, "")
	fakeSecretInput(t, "wrong-value", "xoxc-123")
	dir := helpSkillDir(t)
	spec := config.SecretSpec{Name: "SLACK_TOKEN", Pattern: "^xoxc-.+", Hint: tokenHint, Docs: "SKILL.md"}

	if _, err := handleOneSecret(env, "", "help-retry", dir, spec, true, nil, nil, false, "linux"); err != nil {
		t.Fatalf("handleOneSecret: %v", err)
	}
	drain()
	out := errBuf.String()

	retry := strings.Index(out, "[retry]")
	if retry < 0 {
		t.Fatalf("control: no [retry] line, so the mismatch path was not reached:\n%s", out)
	}
	if strings.Count(out, "2. Copy the token") != 2 {
		t.Errorf("hint should be shown twice (before the prompt and after the mismatch):\n%s", out)
	}
	if !strings.Contains(out[retry:], "1. Open https://app.example.com") {
		t.Errorf("hint is not shown after the [retry] line:\n%s", out)
	}
	if !strings.Contains(out[retry:], "More: "+filepath.Join(dir, "SKILL.md")) {
		t.Errorf("docs path is not shown after the [retry] line:\n%s", out)
	}
}

// A skill without hint and docs looks exactly as before. The only change is
// the "input hidden" note on a terminal.
func TestRegisterSecretWithoutHelpIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		hidden     bool
		wantPrompt string
	}{
		{true, "  ? TOKEN (input hidden): \n"},
		{false, "  ? TOKEN: \n"},
	} {
		fakeSecretInputHidden(t, tc.hidden)
		env, _, errBuf, drain := newPipeEnv(t, "")
		prompts := fakeSecretInput(t, "value")
		spec := config.SecretSpec{Name: "TOKEN", Description: "The token."}

		if _, err := handleOneSecret(env, "", "help-plain", helpSkillDir(t), spec, true, nil, nil, false, "linux"); err != nil {
			t.Fatalf("handleOneSecret: %v", err)
		}
		drain()
		if got, want := errBuf.String(), "  TOKEN The token.\n  stored TOKEN\n"; got != want {
			t.Errorf("hidden=%v: stderr = %q, want %q", tc.hidden, got, want)
		}
		if got := prompts.String(); got != tc.wantPrompt {
			t.Errorf("hidden=%v: prompt = %q, want %q", tc.hidden, got, tc.wantPrompt)
		}
	}
}

func TestRegisterSecretPromptNoteKeepsEnvDefault(t *testing.T) {
	spec := config.SecretSpec{Name: "T", Pattern: "^a"}
	got := secretPromptNote(spec, false, "press Enter to accept value from $T_ENV")
	if want := " (must match ^a; press Enter to accept value from $T_ENV)"; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

// A docs file that does not exist is left out: pointing the user at a
// missing file does not help. `omac register` warns about it up front.
func TestRegisterSecretOmitsMissingDocsFile(t *testing.T) {
	fakeSecretInputHidden(t, true)
	env, _, errBuf, drain := newPipeEnv(t, "")
	fakeSecretInput(t, "v")
	spec := config.SecretSpec{Name: "TOKEN", Hint: "Ask your admin.", Docs: "missing.md#x"}

	if _, err := handleOneSecret(env, "", "help-missing", helpSkillDir(t), spec, true, nil, nil, false, "linux"); err != nil {
		t.Fatalf("handleOneSecret: %v", err)
	}
	drain()
	out := errBuf.String()
	if !strings.Contains(out, "    Ask your admin.\n") {
		t.Fatalf("control: hint not rendered:\n%s", out)
	}
	if strings.Contains(out, "More:") {
		t.Errorf("a missing docs file should not be printed:\n%s", out)
	}
}

func TestRegisterFieldShowsHintDocsAndRepeatsAfterInvalidValue(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "EU\neu-west\n")
	dir := helpSkillDir(t)
	store := &skillconfig.Store{}
	spec := config.ConfigSpec{
		Name:        "REGION",
		Description: "Region to use.",
		Pattern:     "^[a-z-]+$",
		Hint:        "Use the lowercase region id,\nfor example eu-west.",
		Docs:        "SKILL.md#regions",
	}

	if _, err := handleOneField(env, store, "help-field", dir, spec, true, nil, nil, false, nil); err != nil {
		t.Fatalf("handleOneField: %v", err)
	}
	drain()
	out := errBuf.String()

	help := "\n" +
		"    Use the lowercase region id,\n" +
		"    for example eu-west.\n" +
		"\n" +
		"    More: " + filepath.Join(dir, "SKILL.md") + ` (section "regions")` + "\n" +
		"\n"
	if !strings.HasPrefix(out, "  REGION Region to use.\n"+help+"  ? REGION: ") {
		t.Errorf("help block missing or misplaced before the first prompt:\n%s", out)
	}
	retry := strings.Index(out, "[retry]")
	if retry < 0 {
		t.Fatalf("control: no [retry] line, so the invalid-value path was not reached:\n%s", out)
	}
	if !strings.Contains(out[retry:], help) {
		t.Errorf("help block not repeated after [retry]:\n%s", out)
	}
	if v, _ := store.Get("help-field", "REGION"); v != "eu-west" {
		t.Errorf("stored value = %q, want eu-west", v)
	}
}

// A config field without hint and docs renders as before.
func TestRegisterFieldWithoutHelpIsUnchanged(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "b\n")
	store := &skillconfig.Store{}
	spec := config.ConfigSpec{Name: "MODE", Description: "Mode.", Type: config.ConfigFieldEnum, Choices: []string{"a", "b"}}

	if _, err := handleOneField(env, store, "help-field-plain", helpSkillDir(t), spec, true, nil, nil, false, nil); err != nil {
		t.Fatalf("handleOneField: %v", err)
	}
	drain()
	want := "  MODE Mode.\n    choices: a, b\n  ? MODE: "
	if got := errBuf.String(); !strings.HasPrefix(got, want) {
		t.Errorf("stderr = %q, want prefix %q", got, want)
	}
}

// TestRunRegisterShowsHelpAndWarnsAboutMissingDocs runs the whole
// `omac register` command against a staged skill: the hint and the docs
// path appear at the prompt, and a docs entry that names a missing file
// or an unusable path is reported as a warning without failing the
// registration.
func TestRunRegisterShowsHelpAndWarnsAboutMissingDocs(t *testing.T) {
	isolateHome(t)
	fakeSecretInputHidden(t, true)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "helpdemo", `  secrets:
    - name: DEMO_TOKEN
      description: Demo token.
      pattern: "^demo-"
      hint: |
        Ask the demo service for a token.
      docs: SKILL.md#getting-a-token
  config:
    - name: DEMO_REGION
      pattern: "^[a-z]+$"
      hint: Any lowercase word.
      docs: missing.md
    - name: DEMO_MODE
      required: false
      hint: Any mode.
      docs: https://example.com/help
`)
	skillDir := filepath.Join(wd, ".opencode", "skills", "helpdemo")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# helpdemo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only one line of input: each config prompt reads stdin through its
	// own buffered reader, so the first one would swallow later lines.
	// DEMO_MODE is optional and gets skipped.
	env, _, errBuf, drain := newPipeEnv(t, "eu\n")
	env.Workdir = wd
	prompts := fakeSecretInput(t, "demo-123")

	code := runRegister([]string{"helpdemo", "--harness", "opencode"}, env)
	drain()
	out := errBuf.String()
	if code != ExitOK {
		t.Fatalf("register: code = %d, want ExitOK\n%s", code, out)
	}
	for _, want := range []string{
		"[warn] omac.yaml: sidecar.config[0].docs: missing.md not found in the skill directory\n",
		"    Ask the demo service for a token.\n",
		"    More: " + filepath.Join(skillDir, "SKILL.md") + ` (section "getting-a-token")` + "\n",
		"    Any lowercase word.\n",
		"[warn] omac.yaml: sidecar.config[1].docs: \"https://example.com/help\" must be a file path inside the skill, not a URL\n",
		"    Any mode.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr is missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "More: ") != 1 {
		t.Errorf("only the valid docs entry should get a More: line:\n%s", out)
	}
	if strings.Contains(out, "More: "+filepath.Join(skillDir, "missing.md")) {
		t.Errorf("the missing docs file should not get a More: line:\n%s", out)
	}
	if got, want := prompts.String(), "  ? DEMO_TOKEN (input hidden; must match ^demo-): \n"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

// TestRunRegisterWarnsAboutUnknownKeys: a key omac does not know (here the
// typo `hnit`) is named in a warning, and registration still succeeds.
func TestRunRegisterWarnsAboutUnknownKeys(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "typo", `  secrets:
    - name: TYPO_TOKEN
      required: false
      hnit: Get it from the admin.
`)
	env, _, errBuf, drain := newPipeEnv(t, "")
	env.Workdir = wd
	fakeSecretInput(t, "")

	code := runRegister([]string{"typo", "--harness", "opencode"}, env)
	drain()
	out := errBuf.String()
	if code != ExitOK {
		t.Fatalf("register: code = %d, want ExitOK\n%s", code, out)
	}
	if want := "[warn] omac.yaml: sidecar.secrets[0].hnit: unknown field (ignored)\n"; !strings.Contains(out, want) {
		t.Errorf("stderr is missing %q:\n%s", want, out)
	}
	if strings.Contains(out, "Get it from the admin.") {
		t.Errorf("an unknown key's value must not be shown as help:\n%s", out)
	}
}

// TestRunRegisterWarnsAboutUnknownKeysOnInvalidMeta: when omac.yaml fails
// validation, the unknown-key warning is printed before the error, so the
// author sees the typo that caused it.
func TestRunRegisterWarnsAboutUnknownKeysOnInvalidMeta(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "badname", `  secrets:
    - nmae: TOKEN
`)
	env, _, errBuf, drain := newPipeEnv(t, "")
	env.Workdir = wd

	code := runRegister([]string{"badname", "--harness", "opencode"}, env)
	drain()
	out := errBuf.String()
	if code != ExitConfigInvalid {
		t.Fatalf("control: register code = %d, want ExitConfigInvalid\n%s", code, out)
	}
	warn := strings.Index(out, "[warn] omac.yaml: sidecar.secrets[0].nmae: unknown field (ignored)\n")
	errLine := strings.Index(out, "omac register: sidecar.secrets[0].name")
	if warn < 0 || errLine < 0 || warn > errLine {
		t.Errorf("want the unknown-key warning before the validation error:\n%s", out)
	}
}

// A multi-line description (e.g. `description: |` in YAML) is printed on
// one line with spaces where the line breaks and tabs were, instead of
// running the words together. The hint keeps its lines.
func TestRegisterMultiLineDescriptionStaysReadable(t *testing.T) {
	fakeSecretInputHidden(t, false)
	env, _, errBuf, drain := newPipeEnv(t, "")
	fakeSecretInput(t, "v")
	spec := config.SecretSpec{
		Name:        "TOKEN",
		Description: "line one\nline two\tand\r\nthree\n",
		Hint:        "step one\nstep two\n",
	}
	if _, err := handleOneSecret(env, "", "help-multiline", "", spec, true, nil, nil, false, "linux"); err != nil {
		t.Fatalf("handleOneSecret: %v", err)
	}
	drain()
	out := errBuf.String()
	if want := "  TOKEN line one line two and three\n"; !strings.HasPrefix(out, want) {
		t.Errorf("stderr = %q, want prefix %q", out, want)
	}
	if !strings.Contains(out, "    step one\n    step two\n") {
		t.Errorf("hint lost its line breaks: %q", out)
	}
}

// TestSecretsSetShowsHintAndDocs: `omac secrets set` prompts through the
// same code as register. It finds the skill directory from the registry,
// where a workdir-local skill is stored relative to the workdir, and must
// join it with the workdir to print an absolute docs path.
func TestSecretsSetShowsHintAndDocs(t *testing.T) {
	isolateHome(t)
	fakeSecretInputHidden(t, true)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "setdemo", `  secrets:
    - name: SET_TOKEN
      description: Set token.
      hint: Ask the set service.
      docs: SKILL.md#token
`)
	skillDir := filepath.Join(wd, ".opencode", "skills", "setdemo")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# setdemo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	regEnv := makeEnv(wd)
	if code := runRegister([]string{"setdemo", "--harness", "opencode", "--no-secrets"}, regEnv); code != ExitOK {
		t.Fatalf("register: code = %d", code)
	}
	// Control: the registry stores the skill dir relative to the workdir,
	// so the join below is actually exercised.
	reg, err := registry.Load(wd)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := reg.Find("setdemo"); e == nil || filepath.IsAbs(e.SkillDir) {
		t.Fatalf("control: want a relative SkillDir in the registry, got %+v", e)
	}

	env, _, errBuf, drain := newPipeEnv(t, "")
	env.Workdir = wd
	prompts := fakeSecretInput(t, "value")
	code := runSecrets([]string{"set", "setdemo", "SET_TOKEN"}, env)
	drain()
	out := errBuf.String()
	if code != ExitOK {
		t.Fatalf("secrets set: code = %d\n%s", code, out)
	}
	want := "  SET_TOKEN Set token.\n" +
		"\n" +
		"    Ask the set service.\n" +
		"\n" +
		"    More: " + filepath.Join(skillDir, "SKILL.md") + ` (section "token")` + "\n" +
		"\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("stderr =\n%s\nwant prefix\n%s", out, want)
	}
	if got := prompts.String(); got != "  ? SET_TOKEN (input hidden): \n" {
		t.Errorf("prompt = %q", got)
	}
}

// Blank lines around a YAML block hint are dropped, and blank lines inside
// it carry no trailing indent, so a copied snippet has no stray whitespace.
func TestPrintValueHelpTrimsBlankHintLines(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	printValueHelp(w, styler{}, "NAME", "d", "\n\nfirst\n\n  \t\nlast\n", "", "")
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := "  NAME d\n\n    first\n\n\n    last\n\n"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}
