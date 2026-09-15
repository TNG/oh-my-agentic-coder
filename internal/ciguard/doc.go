// Package ciguard holds security regression tests for omac's own CI
// workflows and release scripts (build tag "vuln"). Its contents are
// behind that tag; this file is untagged so the package still has a file
// in the default build, which per-package tooling (go vet ./internal/
// ciguard/, linters, editors) requires.
//
// Unlike the rest of the security suite, these properties are about YAML
// and shell text, not Go code: a workflow's `run:` block is shell that
// nothing in the normal edit loop parses, so an injection there is
// invisible to `go vet`/`go build`. Reusing this repo's own build-tag-vuln
// harness (rather than a second pin-file mechanism) keeps one CI job, one
// pin file, one runner for both classes of finding.
package ciguard
