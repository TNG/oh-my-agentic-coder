package sandboxprofile

import (
	"runtime"
)

// Baseline is the implicit platform grant/deny set merged into every
// resolved profile. It replaces nono's implicit default-profile groups
// (deny_credentials, deny_keychains_*, system_read_*, system_write_*,
// homebrew_*, user_tools, ...). Paths here are pre-expansion (may use ~
// and $VAR) and are filtered for existence at launch time.
type Baseline struct {
	// Read: read-only system paths required for process execution.
	Read []string
	// Write: temp/device paths that must stay writable.
	Write []string
	// ProtectedPaths are denied even when covered by a broader grant,
	// unless listed in filesystem.override_deny.
	ProtectedPaths []string
	// WorkdirProtected are basenames (e.g. ".env") denied inside the
	// workdir and any explicitly granted tree. Unlike ProtectedPaths,
	// they are resolved at launch time against the actual workdir, so
	// they catch workdir-relative files the static ProtectedPaths set
	// cannot.
	WorkdirProtected []string
}

// PlatformBaseline returns the baseline for the current GOOS.
func PlatformBaseline() Baseline {
	if runtime.GOOS == "darwin" {
		return darwinBaseline()
	}
	return linuxBaseline()
}

// protectedCommon is the cross-platform protected-path set (nono's
// deny_credentials + deny_shell_history + deny_shell_configs groups).
func protectedCommon() []string {
	return []string{
		// credentials
		"~/.ssh",
		"~/.gnupg",
		"~/.aws",
		"~/.azure",
		"~/.config/gcloud",
		"~/.gcloud",
		"~/.kube",
		"~/.docker",
		"~/.git-credentials",
		"~/.netrc",
		"~/.npmrc",
		"~/.vault-token",
		"~/.credentials",
		"~/.secrets",
		"~/.keys",
		"~/.pki",
		"~/.terraform.d",
		"~/.config/op",
		// shell history
		"~/.bash_history",
		"~/.zsh_history",
		"~/.history",
		"~/.python_history",
		// shell configs (may embed secrets)
		"~/.zshrc",
		"~/.zprofile",
		"~/.zshenv",
		"~/.zlogin",
		"~/.zlogout",
		"~/.bashrc",
		"~/.bash_profile",
		"~/.bash_login",
		"~/.bash_logout",
		"~/.profile",
		"~/.config/fish",
		"~/.env",
		"~/.envrc",
	}
}

// workdirProtectedCommon are basenames denied inside the workdir and
// any explicitly granted tree. Resolved at launch time against the
// actual workdir (see ResolveGrants), so workdir-relative files like
// `<workdir>/.env` are blocked by default without a --deny flag.
func workdirProtectedCommon() []string {
	return []string{
		".env",
		".envrc",
	}
}

func darwinBaseline() Baseline {
	return Baseline{
		Read: []string{
			"/bin", "/sbin", "/usr/bin", "/usr/sbin",
			"/usr/local/bin", "/usr/lib", "/usr/local/lib", "/usr/share",
			"/System", "/Library",
			"/dev",
			"/private/var/db/dyld", "/var/db",
			"/private/var/select", "/var/select",
			"/etc", "/private/etc",
			"/usr/share/zoneinfo", "/usr/share/terminfo",
			"/var/db/timezone",
			"/opt", "/Applications",
			// Homebrew
			"/opt/homebrew", "/usr/local/Cellar", "/usr/local/opt",
			// user tools
			"~/.local/bin",
			"$TMPDIR",
			"/tmp", "/private/tmp",
			"/var/folders", "/private/var/folders",
		},
		Write: []string{
			"/private/tmp", "/tmp",
			"/private/var/folders", "/var/folders",
			"/dev",
			"$TMPDIR",
		},
		ProtectedPaths: append(protectedCommon(),
			// container sockets: root-equivalent host access if reachable
			"/var/run/docker.sock",
			// keychains / password stores
			"~/Library/Keychains",
			"/Library/Keychains",
			"~/.password-store",
			"~/.1password",
			"~/Library/Group Containers/2BUA8C4S2C.com.1password",
			"~/Library/Application Support/1Password",
			"~/Library/Containers/com.1password.1password",
			// browser data
			"~/Library/Application Support/Google/Chrome",
			"~/Library/Application Support/Firefox",
			"~/Library/Application Support/Microsoft Edge",
			"~/Library/Application Support/Arc",
			"~/Library/Application Support/Brave Browser",
			"~/Library/Safari",
			// private user data
			"~/Library/Messages",
			"~/Library/Mail",
			"~/Library/Cookies",
			"~/Library/Containers/com.apple.Safari",
			"~/Library/Application Support/MobileSync",
		),
		WorkdirProtected: workdirProtectedCommon(),
	}
}

func linuxBaseline() Baseline {
	return Baseline{
		Read: []string{
			"/bin", "/sbin", "/usr",
			"/lib", "/lib64",
			"/etc",
			"/run/systemd/resolve", // resolv.conf indirection on systemd hosts
			"~/.local/bin",
			"$TMPDIR",
		},
		Write: []string{
			"/tmp",
			"$TMPDIR",
		},
		ProtectedPaths: append(protectedCommon(),
			// container sockets: root-equivalent host access if reachable.
			// Both entries are listed because /var/run is a symlink to
			// /run on most modern distros but not universally, and
			// ProtectedPaths matching is a literal string prefix check
			// (no symlink resolution).
			"/var/run/docker.sock",
			"/run/docker.sock",
			// keyring / password stores
			"~/.password-store",
			"~/.local/share/keyrings",
			"~/.config/keyrings",
			// browser data
			"~/.mozilla",
			"~/.config/google-chrome",
			"~/.config/chromium",
			"~/.config/microsoft-edge",
			"~/.config/BraveSoftware",
		),
		WorkdirProtected: workdirProtectedCommon(),
	}
}

// EffectiveProtectedPaths returns the platform protected set minus the
// profile's override_deny holes. Comparison happens on the expanded
// form of both lists; entries that fail to expand are kept verbatim.
func EffectiveProtectedPaths(b Baseline, overrideDeny []string) []string {
	overrides := BuildOverrideLookup(overrideDeny)
	var out []string
	for _, p := range b.ProtectedPaths {
		exp, err := ExpandPath(p)
		if err != nil {
			out = append(out, p)
			continue
		}
		if overrides[exp] {
			continue
		}
		out = append(out, exp)
	}
	return out
}

// BuildOverrideLookup creates a set keyed by both the raw and expanded
// form of each override_deny entry, so overrides work as bare basenames
// (e.g. ".env") or absolute paths. Shared by EffectiveProtectedPaths
// and the workdir-protected resolver to keep the override-keying policy
// in one place.
func BuildOverrideLookup(overrideDeny []string) map[string]bool {
	overrides := make(map[string]bool, len(overrideDeny))
	for _, o := range overrideDeny {
		overrides[o] = true
		if exp, err := ExpandPath(o); err == nil {
			overrides[exp] = true
		}
	}
	return overrides
}
