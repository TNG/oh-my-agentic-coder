package intent

// The Hint* strings are the authoritative remediation guidance surfaced to a
// sandboxed agent after a denial. They are shared by the facade's GET
// /sandbox/intent lookup and the netproxy deny path (body + response header)
// so every channel speaks with one voice and an agent never has to guess.
//
// Each value MUST stay single-line and ASCII-only: they are also emitted as an
// HTTP response header, whose field-value forbids CR/LF and discourages
// non-ASCII octets.
const (
	HintUndeclared  = "No intent on file for this target. If a request to it was denied by the sandbox or is waiting on user approval, POST $OMAC_BASE/sandbox/intent {\"target\":\"...\",\"reason\":\"...\"} and retry, so the user sees why you need it. If you already declared an intent earlier and the request was still denied, the user reviewed it and declined - do not retry."
	HintDeclared    = "An intent is on file for this target. If the request was still denied, the user reviewed your reason and declined it - do not retry; choose another approach or ask the user."
	HintExplainMore = "The user clicked \"Explain more\" in the approval dialog: your intent (if any) was not specific enough to decide. Do not retry without an intent explanation. POST a fuller, specific reason to $OMAC_BASE/sandbox/intent {\"target\":\"...\",\"reason\":\"...\"} stating exactly why you need this host, what you will fetch, and how you will use it, then retry so the user can reconsider."
)
