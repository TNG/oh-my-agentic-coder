package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
)

func newStartReloaderForTest(t *testing.T) *startReloader {
	t.Helper()
	isolateHome(t)
	// A real facade: reload installs stub routes for skills it cannot bring up,
	// so the facade is no longer optional (see reloadStubRoute). Start may fail
	// if loopback listen is forbidden; that's tolerable — AddRoute doesn't need
	// a live listener.
	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	_ = f.Start(t.Context())
	t.Cleanup(func() { f.Close() })
	return &startReloader{
		env:     makeEnv(t.TempDir()),
		facade:  f,
		mounted: map[string]string{},
	}
}

func TestStartReloaderMountedTracking(t *testing.T) {
	r := newStartReloaderForTest(t)
	if r.isMounted("slack") {
		t.Fatal("nothing should be mounted yet")
	}
	r.markMounted("slack", "slack")
	r.markMounted("email", "email")
	if !r.isMounted("slack") || !r.isMounted("email") {
		t.Error("markMounted did not record names")
	}
	if r.isMounted("jira") {
		t.Error("unexpected mount")
	}
}

// TestStartReloaderReloadReportsMissingSecret: a skill missing a required
// secret must not be spawned — and must not be dropped in silence either.
// Before #174 reload just `continue`d: no message, no route, no diagnostic, so
// an agent that had installed and registered a skill had no way to learn why it
// never appeared. It now gets a pending-credentials route, a manifest entry,
// and one line on stderr.
func TestStartReloaderReloadReportsMissingSecret(t *testing.T) {
	r := newStartReloaderForTest(t)
	wd := r.env.Workdir

	stageSkillWithSecret(t, wd, "slack")
	// Register it workdir-local so reload's registry scan finds it.
	if code := runRegister([]string{"slack", "--no-secrets"}, r.env); code != ExitOK {
		t.Fatalf("register exit=%d", code)
	}

	added := r.reload()
	if len(added) != 0 {
		t.Errorf("expected no skills mounted (missing secret), got %v", added)
	}
	// NOT mounted: a later reload, after `omac secrets set`, must still be able
	// to promote it.
	if r.isMounted("slack") {
		t.Error("slack should not be mounted with a missing required secret")
	}

	// A stub route exists, so a probe gets an actionable 409 rather than a 404.
	if !r.facade.HasRoute("", "slack") {
		t.Error("want a stub route for the unready skill, got none")
	}

	// The manifest reports it, in serve's shape, so the agent's briefing can
	// explain it (internal/manifest renders pending-credentials + missing).
	m := r.manifest()
	skills, _ := m["skills"].([]map[string]any)
	var found map[string]any
	for _, sk := range skills {
		if sk["name"] == "slack" {
			found = sk
		}
	}
	if found == nil {
		t.Fatalf("manifest omitted the unready skill: %v", skills)
	}
	if got := found["state"]; got != "pending-credentials" {
		t.Errorf("state = %v, want pending-credentials", got)
	}
	if got, _ := found["missing"].([]string); len(got) != 1 || got[0] != "API_TOKEN" {
		t.Errorf("missing = %v, want [API_TOKEN]", found["missing"])
	}
	if _, ok := found["base"]; ok {
		t.Error("an unready skill must not advertise a base URL")
	}

	// One warning per reason, not one per reload.
	if !r.warnOnce("x", "reason") || r.warnOnce("x", "reason") {
		t.Error("warnOnce should report only the first occurrence of a reason")
	}
	if !r.warnOnce("x", "different reason") {
		t.Error("warnOnce should report a CHANGED reason for the same skill")
	}
}

// TestStartReloaderPromotionClearsNotReady: once the missing value is supplied
// and a later reload mounts the skill, the stale pending-credentials entry must
// disappear from the manifest rather than shadowing the live route.
func TestStartReloaderPromotionClearsNotReady(t *testing.T) {
	r := newStartReloaderForTest(t)
	r.markNotReady("slack", &notReadySkill{
		Mount: "slack", State: facade.RoutePendingCredentials, Missing: []string{"API_TOKEN"},
	})
	if got := r.manifest()["skills"].([]map[string]any); len(got) != 1 || got[0]["state"] != "pending-credentials" {
		t.Fatalf("manifest = %v, want one pending-credentials entry", got)
	}

	r.markMounted("slack", "slack")
	skills := r.manifest()["skills"].([]map[string]any)
	if len(skills) != 1 {
		t.Fatalf("manifest = %v, want exactly one entry (no stale duplicate)", skills)
	}
	if skills[0]["state"] != "ready" {
		t.Errorf("state = %v, want ready after promotion", skills[0]["state"])
	}
}

// TestReloadStubRouteMapping locks reload's presentation of the shared readiness
// problems. The split is RECOVERABILITY: anything a value can still clear stays
// pending-credentials, because a pending route is promotable on the next reload
// while a broken one is not. Only a cause needing a re-register is broken.
func TestReloadStubRouteMapping(t *testing.T) {
	cases := []struct {
		name      string
		problems  []skillstate.Problem
		wantState facade.RouteState
		wantNil   bool
		wantMiss  []string
	}{
		{name: "ready", problems: nil, wantNil: true},
		{
			name:      "missing secret is supplyable",
			problems:  []skillstate.Problem{{Kind: skillstate.MissingSecret, Field: "TOKEN"}},
			wantState: facade.RoutePendingCredentials,
			wantMiss:  []string{"TOKEN"},
		},
		{
			name:      "missing field is supplyable",
			problems:  []skillstate.Problem{{Kind: skillstate.MissingField, Field: "BASE"}},
			wantState: facade.RoutePendingCredentials,
			wantMiss:  []string{"BASE"},
		},
		{
			// Starting a Secret Service or exporting the variable clears this,
			// so the route must stay promotable. Breaking it would strand every
			// skill with a required secret on a headless host behind a 502 that
			// never recovers — the CI failure that caught the earlier mapping.
			name:      "dead keychain stays pending",
			problems:  []skillstate.Problem{{Kind: skillstate.KeychainUnavailable, Field: "TOKEN", Detail: "no bus", Fix: "install gnome-keyring"}},
			wantState: facade.RoutePendingCredentials,
			wantMiss:  []string{"TOKEN"},
		},
		{
			// Fixing the export, or storing a valid value (the keychain wins
			// over env_passthrough), clears this too.
			name:      "malformed env secret stays pending",
			problems:  []skillstate.Problem{{Kind: skillstate.InvalidSecret, Field: "TOKEN", Detail: "bad shape", Fix: "fix it"}},
			wantState: facade.RoutePendingCredentials,
			wantMiss:  []string{"TOKEN"},
		},
		{
			name:      "bundle drift needs a re-register",
			problems:  []skillstate.Problem{{Kind: skillstate.BundleDrift, Detail: "changed", Fix: "omac register --force x"}},
			wantState: facade.RouteBroken,
		},
		{
			name:      "broken meta needs a re-register",
			problems:  []skillstate.Problem{{Kind: skillstate.MetaBroken, Detail: "bad yaml", Fix: "omac register --force x"}},
			wantState: facade.RouteBroken,
		},
		{
			// A terminal cause outranks a credential one: telling the user to
			// supply a value is pointless while the bundle has drifted.
			name: "terminal outranks pending",
			problems: []skillstate.Problem{
				{Kind: skillstate.MissingField, Field: "BASE"},
				{Kind: skillstate.BundleDrift, Detail: "changed", Fix: "omac register --force x"},
			},
			wantState: facade.RouteBroken,
		},
		{
			// Both are recoverable; the one with its own remedy sets the detail,
			// and both fields are listed.
			name: "keychain detail wins, both fields listed",
			problems: []skillstate.Problem{
				{Kind: skillstate.MissingField, Field: "BASE"},
				{Kind: skillstate.KeychainUnavailable, Field: "TOKEN", Detail: "no bus", Fix: "install gnome-keyring"},
			},
			wantState: facade.RoutePendingCredentials,
			wantMiss:  []string{"BASE", "TOKEN"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reloadStubRoute("mnt", c.problems)
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil (ready), got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want a stub route, got nil")
			}
			if got.State != c.wantState {
				t.Errorf("state = %q, want %q", got.State, c.wantState)
			}
			if got.Detail == "" {
				t.Error("a stub route must carry a diagnostic")
			}
			if got.Mount != "mnt" {
				t.Errorf("mount = %q, want mnt", got.Mount)
			}
			if len(got.Missing) != len(c.wantMiss) {
				t.Fatalf("missing = %v, want %v", got.Missing, c.wantMiss)
			}
			for i := range c.wantMiss {
				if got.Missing[i] != c.wantMiss[i] {
					t.Errorf("missing = %v, want %v", got.Missing, c.wantMiss)
				}
			}
		})
	}
}

func TestStartReloaderDirsEndpoint(t *testing.T) {
	r := newStartReloaderForTest(t)
	r.markMounted("slack", "slack")
	mux := r.startTestMux()
	req := httptest.NewRequest("GET", "/__omac__/dirs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dirs status=%d", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("empty dirs body")
	}
}

func TestStartReloaderActivateNot404(t *testing.T) {
	r := newStartReloaderForTest(t)
	r.markMounted("slack", "slack")
	mux := r.startTestMux()

	// The plugin (built for serve) calls activate; start must answer 200 with
	// a serve-shaped manifest, not 404.
	body := `{"dir":"` + r.env.Workdir + `"}`
	req := httptest.NewRequest("POST", "/__omac__/activate", stringReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("activate status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{`"skills"`, `"dir_token"`, `"slack"`, `"base"`} {
		if !contains(out, want) {
			t.Errorf("activate manifest missing %s: %s", want, out)
		}
	}
}

func TestStartReloaderSessionReport(t *testing.T) {
	r := newStartReloaderForTest(t)
	mux := r.startTestMux()
	if r.reportedSession() != "" {
		t.Fatal("no session should be reported yet")
	}

	post := func(body string) int {
		req := httptest.NewRequest("POST", "/__omac__/session", stringReader(body))
		req.Header.Set("content-type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(`{"session":"ses_abc"}`); code != 200 {
		t.Fatalf("session report status=%d, want 200", code)
	}
	if got := r.reportedSession(); got != "ses_abc" {
		t.Errorf("reportedSession=%q, want ses_abc", got)
	}
	// Last report wins (a run may touch more than one session).
	post(`{"session":"ses_def"}`)
	if got := r.reportedSession(); got != "ses_def" {
		t.Errorf("reportedSession=%q, want ses_def", got)
	}
	// An empty/malformed report must not clobber a known id.
	post(`{"session":""}`)
	post(`not json`)
	if got := r.reportedSession(); got != "ses_def" {
		t.Errorf("empty/bad report clobbered id: %q", got)
	}
}

// startTestMux builds the same routes startControlPlane wires, for testing.
func (r *startReloader) startTestMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/__omac__/reload", r.handleReload)
	m.HandleFunc("/__omac__/dirs", r.handleDirs)
	m.HandleFunc("/__omac__/activate", r.handleActivate)
	m.HandleFunc("/__omac__/deactivate", r.handleActivate)
	m.HandleFunc("/__omac__/reload-global", r.handleReloadGlobalStart)
	m.HandleFunc("/__omac__/session", r.handleSession)
	return m
}

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }
func contains(s, sub string) bool           { return strings.Contains(s, sub) }

// TestStartReloaderPrunesDeregisteredNotReady: a skill deregistered mid-session
// must stop being advertised. Otherwise the manifest keeps telling the agent to
// run `omac secrets set <skill>` (internal/manifest renders exactly that for a
// pending-credentials entry) for a skill that no longer exists.
func TestStartReloaderPrunesDeregisteredNotReady(t *testing.T) {
	r := newStartReloaderForTest(t)
	wd := r.env.Workdir

	stageSkillWithSecret(t, wd, "slack")
	if code := runRegister([]string{"slack", "--no-secrets"}, r.env); code != ExitOK {
		t.Fatalf("register exit=%d", code)
	}
	r.reload()
	if len(r.notReady) != 1 || !r.facade.HasRoute("", "slack") {
		t.Fatalf("expected slack tracked as not-ready with a stub route, got %v", r.notReady)
	}

	if code := runDeregister([]string{"slack"}, r.env); code != ExitOK {
		t.Fatalf("deregister exit=%d", code)
	}
	r.reload()

	if len(r.notReady) != 0 {
		t.Errorf("notReady = %v, want empty after deregister", r.notReady)
	}
	if skills := r.manifest()["skills"].([]map[string]any); len(skills) != 0 {
		t.Errorf("manifest = %v, want no phantom skill", skills)
	}
	if r.facade.HasRoute("", "slack") {
		t.Error("stub route should be removed with the registration")
	}
}

// TestStartReloaderReportsBrokenMeta: an unparseable omac.yaml is a not-ready
// cause like any other. Dropping it silently was the very bug the notReady
// tracking exists to fix — an agent that had just edited the file would watch it
// vanish from the manifest with no explanation.
func TestStartReloaderReportsBrokenMeta(t *testing.T) {
	r := newStartReloaderForTest(t)
	wd := r.env.Workdir

	stageSkillWithSecret(t, wd, "slack")
	if code := runRegister([]string{"slack", "--no-secrets"}, r.env); code != ExitOK {
		t.Fatalf("register exit=%d", code)
	}
	// Break the meta AFTER registering, the way an agent editing the workdir would.
	metaPath := filepath.Join(wd, ".opencode", "skills", "slack", "omac.yaml")
	if err := os.WriteFile(metaPath, []byte("name: slack\nsidecar: [this is not a mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.reload()

	if r.isMounted("slack") {
		t.Error("a skill with a broken omac.yaml must not mount")
	}
	skills := r.manifest()["skills"].([]map[string]any)
	if len(skills) != 1 {
		t.Fatalf("manifest = %v, want the broken skill reported, not dropped", skills)
	}
	if got := skills[0]["state"]; got != string(facade.RouteBroken) {
		t.Errorf("state = %v, want broken", got)
	}
	if detail, _ := skills[0]["detail"].(string); !strings.Contains(detail, "omac.yaml") {
		t.Errorf("detail = %q, want it to name the file", detail)
	}
}
