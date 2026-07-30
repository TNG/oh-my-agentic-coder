package buildmanifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeManifest writes `.omac/build.yaml` under wt with the given content.
func writeManifest(t *testing.T, wt, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_MissingFileIsZeroManifest(t *testing.T) {
	// Criterion 1: a standard Gradle project requires no manifest.
	wt := t.TempDir()
	m, err := Load(wt)
	if err != nil {
		t.Fatalf("missing manifest should not error, got: %v", err)
	}
	if m == nil {
		t.Fatal("Load returned nil manifest for missing file")
	}
	if m.HasManifest() {
		t.Errorf("missing-file manifest should be zero, got %+v", m)
	}
	if err := m.Validate(HostPolicy{}); err != nil {
		t.Errorf("zero manifest should validate clean, got: %v", err)
	}
}

func TestLoad_EmptyDocumentIsZeroManifest(t *testing.T) {
	wt := t.TempDir()
	writeManifest(t, wt, "")
	m, err := Load(wt)
	if err != nil {
		t.Fatalf("empty file: %v", err)
	}
	if m.HasManifest() {
		t.Errorf("empty file should be zero manifest, got %+v", m)
	}
}

func TestLoad_Yarp3Example(t *testing.T) {
	// Criterion 2: parse the spec's illustrative yarp3 manifest.
	wt := t.TempDir()
	writeManifest(t, wt, `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
        - minio/minio:latest
`)
	m, err := Load(wt)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("Version = %d, want 1", m.Version)
	}
	if len(m.Builds) != 1 {
		t.Fatalf("Builds = %d, want 1", len(m.Builds))
	}
	if m.Builds[0].Root != "backend" {
		t.Errorf("Root = %q, want backend", m.Builds[0].Root)
	}
	if m.Builds[0].Tool != "gradle" {
		t.Errorf("Tool = %q, want gradle", m.Builds[0].Tool)
	}
	if m.Builds[0].Containers == nil || len(m.Builds[0].Containers.Images) != 2 {
		t.Fatalf("Images = %v, want 2", m.Builds[0].Containers)
	}
	want := []string{"pgvector/pgvector:pg16", "minio/minio:latest"}
	for i, w := range want {
		if m.Builds[0].Containers.Images[i] != w {
			t.Errorf("Image[%d] = %q, want %q", i, m.Builds[0].Containers.Images[i], w)
		}
	}
	if err := m.Validate(HostPolicy{}); err != nil {
		t.Errorf("yarp3 manifest should validate, got: %v", err)
	}
}

func TestLoad_WithRegistriesAndResources(t *testing.T) {
	wt := t.TempDir()
	writeManifest(t, wt, `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
registries:
  - alias: internal
    upstream: ghcr.io/tng
resources:
  maxHeap: 3g
  maxDuration: 45m
  maxCPU: 4
  maxProcesses: 512
`)
	m, err := Load(wt)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Registries) != 1 || m.Registries[0].Alias != "internal" || m.Registries[0].Upstream != "ghcr.io/tng" {
		t.Errorf("Registries = %+v", m.Registries)
	}
	if m.Resources == nil || m.Resources.MaxHeap != "3g" || m.Resources.MaxCPU != 4 {
		t.Errorf("Resources = %+v", m.Resources)
	}
	if m.Resources.MaxDuration != 45*time.Minute {
		t.Errorf("MaxDuration = %v, want 45m", m.Resources.MaxDuration)
	}
	// Within ceiling → valid.
	if err := m.Validate(HostPolicy{MaxHeap: "4g", MaxDuration: time.Hour, MaxCPU: 8, MaxProcesses: 1024}); err != nil {
		t.Errorf("validate within ceiling: %v", err)
	}
}

func TestValidate_RejectsSecretFields(t *testing.T) {
	// Criterion 2: a manifest with a secret field is rejected at parse time.
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "registry with password",
			yaml: `version: 1
builds:
  - root: backend
registries:
  - alias: internal
    upstream: ghcr.io/tng
    password: hunter2
`,
			wantSub: "secret field rejected",
		},
		{
			name: "registry with token",
			yaml: `version: 1
registries:
  - alias: internal
    upstream: ghcr.io/tng
    token: abc123
`,
			wantSub: "secret field rejected",
		},
		{
			name: "registry with credential",
			yaml: `version: 1
registries:
  - alias: internal
    upstream: ghcr.io/tng
    credential: secret
`,
			wantSub: "secret field rejected",
		},
		{
			name: "build with apikey",
			yaml: `version: 1
builds:
  - root: backend
    apikey: abc
`,
			wantSub: "secret field rejected",
		},
		{
			name: "registry with auth",
			yaml: `version: 1
registries:
  - alias: internal
    upstream: ghcr.io/tng
    auth: bearer xyz
`,
			wantSub: "secret field rejected",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), c.wantSub)
			}
			// Also ensure it's a *ManifestError (CLI maps to ExitPolicyDenied).
			var me *ManifestError
			if !errors.As(err, &me) {
				t.Errorf("error should be *ManifestError, got %T", err)
			}
		})
	}
}

func TestValidate_EmptySecretValueAllowed(t *testing.T) {
	// A secret-named field with an empty value is allowed (no secret present);
	// only a non-empty value is rejected. This lets a manifest include a
	// commented-out placeholder without failing.
	_, err := Parse([]byte(`version: 1
registries:
  - alias: internal
    upstream: ghcr.io/tng
    password: ""
`))
	if err != nil {
		t.Fatalf("empty secret value should be allowed, got: %v", err)
	}
}

func TestValidate_RejectsAbsoluteRoot(t *testing.T) {
	// Criterion 8: a manifest with an absolute root is rejected.
	_, err := Parse([]byte(`version: 1
builds:
  - root: /Users/me/project/backend
`))
	if err == nil {
		t.Fatal("expected absolute-root rejection")
	}
	if !strings.Contains(err.Error(), "absolute root") {
		t.Errorf("error = %q, want 'absolute root'", err.Error())
	}
}

func TestValidate_RejectsTraversalRoot(t *testing.T) {
	_, err := Parse([]byte(`version: 1
builds:
  - root: ../backend
`))
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("error = %q, want '..'", err.Error())
	}
}

func TestValidate_RelativeRootWorksFromAnyWorktree(t *testing.T) {
	// Criterion 8: a manifest referencing `root: backend` works from any
	// worktree that contains `backend/` — no absolute path needed.
	_, err := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	if err != nil {
		t.Fatalf("relative root should validate: %v", err)
	}
}

func TestValidate_RejectsBadVersion(t *testing.T) {
	// Version errors are structural → raised at Parse time.
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"wrong version", "version: 2\nbuilds:\n  - root: backend\n", "unsupported manifest version"},
		{"missing version", "builds:\n  - root: backend\n", "missing version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatal("expected version error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

func TestValidate_RejectsUnsupportedTool(t *testing.T) {
	// Tool errors are structural → raised at Parse time.
	_, err := Parse([]byte(`version: 1
builds:
  - root: backend
    tool: maven
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported tool") {
		t.Errorf("error = %v, want 'unsupported tool'", err)
	}
}

func TestValidate_RegistryWithEmbeddedUserinfo(t *testing.T) {
	// A registry upstream with embedded credentials (user:pass@) is rejected.
	_, err := Parse([]byte(`version: 1
registries:
  - alias: internal
    upstream: "https://user:pass@ghcr.io/tng"
`))
	if err == nil {
		t.Fatal("expected embedded-credential rejection")
	}
	if !strings.Contains(err.Error(), "embedded credentials") {
		t.Errorf("error = %q, want 'embedded credentials'", err.Error())
	}
}

func TestValidate_ResourceAboveCeilingDenied(t *testing.T) {
	// Criterion 3: a resource request above the host policy ceiling fails.
	m, err := Parse([]byte(`version: 1
resources:
  maxHeap: 8g
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	host := HostPolicy{MaxHeap: "4g"}
	err = m.Validate(host)
	if err == nil {
		t.Fatal("expected ceiling rejection")
	}
	if !strings.Contains(err.Error(), "exceeds host ceiling") {
		t.Errorf("error = %q, want 'exceeds host ceiling'", err.Error())
	}
	var me *ManifestError
	if !errors.As(err, &me) {
		t.Errorf("want *ManifestError, got %T", err)
	}
}

func TestValidate_ResourceAtCeilingOK(t *testing.T) {
	m, err := Parse([]byte(`version: 1
resources:
  maxHeap: 4g
  maxDuration: 30m
  maxCPU: 4
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	host := HostPolicy{MaxHeap: "4g", MaxDuration: 30 * time.Minute, MaxCPU: 4}
	if err := m.Validate(host); err != nil {
		t.Errorf("at-ceiling should be OK: %v", err)
	}
}

func TestValidate_ResourceBelowCeilingOK(t *testing.T) {
	m, _ := Parse([]byte(`version: 1
resources:
  maxHeap: 1g
`))
	if err := m.Validate(HostPolicy{MaxHeap: "4g"}); err != nil {
		t.Errorf("below-ceiling should be OK: %v", err)
	}
}

func TestValidate_ResourceAbsentHostDefaultApplies(t *testing.T) {
	// Criterion 3 (first half): host defaults apply when requests absent.
	// A manifest with no resources block validates regardless of ceiling.
	m, _ := Parse([]byte(`version: 1
builds:
  - root: backend
`))
	if err := m.Validate(HostPolicy{MaxHeap: "2g"}); err != nil {
		t.Errorf("absent resources should use host default (no error): %v", err)
	}
}

func TestValidate_DurationAboveCeiling(t *testing.T) {
	m, _ := Parse([]byte(`version: 1
resources:
  maxDuration: 2h
`))
	err := m.Validate(HostPolicy{MaxDuration: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "exceeds host ceiling") {
		t.Errorf("error = %v, want 'exceeds host ceiling'", err)
	}
}

func TestValidate_CPUDAboveCeiling(t *testing.T) {
	m, _ := Parse([]byte(`version: 1
resources:
  maxCPU: 16
`))
	err := m.Validate(HostPolicy{MaxCPU: 8})
	if err == nil || !strings.Contains(err.Error(), "exceeds host ceiling") {
		t.Errorf("error = %v, want 'exceeds host ceiling'", err)
	}
}

// TestValidate_RequestAgainstZeroCeilingFailsClosed asserts spec.md:150:
// OMAC "provides host-owned defaults and ceilings for CPU, memory, process
// count." A zero host ceiling means the host has NOT authorized that
// dimension, so a manifest request for it is fail-closed denied with an
// actionable message naming the dimension — rather than silently letting
// any value through.
func TestValidate_RequestAgainstZeroCeilingFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		wantSub  string
	}{
		{"CPU", "version: 1\nresources:\n  maxCPU: 4\n", "no max-CPU ceiling configured"},
		{"Processes", "version: 1\nresources:\n  maxProcesses: 512\n", "no max-processes ceiling configured"},
		{"Duration", "version: 1\nresources:\n  maxDuration: 30m\n", "no max-duration ceiling configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := Parse([]byte(c.manifest))
			// Zero host ceiling on the requested dimension.
			err := m.Validate(HostPolicy{MaxHeap: "2g"})
			if err == nil {
				t.Fatalf("want denial naming %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error = %v, want substring %q", err, c.wantSub)
			}
		})
	}
}

func TestValidate_ForbiddenFieldRejected(t *testing.T) {
	// Criterion 7: a manifest with a forbidden-shape field yields a
	// HostForbiddenError.
	_, err := Parse([]byte(`version: 1
builds:
  - root: backend
    containers:
      images:
        - pgvector/pgvector:pg16
      bindMounts:
        - /Users/me/.ssh
`))
	if err == nil {
		t.Fatal("expected forbidden-field rejection")
	}
	var hfe *HostForbiddenError
	if !errors.As(err, &hfe) {
		t.Errorf("want *HostForbiddenError, got %T: %v", err, err)
	}
	rendered := hfe.Render()
	for _, want := range []string{"forbidden by host policy", "cannot be enabled through", ".omac/build.yaml"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("render missing %q:\n%s", want, rendered)
		}
	}
}

func TestParseHeap(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2g", 2 * 1024 * 1024 * 1024, true},
		{"512m", 512 * 1024 * 1024, true},
		{"1024k", 1024 * 1024, true},
		{"8192", 8192, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := parseHeap(c.in)
		if ok != c.ok || (c.ok && got != c.want) {
			t.Errorf("parseHeap(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
