# Port ranges for listen_port and allow_tcp_connect

## Why

A profile grants ports one integer at a time. Processes that reserve a
*contiguous* block of ports therefore need one entry per port: a dev server that
scans 8000–8100, a Hadoop/Spark-style worker that draws its ephemeral ports from
a configured pool, a test runner that allocates ports at startup. Writing 101
integers by hand is tedious and easy to get wrong, and a missing port surfaces
much later as a bind failure inside the sandbox.

The two fields that need this are `network.listen_port` and
`network.allow_tcp_connect`. Both are `[]int` today
(`internal/sandboxprofile/profile.go:150` and
`internal/sandboxprofile/profile.go:153`), and the matching CLI flags accept a
single port.

## What Changes

- Both fields accept a mixed list of integers and strings, where a string is
  either `"N"` or `"A-B"` (for example `"8000-8100"`). Ranges are inclusive.
- String entries expand to individual ports when the profile is loaded, so the
  in-memory value is the same plain port list as today. Nothing downstream
  changes: grants normalization, the macOS bind grant, and the Linux per-port
  Landlock rules keep consuming a list of integers.
- The flags `--listen-port` and `--allow-tcp-connect` accept `N` or `A-B`,
  repeatable as today.
- Expansion is bounded: one field may expand to at most 1000 ports. A reversed
  range, an endpoint outside 0–65535, and an over-cap range are load-time
  errors naming the offending value.
- `network.open_port` and `--open-port` stay as they are (see Out of Scope).
- Profiles containing only integers keep parsing and validating exactly as
  before.

## Decisions

- **Expand at load time, not at enforcement time.** Every consumer of these
  fields already works on a list of integers. Expanding once, where the profile
  is decoded, keeps that surface unchanged and makes a range pure shorthand.
  Profiles are user-edited files that omac never rewrites
  (`docs/configuration.md:88`), so the expanded list is never written back and
  the readable `"8000-8100"` stays in the user's file.
- **Cap the expanded list at 1000 ports per field.** The expanded list reaches
  the argv of the Linux stage-2 helper and becomes one Landlock rule per port;
  the cap bounds both. An over-cap profile is rejected instead of being clamped,
  so a grant never quietly shrinks.
- **Reject at parse time rather than at enforcement time.** An older omac given
  a range-string profile fails loudly with a parse error naming the entry,
  matching the existing "an unknown field is an error" stance. Silently dropping
  the grant would leave the user with a sandbox that looks configured and is
  not.

## Capabilities

### New Capabilities

<!-- none -->

### Modified Capabilities

- `sandbox-profile`: the two port-grant fields and their CLI flags accept range
  strings; the expansion, cap, and error rules join the profile-format
  requirement.

`openspec/specs/` holds no `sandbox-profile` baseline yet: the capability's
requirements currently live in the active change
`openspec/changes/native-sandbox/specs/sandbox-profile/spec.md`. The delta is
written as `MODIFIED` against that text, so `native-sandbox` must be archived
before this change is archived.

## Impact

- **`internal/sandboxprofile/`**: a new `PortList` type defined over `[]int`
  that decodes the mixed list, plus `ParsePortEntry` for the `"N"`/`"A-B"`
  syntax; the two `Network` fields change type; the two flag cases parse ranges.
  Because `PortList` is a `[]int` type, helpers such as `dedupeInts` and
  `appendIntCopy` keep working unchanged.
- **Kernel enforcement**: unchanged. A range is only a shorthand for a longer
  per-port list; the sandbox still allows exactly the ports it would allow if
  the user had written them out.
- **Docs**: `docs/configuration.md` gains a "Granting port ranges" subsection
  next to "Opening a port"; `docs/security.md` mentions the range syntax in the
  two port rows.
- **Compatibility**: int-only profiles are unaffected. A range-string profile is
  rejected by an older omac at parse time.

## Out of Scope / Future Work

- **`network.open_port` ranges, and `--open-port` ranges.** On Linux a granted
  port also allows outbound connections to *any* host on that port, because
  Landlock cannot scope a port to localhost. That makes `open_port` the
  security-sensitive field. `omac doctor` and `omac provenance --check` report
  every numeric `open_port`, and `omac diagnose` reasons about one numeric entry
  per probe. Accepting a range there needs its own round for those warning and
  diagnostic surfaces. Follow-up issue.
- Removing the ≥1 floor for these two fields or the 65535 bound. A range that
  expands to include 0 still fails validation for `listen_port` and
  `allow_tcp_connect`.
- Named port sets, port aliases, or include-style profile fragments.
