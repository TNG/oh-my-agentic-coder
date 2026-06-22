package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
)

// maybeSync loads .opencode/skills.yaml and, if present, ensures the declared
// skills are installed and registered. Called from runStart after sidecars are
// healthy and before the inner harness is exec'd.
//
// Returns ExitOK on success. The sync is non-blocking: a missing marketplace
// sidecar or skills with absent required secrets are handled gracefully with
// warnings; they do not abort the session.
func maybeSync(env *Env, discovered []skillsource.Entry, reg *registry.Registry, tcpPort int, prune bool, harness config.Harness) int {
	manifest, found, err := LoadSkillManifest(env.Workdir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "[warn] omac start: skills.yaml: %v\n", err)
		return ExitOK // non-fatal
	}
	if !found {
		return ExitOK // no manifest — fast path, zero overhead
	}

	// Fast path: every manifest skill already on disk and in registry?
	if allSynced(manifest, discovered, reg) && !prune {
		return ExitOK
	}

	// Locate the marketplace sidecar port via the facade.
	marketplaceURL := fmt.Sprintf("http://127.0.0.1:%d/skill-marketplace", tcpPort)
	if !marketplaceReachable(marketplaceURL) {
		fmt.Fprintln(env.Stderr, "[warn] omac start: .opencode/skills.yaml found but skill-marketplace is not registered — skipping sync")
		return ExitOK
	}

	if err := syncManifest(env, manifest, discovered, reg, marketplaceURL, prune, harness); err != nil {
		fmt.Fprintf(env.Stderr, "[warn] omac start: sync: %v\n", err)
	}
	return ExitOK
}

// allSynced reports whether every manifest skill is both installed on disk
// and registered. When true the fast path skips all marketplace HTTP calls.
func allSynced(manifest *SkillManifest, discovered []skillsource.Entry, reg *registry.Registry) bool {
	onDisk := map[string]struct{}{}
	for _, e := range discovered {
		onDisk[e.Name] = struct{}{}
	}
	inReg := map[string]struct{}{}
	for _, e := range reg.Registered {
		inReg[e.Name] = struct{}{}
	}
	for _, s := range manifest.Skills {
		name := s.Name
		if name == "" {
			name = s.ID // fallback label (ID-only entry)
		}
		if _, ok := onDisk[name]; !ok {
			return false
		}
		if _, ok := inReg[name]; !ok {
			return false
		}
	}
	return true
}

// marketplaceReachable does a lightweight probe to confirm the marketplace
// sidecar is serving (avoids a confusing 502 from the facade when not running).
func marketplaceReachable(baseURL string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// syncManifest performs the full install-then-register loop over manifest.Skills.
func syncManifest(env *Env, manifest *SkillManifest, discovered []skillsource.Entry, reg *registry.Registry, marketplaceURL string, prune bool, harness config.Harness) error {
	client := &http.Client{Timeout: 30 * time.Second}

	onDisk := map[string]skillsource.Entry{}
	for _, e := range discovered {
		onDisk[e.Name] = e
	}
	inReg := map[string]struct{}{}
	for _, e := range reg.Registered {
		inReg[e.Name] = struct{}{}
	}

	manifestNames := map[string]struct{}{}

	for _, ms := range manifest.Skills {
		name, installErr := syncOne(env, client, ms, onDisk, inReg, marketplaceURL, harness)
		if name != "" {
			manifestNames[name] = struct{}{}
		}
		if installErr != nil {
			fmt.Fprintf(env.Stderr, "[warn] omac start: sync %q: %v\n", nameOrID(ms), installErr)
		}
	}

	if prune {
		pruneExtras(env, onDisk, manifestNames, harness)
	}
	return nil
}

// syncOne ensures a single manifest skill is installed and registered.
// Returns the resolved skill name (for prune bookkeeping) and any non-fatal error.
func syncOne(env *Env, client *http.Client, ms ManifestSkill, onDisk map[string]skillsource.Entry, inReg map[string]struct{}, marketplaceURL string, harness config.Harness) (string, error) {
	// Determine the key name for on-disk lookup.
	resolvedName := ms.Name

	entry, alreadyOnDisk := onDisk[resolvedName]

	if !alreadyOnDisk {
		// Need to install from marketplace.
		artifactID := ms.ID
		if artifactID == "" {
			// Resolve name → UUID via search.
			id, canonicalName, err := searchArtifact(client, marketplaceURL, ms.Name)
			if err != nil {
				return resolvedName, fmt.Errorf("search %q: %w", ms.Name, err)
			}
			artifactID = id
			resolvedName = canonicalName
		}

		canonicalName, err := installArtifact(client, env, marketplaceURL, artifactID, ms.Version)
		if err != nil {
			return resolvedName, fmt.Errorf("install %q: %w", nameOrID(ms), err)
		}

		// Warn if supplied name diverges from the canonical name returned by marketplace.
		if ms.Name != "" && ms.ID != "" && canonicalName != ms.Name {
			fmt.Fprintf(env.Stderr,
				"[warn] skills.yaml: id %q is artifact %q but manifest says name: %q\n"+
					"       — update the name: field in .opencode/skills.yaml\n",
				ms.ID, canonicalName, ms.Name)
		}
		if ms.ID == "" {
			resolvedName = canonicalName
		}
		fmt.Fprintf(env.Stderr, "[info] installed %s from marketplace\n", resolvedName)

		// Re-discover so we can register the newly-installed skill.
		freshDiscovered, discErr := skillsource.Discover(env.Workdir, harness)
		if discErr != nil {
			return resolvedName, fmt.Errorf("rediscover after install: %w", discErr)
		}
		for _, e := range freshDiscovered {
			if e.Name == resolvedName {
				entry = e
				alreadyOnDisk = true
				break
			}
		}
		if !alreadyOnDisk {
			return resolvedName, fmt.Errorf("installed %q but could not find it on disk afterwards", resolvedName)
		}
	} else if ms.ID != "" && ms.Name != "" {
		// Both provided and skill is already on disk — no install, but we can
		// still warn about name divergence in a future enhancement. For now skip.
	}

	// Now ensure it's registered.
	if _, registered := inReg[resolvedName]; !registered {
		registered, skipReason, err := registerSkillForSync(entry, env, harness)
		if err != nil {
			return resolvedName, fmt.Errorf("register: %w", err)
		}
		if registered {
			fmt.Fprintf(env.Stderr, "[info] registered %s\n", resolvedName)
		} else if skipReason != "" {
			fmt.Fprintf(env.Stderr, "[info] %s: installed but not registered — %s\n", resolvedName, skipReason)
		}
	}

	return resolvedName, nil
}

// searchArtifact searches the marketplace for an exact name match and returns
// its artifact UUID and canonical name. Errors when 0 or >1 exact matches are
// found — the user should add an id: field to disambiguate.
func searchArtifact(client *http.Client, marketplaceURL, name string) (string, string, error) {
	body, _ := json.Marshal(map[string]any{"query": name, "top_k": 10})
	resp, err := doPost(client, marketplaceURL+"/tools/search_published_artifacts", body)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("search HTTP %d: %s", resp.StatusCode, raw)
	}

	// The sidecar returns the MCP tool result directly — a plain JSON array.
	var artifacts []struct {
		ArtifactID string `json:"artifact_id"`
		Name       string `json:"name"`
	}
	if err := json.Unmarshal(raw, &artifacts); err != nil {
		return "", "", fmt.Errorf("decode search response: %w", err)
	}

	var matches []struct{ id, name string }
	for _, a := range artifacts {
		if a.Name == name {
			matches = append(matches, struct{ id, name string }{a.ArtifactID, a.Name})
		}
	}
	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("no marketplace artifact with name %q — check the spelling or add an id: field", name)
	case 1:
		return matches[0].id, matches[0].name, nil
	default:
		return "", "", fmt.Errorf("multiple marketplace artifacts match name %q — add an id: field to disambiguate", name)
	}
}

// installArtifact calls POST /install and returns the installed skill's canonical name.
func installArtifact(client *http.Client, env *Env, marketplaceURL, artifactID string, version *int) (string, error) {
	payload := map[string]any{"artifact_id": artifactID}
	if version != nil {
		payload["version_number"] = *version
	}
	body, _ := json.Marshal(payload)
	resp, err := doPost(client, marketplaceURL+"/install", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("install HTTP %d: %s", resp.StatusCode, raw)
	}

	var result struct {
		Upstream struct {
			Name string `json:"name"`
		} `json:"upstream"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Upstream.Name == "" {
		// Fall back to artifact ID as the name if the response is unexpected.
		return artifactID, nil
	}
	return result.Upstream.Name, nil
}

// doPost performs a POST with JSON body and returns the response.
func doPost(client *http.Client, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

// registerSkillForSync registers a discovered skill non-interactively.
// It checks the keychain for required secrets first; if any are absent the
// skill is skipped (installed but unregistered) so the session starts cleanly.
// Returns (registered bool, skipReason string, err error).
func registerSkillForSync(entry skillsource.Entry, env *Env, harness config.Harness) (bool, string, error) {
	metaPath := entry.Dir + "/" + config.MetaFileName
	meta, err := config.LoadMeta(metaPath)
	if err != nil {
		return false, "", fmt.Errorf("load meta: %w", err)
	}
	if meta.Sidecar == nil {
		return false, "no sidecar block in omac.yaml", nil
	}

	global := entry.Kind == "user-global"
	secretScope := ""
	if !global {
		secretScope = keychain.WorkdirID(env.Workdir)
	}

	// Check required secrets. If any are absent, skip registration so the
	// session does not fail on startup with a "required secret missing" error.
	for _, spec := range meta.Sidecar.Secrets {
		if !spec.IsRequired() {
			continue
		}
		_, err := keychain.GetWithFallback(secretScope, entry.Name, spec.Name)
		if err != nil {
			if errors.Is(err, keychain.ErrNotFound) {
				return false, fmt.Sprintf("required secret %q missing — run: omac register %s", spec.Name, entry.Name), nil
			}
			return false, "", fmt.Errorf("keychain: %w", err)
		}
	}

	bundleHash, err := config.BundleHash(entry.Dir)
	if err != nil {
		return false, "", fmt.Errorf("bundle hash: %w", err)
	}

	declaredNames := make([]string, 0, len(meta.Sidecar.Secrets))
	for _, s := range meta.Sidecar.Secrets {
		declaredNames = append(declaredNames, s.Name)
	}

	// Preserve any previously-stored skipped lists so a later interactive
	// `omac register` still benefits from the user's prior answers.
	prevSkippedSecrets, prevSkippedFields := loadPrevSkipped(env.Workdir, entry.Name, global)
	skippedSecrets := make([]string, 0, len(prevSkippedSecrets))
	for n := range prevSkippedSecrets {
		skippedSecrets = append(skippedSecrets, n)
	}
	skippedFields := make([]string, 0, len(prevSkippedFields))
	for n := range prevSkippedFields {
		skippedFields = append(skippedFields, n)
	}

	stored := entry.Dir
	if entry.Kind == "workdir" {
		stored = rel(env.Workdir, entry.Dir)
	}

	if err := withRegistryLock(env.Workdir, global, func() error {
		reg, err := loadRegistry(env.Workdir, global)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:                entry.Name,
			Harness:             harness.Name,
			SkillDir:            stored,
			BundleHash:          bundleHash,
			RegisteredAt:        time.Now().UTC(),
			DeclaredSecretNames: dedupSorted(declaredNames),
			SkippedSecretNames:  dedupSorted(skippedSecrets),
			SkippedConfigFields: dedupSorted(skippedFields),
		})
		return saveRegistry(env.Workdir, global, reg)
	}); err != nil {
		return false, "", fmt.Errorf("registry: %w", err)
	}

	return true, "", nil
}

// pruneExtras uninstalls discovered skills not listed in the manifest.
func pruneExtras(env *Env, onDisk map[string]skillsource.Entry, manifestNames map[string]struct{}, harness config.Harness) {
	for name, entry := range onDisk {
		if _, keep := manifestNames[name]; keep {
			continue
		}
		registered := false
		global := false
		if wReg, err := registry.Load(env.Workdir); err == nil {
			if e, _ := wReg.FindForHarness(name, harness.Name); e != nil {
				registered = true
			}
		}
		if !registered {
			if gReg, err := registry.LoadGlobal(); err == nil {
				if e, _ := gReg.FindForHarness(name, harness.Name); e != nil {
					registered = true
					global = true
				}
			}
		}
		if registered {
			if code := deregisterSkill(name, harness.Name, env, global, true); code != ExitOK {
				fmt.Fprintf(env.Stderr, "[warn] omac start: prune: could not deregister %s\n", name)
				continue
			}
		}
		if err := os.RemoveAll(entry.Dir); err != nil {
			fmt.Fprintf(env.Stderr, "[warn] omac start: prune: remove %s: %v\n", name, err)
			continue
		}
		fmt.Fprintf(env.Stderr, "[info] pruned %s (not in %s)\n", name, skillManifestRelPath)
	}
}

func nameOrID(ms ManifestSkill) string {
	if ms.Name != "" {
		return ms.Name
	}
	return ms.ID
}
