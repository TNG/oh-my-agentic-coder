package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// runCache dispatches `omac cache <verb>`.
//
//	omac cache clear           Remove the current workdir's cache scope.
//	omac cache clear --all     Remove every inactive cache scope.
//
// `--all` is the explicit destructive confirmation: there is no
// interactive prompt. Active scopes (lock held) are reported and left
// intact; unsafe or replaced scopes are reported as skipped.
func runCache(args []string, env *Env) int {
	if len(args) == 0 {
		printCacheUsage(env)
		return ExitMisuse
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "clear":
		return runCacheClear(rest, env)
	case "--help", "-h", "help":
		printCacheUsage(env)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "omac cache: unknown verb %q\n", verb)
		printCacheUsage(env)
		return ExitMisuse
	}
}

func runCacheClear(args []string, env *Env) int {
	all := false
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		case "--help", "-h":
			printCacheUsage(env)
			return ExitOK
		default:
			fmt.Fprintf(env.Stderr, "omac cache clear: unknown argument %q\n", a)
			printCacheUsage(env)
			return ExitMisuse
		}
	}

	if all {
		results, err := toolcache.ClearAll()
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac cache clear --all:", err)
			return ExitIOError
		}
		for _, r := range results {
			renderClearResult(env, r)
		}
		return ExitOK
	}

	result, err := toolcache.ClearWorkdir(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac cache clear:", err)
		return ExitIOError
	}
	renderClearResult(env, result)
	return ExitOK
}

func renderClearResult(env *Env, r toolcache.ClearResult) {
	if r.Reason == "" {
		fmt.Fprintf(env.Stdout, "%s  %s\n", r.Status, r.Path)
		return
	}
	fmt.Fprintf(env.Stdout, "%s  %s  (%s)\n", r.Status, r.Path, r.Reason)
}

func printCacheUsage(env *Env) {
	fmt.Fprintln(env.Stderr, `omac cache — manage the tool cache

Usage:
  omac cache clear           Remove the current workdir's cache scope.
  omac cache clear --all     Remove every inactive cache scope (destructive).

A scope is reported as:
  removed   — inactive scope deleted
  active    — scope lock is held, left intact
  skipped   — scope is unsafe, missing, or was replaced`)
}
