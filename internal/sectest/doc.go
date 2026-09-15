// Package sectest holds the environment guards shared by omac's security
// regression suite. Its contents are behind the "vuln" build tag; this file
// is untagged so the package still has a file in the default build, which
// per-package tooling (go vet ./internal/sectest/, linters, editors) requires.
package sectest
