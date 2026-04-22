package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

func runRegister(args []string, env *Env) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		force           = fs.Bool("force", false, "Update registry entry even if meta_hash differs.")
		reprompt        = fs.Bool("reprompt-secrets", false, "Re-prompt for secrets even if already stored.")
		noSecrets       = fs.Bool("no-secrets", false, "Skip all secret prompts; caller promises to supply them at start time.")
		secretsFromPath = fs.String("secrets-from", "", "Read KEY=VALUE secrets from this file instead of prompting.")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac register <skill> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	skillName := fs.Arg(0)
	skillDir := filepath.Join(env.Workdir, ".opencode", "skills", skillName)
	metaPath := filepath.Join(skillDir, "meta.yaml")

	// 1. Load + validate meta.
	meta, err := config.LoadMeta(metaPath)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac register:", err)
		if errors.Is(err, iofs.ErrNotExist) {
			return ExitPrerequisiteMissing
		}
		return ExitConfigInvalid
	}
	if meta.Sidecar == nil {
		fmt.Fprintf(env.Stderr, "omac register: skill %q has no sidecar block; nothing to register\n", skillName)
		return ExitMisuse
	}
	metaHash, err := config.HashMetaFile(metaPath)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac register: hash meta:", err)
		return ExitIOError
	}

	// 2. Secret handling.
	declaredNames := make([]string, 0, len(meta.Sidecar.Secrets))
	for _, s := range meta.Sidecar.Secrets {
		declaredNames = append(declaredNames, s.Name)
	}

	if !*noSecrets {
		fromFile, err := loadSecretsFile(*secretsFromPath)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac register:", err)
			return ExitConfigInvalid
		}
		for _, spec := range meta.Sidecar.Secrets {
			if err := handleOneSecret(env, skillName, spec, *reprompt, fromFile); err != nil {
				// Determine exit code from err message tag.
				if strings.HasPrefix(err.Error(), "keychain:") {
					fmt.Fprintln(env.Stderr, "omac register:", err)
					return ExitKeychainError
				}
				if strings.HasPrefix(err.Error(), "refused:") {
					fmt.Fprintln(env.Stderr, "omac register:", err)
					return ExitSecretRefused
				}
				fmt.Fprintln(env.Stderr, "omac register:", err)
				return ExitGeneric
			}
		}
	}

	// 3. Install-script print (never executed).
	host := osinfo.Detect()
	scriptRel := meta.Sidecar.InstallScriptFor(host)
	if scriptRel == "" {
		fmt.Fprintf(env.Stdout, "\n[info] skill %q declares no install script for OS %q; assuming the sidecar is ready to run.\n", skillName, host)
	} else {
		scriptAbs := filepath.Join(skillDir, scriptRel)
		if err := printInstallScript(env, scriptAbs, host); err != nil {
			fmt.Fprintln(env.Stderr, "omac register: print install script:", err)
			return ExitIOError
		}
	}

	// 4. Registry update (atomic, under flock).
	if err := registry.WithLock(env.Workdir, func() error {
		reg, err := registry.Load(env.Workdir)
		if err != nil {
			return err
		}
		if existing, _ := reg.Find(skillName); existing != nil {
			if existing.MetaHash != metaHash && !*force {
				return fmt.Errorf("already registered with a different meta_hash; pass --force to update")
			}
		}
		reg.Upsert(registry.Entry{
			Name:                skillName,
			SkillDir:            rel(env.Workdir, skillDir),
			MetaHash:            metaHash,
			RegisteredAt:        time.Now().UTC(),
			DeclaredSecretNames: declaredNames,
		})
		return registry.Save(env.Workdir, reg)
	}); err != nil {
		fmt.Fprintln(env.Stderr, "omac register: registry:", err)
		return ExitIOError
	}

	fmt.Fprintf(env.Stdout, "\n[ok] registered %s (workdir=%s)\n", skillName, env.Workdir)
	return ExitOK
}

// rel returns path relative to base, or the original if not reachable.
func rel(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return r
}

// handleOneSecret implements the per-secret flow from §16.4.
func handleOneSecret(env *Env, skill string, spec config.SecretSpec, reprompt bool, fromFile map[string]string) error {
	// 1. Already in keychain?
	if !reprompt {
		present, err := keychain.Has(skill, spec.Name)
		if err != nil {
			return fmt.Errorf("keychain: %w", err)
		}
		if present {
			fmt.Fprintf(env.Stderr, "  [skip] %s already in keychain\n", spec.Name)
			return nil
		}
	}

	// 2. --secrets-from file takes precedence over prompting.
	if v, ok := fromFile[spec.Name]; ok {
		if err := validatePattern(spec, v); err != nil {
			return err
		}
		s := secrets.NewSecretString(v)
		defer s.Zero()
		if err := keychain.Set(skill, spec.Name, s); err != nil {
			return fmt.Errorf("keychain: %w", err)
		}
		fmt.Fprintf(env.Stderr, "  stored %s (from file)\n", spec.Name)
		return nil
	}

	// 3. Env-based non-interactive supply: OMAC_SECRET_<NAME>.
	if v, ok := os.LookupEnv("OMAC_SECRET_" + spec.Name); ok {
		if err := validatePattern(spec, v); err != nil {
			return err
		}
		s := secrets.NewSecretString(v)
		defer s.Zero()
		if err := keychain.Set(skill, spec.Name, s); err != nil {
			return fmt.Errorf("keychain: %w", err)
		}
		fmt.Fprintf(env.Stderr, "  stored %s (from OMAC_SECRET_%s)\n", spec.Name, spec.Name)
		return nil
	}

	// 4. default_from_env offers a pre-filled default on the prompt.
	var defaultHint string
	if spec.DefaultFromEnv != "" {
		if v, ok := os.LookupEnv(spec.DefaultFromEnv); ok && v != "" {
			// Accept on empty input.
			if err := validatePattern(spec, v); err == nil {
				defaultHint = fmt.Sprintf(" (press Enter to accept value from $%s)", spec.DefaultFromEnv)
			}
		}
	}

	// 5. Interactive prompt loop.
	if spec.Description != "" {
		fmt.Fprintf(env.Stderr, "  %s: %s\n", spec.Name, spec.Description)
	}
	attempts := 0
	for {
		attempts++
		prompt := fmt.Sprintf("  enter %s%s: ", spec.Name, defaultHint)
		value, err := secrets.ReadPassword(prompt)
		if err != nil {
			return fmt.Errorf("read %s: %w", spec.Name, err)
		}
		// Empty input → accept default_from_env if offered, else treat per `required`.
		if value.IsEmpty() {
			value.Zero()
			if defaultHint != "" {
				if v, ok := os.LookupEnv(spec.DefaultFromEnv); ok && v != "" {
					s := secrets.NewSecretString(v)
					if err := keychain.Set(skill, spec.Name, s); err != nil {
						s.Zero()
						return fmt.Errorf("keychain: %w", err)
					}
					s.Zero()
					fmt.Fprintf(env.Stderr, "  stored %s (from $%s)\n", spec.Name, spec.DefaultFromEnv)
					return nil
				}
			}
			if !spec.IsRequired() {
				fmt.Fprintf(env.Stderr, "  [skip] %s (optional, not provided)\n", spec.Name)
				return nil
			}
			if attempts >= 3 {
				return fmt.Errorf("refused: required secret %q not supplied", spec.Name)
			}
			fmt.Fprintln(env.Stderr, "  [retry] required; please enter a value")
			continue
		}
		if err := validatePattern(spec, value.ExposeString()); err != nil {
			value.Zero()
			if attempts >= 3 {
				return fmt.Errorf("refused: %s does not match pattern after %d attempts", spec.Name, attempts)
			}
			fmt.Fprintf(env.Stderr, "  [retry] %s\n", err)
			continue
		}
		if err := keychain.Set(skill, spec.Name, value); err != nil {
			value.Zero()
			return fmt.Errorf("keychain: %w", err)
		}
		value.Zero()
		fmt.Fprintf(env.Stderr, "  stored %s\n", spec.Name)
		return nil
	}
}

func validatePattern(spec config.SecretSpec, v string) error {
	if spec.Pattern == "" {
		return nil
	}
	re, err := regexp.Compile(spec.Pattern)
	if err != nil {
		return fmt.Errorf("invalid pattern for %s: %w", spec.Name, err)
	}
	if !re.MatchString(v) {
		return fmt.Errorf("value for %s does not match /%s/", spec.Name, spec.Pattern)
	}
	return nil
}

// loadSecretsFile reads KEY=VALUE lines. Empty lines and # comments are skipped.
func loadSecretsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open --secrets-from: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("--secrets-from: missing '=' in line: %s", line)
		}
		key, val := line[:eq], line[eq+1:]
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read --secrets-from: %w", err)
	}
	return out, nil
}

// printInstallScript prints the script path + full contents. Never executes.
func printInstallScript(env *Env, path string, host osinfo.OS) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sep := strings.Repeat("=", 70)
	fmt.Fprintf(env.Stdout, "\n%s\n", sep)
	fmt.Fprintf(env.Stdout, "Install script for OS %q: %s\n", host, path)
	fmt.Fprintln(env.Stdout, "omac does not run this for you. Inspect it, then run:")
	fmt.Fprintf(env.Stdout, "  bash %s\n", path)
	fmt.Fprintf(env.Stdout, "%s\n", sep)
	fmt.Fprintln(env.Stdout, "# ===== BEGIN install script =====")
	_, _ = env.Stdout.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		fmt.Fprintln(env.Stdout)
	}
	fmt.Fprintln(env.Stdout, "# ===== END install script =====")
	return nil
}
