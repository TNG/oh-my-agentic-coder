# Tasks: port-ranges

## 1. Port entry parsing (`internal/sandboxprofile`)

- [ ] 1.1 Add `PortList` (defined over `[]int`) with `UnmarshalJSON` that accepts
  a JSON array of integers and `"N"`/`"A-B"` strings, expanding ranges at decode
  time
- [ ] 1.2 Add `ParsePortEntry` for `"N"` and `"A-B"`: trim surrounding spaces,
  inclusive range, both endpoints in 0–65535, `A <= B`; errors name the
  offending value and carry no `sandbox profile:` prefix, because the loader
  wraps decode errors as `parse sandbox profile: …` and a prefix here would
  surface doubled
- [ ] 1.3 Enforce `maxExpandedPorts = 1000` per field and per single entry, with
  an error naming the cap
- [ ] 1.4 Table-driven tests: ints only, mixed list, string single, the `0`
  sentinel, reversed range, out-of-bounds endpoint, over-cap range, `"1-2000"`
  whose error names the 1000 cap, leading zeros (`"0080-0085"` → `[80..85]`),
  empty string entry, float/bool/null/non-numeric entries, non-array JSON,
  surrounding spaces. Assert errors by the offending value they name; the
  profile-loading tests keep asserting the `parse sandbox profile:` chain

## 2. Profile fields

- [ ] 2.1 Change `Network.ListenPort` and `Network.AllowTCPConnect` to
  `PortList`; leave `Network.OpenPort` as `[]int`
- [ ] 2.2 Update the field doc comments to document both entry forms
- [ ] 2.3 Tests: the mixed list loads with the expanded length; `"0-100"` still
  fails `Validate`'s ≥1 floor; existing integer rejection cases still fail

## 3. CLI flags

- [ ] 3.1 `--listen-port` and `--allow-tcp-connect` accept `N` or `A-B`, in
  inline `=` and separate-value form, repeatable, merged additively, with the
  merged per-field list rejected above 1000 ports
- [ ] 3.2 Keep `--open-port` single-port
- [ ] 3.3 Update the `omac sandbox run` usage and help text to `<port|A-B>`
- [ ] 3.4 Tests: a range on a flag, two flags summing their entries, an invalid
  range error naming the flag and value, a profile-plus-flag merge over the
  1000-port cap rejected, a single port unchanged

## 4. Documentation

- [ ] 4.1 `docs/configuration.md`: "Granting port ranges" with a mixed-list
  example, the 1000-port cap, the flag form, and the Linux per-port note
- [ ] 4.2 `docs/security.md`: mention the range syntax in the two port rows

## 5. Verification and PR

- [ ] 5.1 `go build ./... && go vet ./internal/sandboxprofile/`
- [ ] 5.2 `go test ./internal/sandboxprofile/ ./internal/sandboxrun/ ./internal/cli/`
  as the fast inner loop while iterating
- [ ] 5.3 `go test ./...` as the final gate before the PR: a `PortList` retypes a
  widely constructed struct, so only the full suite catches downstream breakage
- [ ] 5.4 Exercise the surface: with `--listen-port 8000-8100`, a bind on 8050
  succeeds and a bind on 9000 is denied
- [ ] 5.5 Open the PR following `.github/pull_request_template.md`, titled
  `feat(sandbox): port ranges for listen_port and allow_tcp_connect`, with
  `Closes #<issue>`, the verification commands and their output, the
  `🤖 Generated with ...` agent footer, and one reviewer
