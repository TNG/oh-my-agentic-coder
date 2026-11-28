//go:build !linux

package sandboxrun

func validateGitMountAliases(*Grants) error { return nil }
