## ADDED Requirements

### Requirement: Continue subcommand

omac SHALL provide an `omac continue [harness] [flags] [-- inner args...]` subcommand that re-launches the most recent session for the current workdir inside the omac sandbox. It SHALL reuse the full `omac start` launch pipeline (registry reconciliation, sidecars, facade, sandbox) and append the resolved harness's "continue last session" inner flag to the inner command.

#### Scenario: Continue the last session in the default harness

- **WHEN** the user runs `omac continue` in a workdir that has at least one prior session
- **THEN** omac SHALL run the same launch pipeline as `omac start opencode` and SHALL append `--continue` to the opencode inner command so the harness reopens the last session for that workdir

#### Scenario: Explicit harness token

- **WHEN** the user runs `omac continue claude`
- **THEN** omac SHALL launch the `claude` harness with that harness's continue flag (`--continue`) appended

#### Scenario: Continue accepts the same flags as start

- **WHEN** the user runs `omac continue --no-sandbox --verbose`
- **THEN** omac SHALL honor those start flags identically to `omac start` and still append the continue inner flag

#### Scenario: Extra inner args are preserved

- **WHEN** the user runs `omac continue -- --model anthropic/claude-opus`
- **THEN** omac SHALL pass `--continue` together with the user-supplied inner args verbatim to the inner harness command
