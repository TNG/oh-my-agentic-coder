package buildbroker

// Bounds and constants for the host build broker v1 protocol. These are
// local control-plane DoS bounds, not build-policy limits: they bound
// allocations and decode work the control plane does per request, not
// anything the build engine enforces against the build itself.
const (
	// MaxExecuteBodyBytes is the upper bound on a single execute request
	// body. The execute handler wraps the request body in an
	// http.MaxBytesReader at this limit before decoding. A Gradle arg
	// list is short; 1 MiB is generous for a frame containing only the
	// canonical worktree and the raw args, while bounding decode work.
	MaxExecuteBodyBytes int64 = 1 << 20 // 1 MiB

	// MaxCancelBodyBytes is the upper bound on a single cancel request
	// body. A cancel frame is one short JSON object; 4 KiB is ample
	// while bounding decode work.
	MaxCancelBodyBytes int64 = 4 << 10 // 4 KiB

	// MaxArgs is the maximum number of raw arguments accepted in an
	// execute frame. Beyond this the request is rejected as a policy
	// denial before any build code runs. Gradle arg lists are short;
	// 4096 is a generous ceiling that still bounds the slice the engine
	// reparses.
	MaxArgs = 4096

	// MaxOutputFrameBytes is the maximum number of RAW bytes each
	// output frame carries before base64 encoding. The broker chunks
	// larger writes into multiple frames so a single write never blocks
	// framing on a huge buffer. 32 KiB keeps a frame + its base64
	// expansion well under common buffer sizes.
	MaxOutputFrameBytes = 32 << 10 // 32 KiB

	// MaxActiveRequests bounds the in-memory active-request registry
	// (requests that have been accepted and not yet completed). A new
	// request that would exceed this is rejected as a policy denial
	// (503-shaped on the wire) before any build code runs. This is a
	// control-plane DoS bound, not a concurrency limit the engine
	// enforces.
	MaxActiveRequests = 256

	// MaxTombstones bounds the completed-ID tombstone map. A tombstone
	// records a recently-completed request so a late cancel returns 410
	// instead of 404. The map is bounded; once full, the oldest
	// tombstone is evicted.
	MaxTombstones = 256

	// TombstoneTTL is how long a completed request ID stays in the
	// tombstone map before eviction. Bounded so the map cannot grow
	// unbounded across a long-lived parent.
	TombstoneTTL = 60 // seconds

	// ForceDeadline is the bounded interval the broker waits between
	// graceful cancellation and forced cancellation during parent
	// shutdown and per-request disconnect. After this interval the
	// broker closes the force signal regardless of whether the engine
	// has finished cleanup; the engine's own forced-cancel path then
	// runs to completion before the broker returns.
	ForceDeadlineSeconds = 10
)

// Endpoint paths. The /v1 path fixes the protocol version; frames do
// not repeat a version and there are no sequence numbers (no
// acknowledgement, retransmission, or resume behavior).
const (
	// ExecutePath is the execute route. The parent registers it on its
	// loopback control listener.
	ExecutePath = "/__omac__/build/v1"

	// CancelPathPrefix is the prefix of the cancel route; the request
	// ID is appended. The handler parses the trailing segment as the
	// request ID.
	CancelPathPrefix = "/__omac__/build/v1/"

	// CancelRouteSuffix is the literal suffix of the cancel route after
	// the request ID.
	CancelRouteSuffix = "/cancel"

	// ContentTypeJSON is the required content type for both endpoints.
	ContentTypeJSON = "application/json"

	// AcceptNDJSON is the accept header the execute client sends; the
	// broker checks it on the execute route only (the cancel route has
	// no response body).
	AcceptNDJSON = "application/x-ndjson"
)
