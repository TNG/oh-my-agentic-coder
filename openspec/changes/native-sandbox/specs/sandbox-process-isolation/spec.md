# sandbox-process-isolation

Kernel-enforced filesystem isolation and environment variable filtering, via Seatbelt on macOS and bubblewrap on Linux.

## ADDED Requirements

### Requirement: Default-deny filesystem isolation
The sandboxed child process SHALL only be able to access filesystem paths granted by the effective profile (profile file plus CLI flags), at the granted access level: `read` paths are readable but not writable, `write` paths writable but not readable, `allow` paths both. Grants apply recursively to directories. Isolation MUST be kernel-enforced and inherited by all descendant processes; it MUST NOT be removable from inside the sandbox.

A platform baseline SHALL be granted implicitly:
- read-only system paths required for process execution (macOS: `/bin`, `/sbin`, `/usr/bin`, `/usr/sbin`, `/usr/local/*`, `/usr/lib`, `/usr/share`, `/System`, `/Library`, `/dev`, dyld caches (`/private/var/db/dyld`), `/etc` (`/private/etc`), zoneinfo/terminfo, `/opt`, `/Applications`, Homebrew paths (`/opt/homebrew`, `/usr/local/Cellar`, `/usr/local/opt`); Linux: `/bin`, `/sbin`, `/usr`, `/lib*`, `/etc` essentials (resolv.conf, hosts, ssl, ld.so.*, locale, terminfo), `/usr/share`, `/dev` basics, `/proc/self`), plus user-local tool paths (`~/.local/bin`).
- writable temp and device paths (macOS: `/tmp` (`/private/tmp`), `/var/folders` (`/private/var/folders`), `$TMPDIR`, `/dev`; Linux: `/tmp`, `/dev/null`, `/dev/tty`, ptys, `/proc/self/fd`).

#### Scenario: Temp dir writable by default
- **WHEN** the child writes to `$TMPDIR` (macOS) or `/tmp` (Linux) without an explicit profile grant
- **THEN** the write succeeds

#### Scenario: Ungranted path is inaccessible
- **WHEN** the child attempts to read `~/.ssh/id_ed25519` and no grant covers it
- **THEN** the access fails with a permission or not-found error

#### Scenario: Read-only grant blocks writes
- **WHEN** the profile grants `~/.gitconfig` as `read` and the child attempts to write it
- **THEN** the read succeeds and the write fails

#### Scenario: Grants are inherited by children
- **WHEN** the sandboxed process spawns a subprocess (e.g. a shell running `cat`)
- **THEN** the subprocess is subject to exactly the same filesystem restrictions

### Requirement: Protected-path denials override grants
A built-in set of protected paths SHALL be denied to the child even when covered by a broader grant (e.g. a profile granting `~` read access still must not expose `~/.ssh`). The protected set SHALL include at minimum:
- credentials: `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.azure`, `~/.config/gcloud`, `~/.kube`, `~/.docker`, `~/.git-credentials`, `~/.netrc`, `~/.npmrc`, `~/.vault-token`, `~/.terraform.d`, `~/.config/op`
- keychains/password stores (macOS): `~/Library/Keychains`, `/Library/Keychains`, `~/.password-store`, 1Password data dirs
- browser data (macOS): Chrome/Firefox/Edge/Arc/Brave application-support dirs, `~/Library/Safari`
- private user data (macOS): `~/Library/Messages`, `~/Library/Mail`, `~/Library/Cookies`, `~/Library/Containers/com.apple.Safari`
- shell history and shell config files: `~/.bash_history`, `~/.zsh_history`, `~/.python_history`, `~/.zshrc`, `~/.zprofile`, `~/.zshenv`, `~/.bashrc`, `~/.bash_profile`, `~/.profile`, `~/.config/fish`, `~/.env`, `~/.envrc`
- Linux keyring/browser equivalents

A profile MAY punch holes through the protected set via `filesystem.override_deny` (string[]): listed paths are removed from the deny set (a grant is still required to actually access them). On macOS denials SHALL be emitted between read-allow and write-allow rules so granted write paths take precedence as in nono; deny rules MUST cover both literal and canonicalized path forms.

#### Scenario: SSH keys protected despite broad grant
- **WHEN** the profile grants `~/Files` as read and the child reads `~/Files/../.ssh/id_ed25519` or any path under `~/.ssh`
- **THEN** the access is denied

#### Scenario: override_deny punches a hole
- **WHEN** the profile sets `override_deny: ["~/.git-credentials"]` and grants it via `filesystem.read`
- **THEN** the child can read `~/.git-credentials` while `~/.netrc` remains denied

#### Scenario: Shell history protected
- **WHEN** the child attempts to read `~/.zsh_history`
- **THEN** the access is denied

### Requirement: User deny list masks files within granted trees
A profile MAY mask files inside otherwise-granted trees via `filesystem.deny` (string[]); the `--deny <path|glob>` flag merges into it. Each entry SHALL be classified by form:
- an entry containing a path separator, or beginning with `~` or `$`, is an explicit path: it is expanded and denied at that exact path;
- a bare basename glob (no separator, e.g. `.env` or `*.key`) SHALL be matched against the file and directory names found by walking the explicitly granted trees and the working directory, and every match denied. Baseline system trees (e.g. `/usr`) SHALL NOT be walked. A matched directory is masked as a whole and not descended into.

Denied paths SHALL be enforced with the same mechanism as the built-in protected set (macOS deny rules between read- and write-allows covering literal and canonicalized forms; Linux `--ro-bind /dev/null` for files and `--tmpfs` for directories). A malformed glob or empty entry SHALL fail profile validation.

#### Scenario: cwd .env denied while the workdir is granted
- **WHEN** the profile grants the workdir `readwrite` and sets `filesystem.deny: [".env"]`, and the working directory contains `.env` and `sub/.env`
- **THEN** both `.env` and `sub/.env` are denied while sibling files (e.g. `main.go`) remain accessible

#### Scenario: explicit deny path
- **WHEN** the profile sets `filesystem.deny: ["./config/prod.env"]` under a granted workdir
- **THEN** that exact file is denied and a same-named file in another directory is not

### Requirement: Workdir access grant
The working directory in which `omac sandbox run` is invoked SHALL be granted to the child at the level given by `workdir.access`: `none` (no grant), `read`, `write`, or `readwrite`. The child's initial working directory SHALL be that directory.

#### Scenario: Readwrite workdir
- **WHEN** `workdir.access` is `readwrite`
- **THEN** the child can create, read, modify, and delete files under the invocation directory

### Requirement: macOS enforcement via Seatbelt
On macOS the sandbox SHALL be applied by executing the child under a generated SBPL (Seatbelt) profile with a `(deny default)` baseline. The generated profile MUST:
- allow `process-exec*` and `process-fork`; scope `process-info*` and signal delivery to the child's own sandbox (`(target self)` / same-sandbox), not arbitrary host processes;
- emit `file-read*` / `file-write*` rules for the granted paths, covering both the literal and the canonicalized form of each path (e.g. `/tmp` and `/private/tmp`); emit `file-read-metadata` for ancestor directories of granted paths so path resolution works;
- restrict `file-map-executable` to readable paths (DYLD-injection defense);
- allow `mach-lookup` generally but deny the Keychain service daemons (`com.apple.SecurityServer`, `com.apple.securityd`, `com.apple.security.keychaind`, `com.apple.secd`, `com.apple.security.agent`);
- allow POSIX shared memory and the mDNSResponder Unix-socket carve-out needed for DNS resolution.

#### Scenario: Keychain inaccessible
- **WHEN** the child runs `security find-generic-password -s omac`
- **THEN** the lookup fails because Keychain daemon access is denied

#### Scenario: Symlinked temp path
- **WHEN** the profile grants `/tmp/omac-x` and the child opens `/private/tmp/omac-x/f`
- **THEN** the access succeeds

#### Scenario: Cannot signal host processes
- **WHEN** the child attempts to send a signal to a process outside the sandbox (macOS: Seatbelt signal scoping; Linux: PID namespace)
- **THEN** the attempt fails

### Requirement: Linux enforcement via bubblewrap
On Linux the sandbox SHALL be applied by executing the child under `bwrap` with: `--ro-bind` for read-granted paths, `--bind` for allow/write-granted paths, the implicit system baseline as `--ro-bind`, fresh `--proc /proc` and `--dev /dev`, `--unshare-pid --unshare-ipc --unshare-uts`, `--die-with-parent`, and `--new-session`. Paths not bound SHALL be absent from the mount namespace. The network namespace MUST NOT be unshared (loopback connectivity to the supervisor proxy and omac facade is required). If `bwrap` is not installed, launch MUST fail with an actionable error.

#### Scenario: Unbound path absent
- **WHEN** the profile grants only the workdir and the child runs `ls /root`
- **THEN** the path does not exist inside the sandbox

#### Scenario: bwrap missing
- **WHEN** `bwrap` is not on `PATH` on Linux
- **THEN** `omac sandbox run` exits non-zero with an error telling the user to install bubblewrap

### Requirement: Environment variable filtering
The child environment SHALL be constructed from scratch in three layers:
1. A fixed blocklist of injection-dangerous variables is always dropped, including at minimum: `LD_*`, `DYLD_*`, `BASH_ENV`, `ENV`, `CDPATH`, `GLOBIGNORE`, `BASH_FUNC_*`, `PROMPT_COMMAND`, `IFS`, `PYTHONSTARTUP`, `PYTHONPATH`, `NODE_OPTIONS`, `NODE_PATH`, `PERL5OPT`, `PERL5LIB`, `RUBYOPT`, `RUBYLIB`, `GEM_PATH`, `GEM_HOME`, `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `DOTNET_STARTUP_HOOKS`, `GOFLAGS`, `OP_SERVICE_ACCOUNT_TOKEN`, `OP_CONNECT_TOKEN`, `OP_CONNECT_HOST`, `OP_SESSION_*`. Dropping these here is unconditional: any ambient value is discarded before layer 3, so a value the supervisor overlays in layer 3 (e.g. `JAVA_TOOL_OPTIONS` under `network.proxy_injection: ["jvm"]`) is always the supervisor's own, never an attacker-supplied one.
2. If `environment.allow_vars` is non-empty, only variables matching an entry (exact name, or prefix match for entries ending in `*`) pass; sandbox-injected variables bypass this list.
3. Sandbox-injected variables (proxy variables per the sandbox-network spec, plus any `network.proxy_injection` env such as a supervisor-controlled `JAVA_TOOL_OPTIONS` or `NODE_USE_ENV_PROXY`) are then set and take precedence over inherited values. The layer-1 blocklist still strips the ambient variable first, so only the supervisor-authored value reaches the child.

#### Scenario: Dangerous variable stripped
- **WHEN** the supervisor environment contains `DYLD_INSERT_LIBRARIES` or `NODE_OPTIONS`
- **THEN** the child environment does not contain it, even if it matches an `allow_vars` entry

#### Scenario: Allowlist with prefix wildcard
- **WHEN** `allow_vars` is `["HOME", "PATH", "OMAC_*"]` and the supervisor env contains `HOME`, `PATH`, `OMAC_BASE`, and `AWS_SECRET_ACCESS_KEY`
- **THEN** the child sees `HOME`, `PATH`, and `OMAC_BASE` but not `AWS_SECRET_ACCESS_KEY`

#### Scenario: No allowlist configured
- **WHEN** `environment.allow_vars` is absent
- **THEN** all supervisor variables except blocklisted ones are passed through

### Requirement: Process lifecycle and exit propagation
The supervisor SHALL forward SIGINT, SIGTERM, and SIGHUP to the sandboxed child, SHALL exit with the child's exit code (or 128+signal if signal-killed), and SHALL tear down the proxy and temporary resources on exit. The child MUST NOT outlive the supervisor.

#### Scenario: Exit code propagation
- **WHEN** the inner command exits with code 3
- **THEN** `omac sandbox run` exits with code 3

#### Scenario: Ctrl-C
- **WHEN** the user sends SIGINT to `omac sandbox run`
- **THEN** the child receives SIGINT and after it terminates the supervisor cleans up and exits
