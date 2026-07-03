package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// auditFlags carries the CLI-level audit switches shared by start and serve.
type auditFlags struct {
	logPath string // --audit-log ("" = use config/default)
	disable bool   // --no-audit
	strict  bool   // --audit-strict
}

// resolveAuditConfig merges the launcher config's audit block with the CLI
// flags (precedence: flag > config > default) into an audit.Config, and
// enforces the --no-audit + --audit-strict misuse rule.
//
// mode/version/secretValues/fatal are supplied by the caller (they differ
// between start and serve). Returns the config and an error string for the
// misuse case (empty when OK).
func resolveAuditConfig(cfg config.AuditConfig, fl auditFlags, mode audit.Mode, version string, secretValues []string, fatal func(error)) (audit.Config, string) {
	if fl.disable && fl.strict {
		return audit.Config{}, "--no-audit cannot be combined with --audit-strict"
	}

	enabled := cfg.AuditEnabled()
	if fl.disable {
		enabled = false
	}

	path := cfg.Path
	if fl.logPath != "" {
		path = fl.logPath
	}

	strict := cfg.Strict || fl.strict

	return audit.Config{
		Enabled:      enabled,
		Path:         path,
		Syslog:       cfg.Syslog,
		Strict:       strict,
		Mode:         mode,
		Version:      version,
		SecretValues: secretValues,
		Fatal:        fatal,
	}, ""
}

// newAuditor builds the Auditor from a resolved audit.Config. On a
// non-strict open failure it warns and falls back to the no-op auditor so
// the run proceeds; on a strict open failure it returns the error so the
// caller can abort before launching the inner command.
func newAuditor(env *Env, ac audit.Config) (audit.Auditor, error) {
	a, err := audit.New(ac)
	if err != nil {
		if ac.Strict {
			return nil, err
		}
		fmt.Fprintf(env.Stderr, "[warn] audit log unavailable (%v); continuing without an audit trail\n", err)
		return audit.Nop(), nil
	}
	return a, nil
}
