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

### Choosing the model for LLM-driven runs

Every workflow that calls a model — `E2E: full`, `E2E: drift`,
`E2E: onboarding`, `Doc drift`, `Security Scan`, and the release-notes
summarizer — resolves its model id the same way, so a run can be pointed at a
different model from the Actions **Run workflow** form without editing or
pushing anything. Precedence, highest first:

| Source | Scope |
|--------|-------|
| `E2E_MODEL_<HARNESS>` (`claude_code_model` input) | one harness |
| `E2E_MODEL` (`model` input) | every harness in the run |
| `modelIDs` in `internal/e2e/versions.go` | the committed pin (what scheduled runs use) |

The two implementations of that precedence — `modelID()` in
`internal/e2e/versions.go` for the Go tests and
[`scripts/resolve-model.sh`](../scripts/resolve-model.sh) for the workflows'
shell steps — are kept in parity by `scripts/resolve-model_test.sh`, which runs
on every PR. Locally the same env vars work:

```bash
E2E_MODEL=vendor/some-model go test -tags=e2e -v ./internal/e2e/
scripts/resolve-model.sh claude-code   # print what a harness would resolve to
```

Two constraints the resolver enforces up front, rather than letting them surface
as an opaque harness failure ten minutes into a run:

- **claude-code launches sonnet or haiku models only.** A cross-harness
  `model` override therefore can't reach it; give it a `claude_code_model` of
  its own (it bills a different provider than the SKAINET gateway anyway).
- **The declared context window must not exceed the model's real one**, or the
  run overflows mid-turn. It defaults to 100000 — safely under every pinned
  model, and a smaller declared window only makes the harness compact earlier.
  Raise it via the `context_limit` input / `E2E_CONTEXT_LIMIT` to exercise a
  larger model's full window.

Each run reports the model it resolved: the run's step summary and an
Actions notice, `meta.txt` in the uploaded e2e artifacts, the `model=` field on
every `OMAC_COMPAT` line, the `Model` column of the compatibility matrix, and
the header of the drift/onboarding report artifacts.

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
