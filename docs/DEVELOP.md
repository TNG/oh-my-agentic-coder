# Developing omac

## Layout

```
cmd/omac/                  Entrypoint.
internal/cli/              Subcommand dispatch (register/deregister/list/
                           secrets/start/doctor/version).
internal/config/           omac.yaml + oh-my-agentic-coder.yaml types.
internal/registry/         .opencode/sidecar.json (atomic writes, flock).
internal/keychain/         Thin wrapper over github.com/zalando/go-keyring.
internal/secrets/          Secret type (redacted Stringer, zeroize) + masked prompt.
internal/osinfo/           macos / linux / wsl detection.
internal/facade/           Unix-socket HTTP reverse proxy (SSE + upgrades).
internal/supervisor/       Sidecar lifecycle (spawn, health, shutdown).
internal/sandbox/          Templated sandbox-runtime launcher.
```

## Build

```bash
# Plain dev build (version reports as the default "0.1.0-dev").
go build -o omac ./cmd/omac
```

### Release-style local build

Reproduce the release binary for your current platform — stripped
(`-s -w`), reproducible (`-trimpath`), with the version stamped in (the
same ldflags GoReleaser uses; see `.goreleaser.yaml`):

```bash
go build -trimpath -ldflags "-s -w -X main.Version=0.1.0-local" -o omac ./cmd/omac
./omac version   # -> omac 0.1.0-local   (note: `version` subcommand, not --version)
```

For the full multi-platform release artifacts (archives, `.deb`,
`.pkg.tar.zst`, checksums) build with GoReleaser, no tag or publish:

```bash
brew install goreleaser
goreleaser release --clean --snapshot --skip=publish   # output in dist/
# current platform only:
goreleaser build --clean --snapshot --single-target
```

## Test

```bash
# Unit + integration tests for every package.
go test ./...

# Formatting and static checks (run both before committing).
gofmt -l .        # prints nothing when clean
go vet ./...
```

Some facade and serve tests open a loopback TCP port (and/or a Unix
socket) and skip automatically in environments where `connect(2)` to
`127.0.0.1` or to a Unix socket is disallowed (e.g. a hardened sandbox).
On a normal dev machine they all run.

### Multi-directory serve mode (`omac serve`)

End-to-end smoke test of the control plane, facade routing, per-workdir
isolation, and a real skill round trip (requires loopback; needs `curl`
and `python3`):

```bash
bash scripts/serve_smoke.sh        # expect "PASS=15  FAIL=0 / ALL GREEN"
```

The OpenCode-side plugin (`.opencode/plugins/omac-multidir.ts`)
typechecks against the published plugin types:

```bash
cd .opencode
npx -p typescript tsc --noEmit --strict --moduleResolution bundler \
  --module esnext --target es2022 --lib es2022,dom --skipLibCheck \
  plugins/omac-multidir.ts
```

To try it with a real OpenCode server, see
[`docs/MULTI_DIR_DESKTOP.md`](MULTI_DIR_DESKTOP.md):

```bash
# Wrap `opencode serve`; --root pre-declares the allowed project roots.
# The positional harness token (opencode|claude) goes right after `serve`.
omac serve opencode --no-sandbox --root "$HOME/code" --verbose -- --port 4096 --print-logs
# Note the logged "control plane on http://127.0.0.1:<CTRL>", then open a
# project under the root in OpenCode Desktop and confirm activation:
#   curl -s http://127.0.0.1:<CTRL>/__omac__/dirs | python3 -m json.tool
```

Under the Claude Code harness, `omac serve claude` / `omac start claude` run
the `claude` CLI instead; the `.claude/` hooks bridge it to the same control
plane. Claude Code has no `opencode serve`-style daemon convention, so it runs
as-is (no subcommand is injected). See `MULTI_DIR_DESKTOP.md` for the
per-harness `serve` notes and limitations.
