# Tasks: native-sandbox

## 1. Profile and CLI foundation

- [x] 1.1 Implement profile schema types + strict JSON parsing (reject unknown fields) in `internal/sandboxprofile`
- [x] 1.2 Implement `~`/`$VAR` path expansion and skip-with-notice for nonexistent paths
- [x] 1.3 Implement profile resolution (path → `~/.config/omac/profiles/<name>.json` → compiled-in), with compiled-in `default` profile mirroring `tng-sandbox.json`
- [x] 1.3b Define the built-in protected-path deny set (credentials, keychains, browser data, shell history/configs, macOS private data) and `filesystem.override_deny` hole-punching; define the platform read/temp-write baselines
- [x] 1.3c Add `filesystem.deny` / `--deny` user deny list: explicit-path and basename-glob forms, glob walk over granted trees + workdir (baseline trees excluded, bounded), resolving into the protected-path masks
- [x] 1.4 Add `omac sandbox run` subcommand: flag parsing (`--allow`, `--read`, `--write`, `--deny`, `--allow-file`, `--open-port`, `--listen-port`, `--allow-tcp-connect`, `--allow-domain`, `--deny-domain`, `--block-net`, `--workdir-access`, `--profile`, `--` separator) and additive merge onto the profile
- [x] 1.5 Unit tests: parsing, unknown-field rejection, resolution order, flag merging, `--block-net` override, `filesystem.deny` resolution + masking

## 2. Filtering proxy (`internal/netproxy`)

- [x] 2.1 Proxy server: ephemeral loopback listener, session token generation, constant-time token validation (407 on missing/wrong), CONNECT tunnel + absolute-URI HTTP forwarding, no TLS termination, SSE/streaming pass-through
- [x] 2.2 Host filter: hard-deny metadata hostnames and link-local resolved IPs (incl. IPv4-mapped IPv6), DNS pinning (connect to resolved IPs), loopback CONNECT refusal
- [x] 2.3 Decision pipeline: learned deny → `deny_domain` → `allow_domain`/learned allow → default (prompt / allowlist-deny / blocklist-allow); case-insensitive exact and `*.suffix` matching
- [x] 2.4 403 deny responses naming the blocked host; structured log line per decision
- [x] 2.5 Unit tests: filter order, wildcard matching, blocklist-only mode, allowlist-without-prompt mode, learned-deny-overrides-allowlist, rebinding pin, token auth

## 3. Interactive prompt and learned policy

- [x] 3.1 Learned-policy store: nono-compatible JSON format, atomic write (temp+rename), load at startup from `~/.config/omac/learned/<profile>.json`
- [x] 3.2 Dialog backends: osascript (macOS), zenity/kdialog (Linux), with exact prompt text and six options, default `Deny once`, cancel = deny once; parallel OS notification
- [x] 3.3 Suffix hint computation (strip leftmost label when ≥3 labels; IP literals as-is)
- [x] 3.4 Timeout handling (kill dialog at deadline), `on_unavailable` fallback, headless detection, per-host prompt coalescing
- [x] 3.5 Unit tests: decision-token parsing, suffix hint, coalescing, once-vs-permanent persistence, timeout default-deny

## 4. macOS backend (Seatbelt)

- [x] 4.1 SBPL profile generator: `(deny default)` baseline, process exec/fork, process-info/signal scoping to self/same-sandbox, system read + temp-write baseline, fs rules for literal+canonical paths, protected-path denies between read- and write-allows, `file-map-executable` limited to readable paths, ancestor `file-read-metadata`, Keychain mach-lookup denies
- [x] 4.2 Network SBPL rules: `(deny network*)` + proxy-port allow, `allow_tcp_connect` (`*:port`), `open_port` localhost allows, blanket bind when `listen_port`/`open_port` non-empty, explicit Unix-socket allows for granted socket files, mDNSResponder carve-out
- [x] 4.3 Child launch via `sandbox-exec -p` with constructed env (blocklist → allowlist → injected proxy vars)
- [x] 4.4 Integration tests (macOS): fs deny/allow/readonly, protected-path deny under broad grant + override_deny hole, keychain deny, direct-connect deny, proxy reachability, Unix socket connect under network deny, `/tmp` canonicalization, `$TMPDIR` writable

## 5. Linux backend (bubblewrap + Landlock)

- [x] 5.1 bwrap argv builder: ro-bind/bind per grants, system baseline, protected-path masking inside granted trees (tmpfs / /dev/null overlays) honoring `override_deny`, `--proc`/`--dev`/`--tmpfs /tmp`, `--unshare-pid/ipc/uts`, `--die-with-parent`, `--new-session`, no `--unshare-net`; bwrap presence check with actionable error
- [x] 5.2 `omac sandbox stage2` hidden subcommand: apply Landlock net rules (`connect_tcp`: proxy port + `allow_tcp_connect` + `open_port`; `bind_tcp`: `listen_port` + `open_port`) via raw syscalls, then exec inner command
- [x] 5.3 Landlock ABI detection: fail closed below ABI 4 with clear error; implement `enforcement: env-only` escape hatch with prominent warning
- [x] 5.4 Integration tests (Linux): unbound path absent, ro-bind write denial, direct-connect deny, allowed bind/connect ports, proxy reachability, stage2 exec

## 6. Supervisor lifecycle

- [ ] 6.1 Wire `omac sandbox run`: start proxy, build child per platform, inject env, forward SIGINT/SIGTERM/SIGHUP, propagate exit code (128+signal), teardown on exit
- [ ] 6.2 `blocked`/`open` network modes (no proxy; full deny vs no restriction) and `--block-net` warning
- [ ] 6.3 TTY handling parity with current `internal/sandbox/launcher.go` (interactive harness must keep working)

## 7. Launcher integration

- [x] 7.1 Add `builtin` launch profile with `{{self}}` placeholder to `internal/config/launcher.go`; keep `nono` and `no-sandbox-debug` templates
- [x] 7.2 Make `builtin` the default; ensure `OMAC_*` env propagation works through `omac sandbox run`
- [x] 7.3 Extend `omac doctor`: bwrap presence, Landlock ABI, `sandbox-exec` presence, dialog backend availability
- [ ] 7.4 End-to-end test: `omac start opencode` under built-in sandbox — bridge Unix socket and TCP transports, SSE streaming, skill round-trip (echo-rest), workdir writes, denied home reads

## 9. Profiles dir, desktop folders, learn mode, formatting

- [x] 9.1 Move profile resolution to ~/.config/omac/sandbox-profiles/; first-start scaffolding of pretty-printed default.json; drop compiled-in fallback once the file exists
- [x] 9.2 Move learned page policy to sibling <name>.pages.json (pretty-printed); migrate path resolution in run.go
- [x] 9.3 `omac serve --for-opencode-desktop`: read OpenCode project worktrees (storage JSON, opencode.db fallback), grant existing worktrees rw via --allow flags, skip "/", log the grant list
- [x] 9.4 `--learn` mode for start+serve: unrestricted fs sandbox variant, fd-sampling recorder, aggregation (dedupe/collapse, exclude granted+baseline+protected), end-of-session y/n prompt, pretty-printed profile rewrite
- [x] 9.5 Pretty-print all JSON omac writes; timestamped/aligned log format for sandbox.log and diagnostics
- [x] 9.6 Tests: scaffolding, pages file round-trip, desktop worktree extraction, learn aggregation/exclusions, profile rewrite, log format

## 8. Documentation and finalization

- [ ] 8.1 Document the sandbox in README.md (profile schema, CLI, platform limitations: macOS coarse `listen_port`, host-unconstrained `allow_tcp_connect`, Landlock v4 floor)
- [ ] 8.2 Document migration from nono (translate the user's *installed* profile incl. accumulated filesystem grants and `policy.override_deny`; copy learned-policy files from nono's locations; note dropped credential injection) and rollback (select `nono` launch profile)
- [ ] 8.2b Parity checklist run: side-by-side `nono run` vs `omac sandbox run` with the tng-sandbox profile — verify identical outcomes for protected-path reads, temp-dir writes, keychain access, SSH (port 22), inner-harness listen on 4097, prompt flow, and learned-policy reuse
- [ ] 8.3 `openspec validate native-sandbox --strict`
