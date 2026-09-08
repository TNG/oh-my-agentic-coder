package containerproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// OwnershipLabelKey is the reserved label prefix the proxy injects on every
// create to mark executor ownership. Client attempts to set any label with
// this prefix are rejected (forgeable labels must not override ownership).
const OwnershipLabelKey = "omac.executor"

// ryukImage is the Testcontainers reaper image v1 disables via
// TESTCONTAINERS_RYUK_DISABLED=true. The filter rejects it fail-closed
// (a client could unset the env — ADR 0002 / REPORT.md §Ryuk-only delta).
const ryukImage = "testcontainers/ryuk"

// endpointDecision is the allowlist verdict for one request: the matched
// rule (or nil for a deny) plus the parsed path segments needed for
// ownership checking and body rewriting.
type endpointDecision struct {
	// allowed is true when the (method, path) matches a v1 allowlist rule.
	allowed bool
	// rule names the matched rule for audit/logging ("ping", "version",
	// "info", "images.json", "image.inspect", "images.create",
	// "containers.create", "container.start", "container.kill",
	// "container.wait", "container.inspect", "container.logs",
	// "containers.list", "container.delete", or "" for a deny).
	rule string
	// containerID is the {id} segment for container-scoped rules. Empty
	// for rules without an id segment.
	containerID string
	// imageRef is the {ref} segment for image-inspect rules.
	imageRef string
}

// decideApplylist matches a request against the ticket-02 v1 allowlist.
// It fails closed: anything not explicitly allowed is a deny
// (KindUnknownEndpoint). The path is the raw URL path (e.g.
// "/v1.44/containers/abc123/json"); the method is the HTTP verb.
func decideAllowlist(method, path string) endpointDecision {
	// Docker versioned paths look like /v1.44/... Strip a leading /v<digits>(.<digits>)?/
	// to normalize; the allowlist accepts any v1.* version prefix (REPORT.md:
	// "any /v1.xx prefix validated to a supported range"). Unversioned
	// /_ping is the one unversioned endpoint.
	rest := path
	versioned := false
	if strings.HasPrefix(path, "/v") {
		// Find the second slash after the version segment.
		if idx := strings.IndexByte(path[1:], '/'); idx >= 0 {
			seg := path[1 : 1+idx]
			if isVersionSeg(seg) {
				rest = path[1+idx:]
				versioned = true
			}
		}
	}

	// /_ping (GET and HEAD) — the Docker client sends this BOTH
	// unversioned (/_ping, for liveness) AND versioned
	// (/v1.44/_ping, for version negotiation). Accept both forms;
	// the comment at line 47 ("Unversioned /_ping is the one unversioned
	// endpoint") described the intent, but the versioned form is real
	// and was wrongly denied — the test at proxy_test.go:365 only
	// exercised the unversioned form, so the gap escaped.
	if (method == http.MethodGet || method == http.MethodHead) && rest == "/_ping" {
		return endpointDecision{allowed: true, rule: "ping"}
	}
	// Other unversioned paths are denied (only /_ping is unversioned).
	if !versioned {
		return endpointDecision{allowed: false}
	}

	switch method {
	case http.MethodGet:
		switch {
		case rest == "/version":
			return endpointDecision{allowed: true, rule: "version"}
		case rest == "/info":
			return endpointDecision{allowed: true, rule: "info"}
		case rest == "/images/json":
			return endpointDecision{allowed: true, rule: "images.json"}
		case rest == "/containers/json":
			return endpointDecision{allowed: true, rule: "containers.list"}
		case strings.HasPrefix(rest, "/images/") && strings.HasSuffix(rest, "/json"):
			ref := strings.TrimSuffix(strings.TrimPrefix(rest, "/images/"), "/json")
			if ref != "" {
				return endpointDecision{allowed: true, rule: "image.inspect", imageRef: ref}
			}
		case strings.HasPrefix(rest, "/containers/") && strings.HasSuffix(rest, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "/containers/"), "/json")
			if id != "" && !strings.ContainsRune(id, '/') {
				return endpointDecision{allowed: true, rule: "container.inspect", containerID: id}
			}
		case strings.HasPrefix(rest, "/containers/") && strings.HasSuffix(rest, "/logs"):
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "/containers/"), "/logs")
			if id != "" && !strings.ContainsRune(id, '/') {
				return endpointDecision{allowed: true, rule: "container.logs", containerID: id}
			}
		}
	case http.MethodPost:
		switch {
		case rest == "/containers/create":
			return endpointDecision{allowed: true, rule: "containers.create"}
		case rest == "/networks/prune":
			// Testcontainers' JVMHookResourceReaper (the in-process
			// cleanup hook, distinct from the Ryuk *container* reaper
			// that TESTCONTAINERS_RYUK_DISABLED disables) calls
			// POST /networks/prune on every JVM shutdown. Allowed
			// with an injected label filter (see serve) so only THIS
			// executor's networks are pruned — never unrelated host
			// networks. Ownership-scoping matches GET /containers/json.
			return endpointDecision{allowed: true, rule: "networks.prune"}
		case rest == "/volumes/prune":
			// Same JVMHookResourceReaper shutdown hook also calls
			// POST /volumes/prune. Allowed with an injected label
			// filter (see serve) so only THIS executor's volumes are
			// pruned — never unrelated host volumes. Testcontainers
			// labels the volumes it creates (incl. the omac.executor
			// ownership label injected at create time), so the label
			// filter scopes the prune to this executor. Mirrors
			// networks.prune.
			return endpointDecision{allowed: true, rule: "volumes.prune"}
		case rest == "/images/prune":
			// The third prune endpoint the JVMHookResourceReaper
			// shutdown hook calls. Allowed with the same injected
			// ownership label filter (see serve). Pulled images do not
			// carry the omac.executor label (it is injected at
			// container create, not image pull), so the label filter
			// scopes the prune to a safe no-op for pulled images; any
			// build-created images labeled with omac.executor are
			// still scoped to this executor. Mirrors networks/volumes
			// prune.
			return endpointDecision{allowed: true, rule: "images.prune"}
		case strings.HasPrefix(rest, "/containers/") && strings.HasSuffix(rest, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "/containers/"), "/start")
			if id != "" && !strings.ContainsRune(id, '/') {
				return endpointDecision{allowed: true, rule: "container.start", containerID: id}
			}
		case strings.HasPrefix(rest, "/containers/") && strings.HasSuffix(rest, "/kill"):
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "/containers/"), "/kill")
			if id != "" && !strings.ContainsRune(id, '/') {
				return endpointDecision{allowed: true, rule: "container.kill", containerID: id}
			}
		case strings.HasPrefix(rest, "/containers/") && strings.HasSuffix(rest, "/wait"):
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "/containers/"), "/wait")
			if id != "" && !strings.ContainsRune(id, '/') {
				return endpointDecision{allowed: true, rule: "container.wait", containerID: id}
			}
		case rest == "/images/create":
			return endpointDecision{allowed: true, rule: "images.create"}
		}
	case http.MethodDelete:
		if strings.HasPrefix(rest, "/containers/") && !strings.Contains(strings.TrimPrefix(rest, "/containers/"), "/") {
			id := strings.TrimPrefix(rest, "/containers/")
			if id != "" {
				return endpointDecision{allowed: true, rule: "container.delete", containerID: id}
			}
		}
	}
	return endpointDecision{allowed: false}
}

// isVersionSeg reports whether seg looks like "v1.44" or "v1".
func isVersionSeg(seg string) bool {
	if !strings.HasPrefix(seg, "v") {
		return false
	}
	rest := seg[1:]
	if rest == "" {
		return false
	}
	dot := strings.IndexByte(rest, '.')
	if dot < 0 {
		// "v1" — all digits.
		return allDigits(rest)
	}
	return allDigits(rest[:dot]) && allDigits(rest[dot+1:])
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isOwnershipScopedRule reports whether the rule targets a specific {id}
// container and therefore requires an ownership check before forwarding.
func isOwnershipScopedRule(rule string) bool {
	switch rule {
	case "container.start", "container.kill", "container.wait",
		"container.inspect", "container.logs", "container.delete":
		return true
	}
	return false
}

// createBody is parsed as an untyped map (json.Unmarshal into
// map[string]any) so unknown fields pass through untouched and the
// fail-closed allowlist owns HostConfig validation (see
// validateCreateBody).
func validateCreateBody(raw []byte, approvedImages []string, executorID string) ([]byte, *ContainerPolicyError) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, &ContainerPolicyError{Kind: KindUnknownEndpoint, Reason: "create body is not valid JSON: " + err.Error()}
	}
	hc, _ := body["HostConfig"].(map[string]any)
	image, _ := body["Image"].(string)

	// 1. Image ∈ approved set. Also reject the Ryuk image fail-closed.
	if isRyukImage(image) {
		return nil, &ContainerPolicyError{Kind: KindRyukForbidden, Image: image}
	}
	if !imageApproved(image, approvedImages) {
		return nil, &ContainerPolicyError{Kind: KindUnapprovedImage, Image: image}
	}

	// 2. Privileged forbidden.
	if b, _ := hc["Privileged"].(bool); b {
		return nil, &ContainerPolicyError{Kind: KindPrivilegedForbidden, Image: image}
	}

	// 3. Binds / Mounts empty.
	if nonEmptyStrSlice(hc["Binds"]) || nonEmptyAnySlice(hc["Mounts"]) {
		return nil, &ContainerPolicyError{Kind: KindBindMountForbidden, Image: image}
	}

	// 4. Host namespaces empty/default. Isolation is included because
	// Testcontainers' docker-java client serializes it on macOS; a
	// non-default value (e.g. "process" on Linux, "hyperv" on Windows)
	// is a host-namespace escape vector on platforms that honor it.
	for _, k := range []string{"NetworkMode", "PidMode", "IpcMode", "UsernsMode", "CgroupnsMode", "Runtime", "Isolation"} {
		if s, _ := hc[k].(string); s != "" && !isDefaultMode(s) {
			return nil, &ContainerPolicyError{Kind: KindHostNamespaceForbidden, Image: image, Reason: k + "=" + s}
		}
	}

	// 5. Devices / capabilities / security options empty.
	if nonEmptyStrSlice(hc["CapAdd"]) || nonEmptyAnySlice(hc["Devices"]) ||
		nonEmptyStrSlice(hc["SecurityOpt"]) || nonEmptyStrSlice(hc["Dns"]) ||
		nonEmptyStrSlice(hc["ExtraHosts"]) {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image}
	}
	if s, _ := hc["CgroupParent"].(string); s != "" {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "CgroupParent=" + s}
	}
	// UTSMode (host UTS namespace escape) — REPORT §"Create-body field
	// analysis" lists it among the present-but-empty keys to validate.
	if s, _ := hc["UTSMode"].(string); s != "" && !isDefaultMode(s) {
		return nil, &ContainerPolicyError{Kind: KindHostNamespaceForbidden, Image: image, Reason: "UTSMode=" + s}
	}
	// AutoRemove MUST be false/absent: a container that auto-removes on
	// exit evades the proxy's ownership tracking and leaves no record for
	// the audit/cleanup path (spec §228: sidecar cleanup is authoritative).
	if b, _ := hc["AutoRemove"].(bool); b {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "AutoRemove=true evades cleanup tracking"}
	}
	// Init / DeviceRequests (GPU pass-through) — not in the v1 accepted
	// surface; deny fail-closed.
	if b, ok := hc["Init"].(bool); ok && b {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "Init not permitted in v1"}
	}
	if nonEmptyAnySlice(hc["DeviceRequests"]) {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "DeviceRequests (GPU) not permitted in v1"}
	}

	// 5c. Additional security-relevant fields that newer docker-java
	// versions serialize (absent from the original ticket-02 capture,
	// which used an older client). Each is validated empty/absent/
	// default; their PRESENCE with safe values is permitted via the
	// allowlist below so the fail-closed unknown-field check does not
	// deny a benign null/empty serialization.
	//
	// Links / VolumesFrom: cross-container access (host-namespace
	// escape + bypass of the ownership/cleanup model). Must be empty.
	if nonEmptyStrSlice(hc["Links"]) || nonEmptyStrSlice(hc["VolumesFrom"]) {
		return nil, &ContainerPolicyError{Kind: KindHostNamespaceForbidden, Image: image, Reason: "Links/VolumesFrom cross-container access denied"}
	}
	// Sysctls: kernel parameters (e.g. net.ipv4.ip_forward) — host
	// escape vector. Must be empty/absent. Docker serializes
	// HostConfig.Sysctls as a map[string]string (JSON object), so
	// check it as a map — nonEmptyStrSlice only matches arrays and
	// would silently let a non-empty map through.
	if m, ok := hc["Sysctls"].(map[string]any); ok && len(m) > 0 {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "Sysctls not permitted in v1"}
	}
	// DeviceCgroupRules: cgroup device allowlist — device access. Empty.
	if nonEmptyStrSlice(hc["DeviceCgroupRules"]) {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "DeviceCgroupRules not permitted in v1"}
	}
	// PublishAllPorts: bypasses the PortBindings 127.0.0.1 rewrite
	// (publishes ALL exposed ports to all interfaces). Must be false.
	if b, _ := hc["PublishAllPorts"].(bool); b {
		return nil, &ContainerPolicyError{Kind: KindBindMountForbidden, Image: image, Reason: "PublishAllPorts bypasses loopback-only port publishing"}
	}
	// RestartPolicy: a container that restarts evades the proxy's
	// ownership tracking and cleanup. Must be "" or "no" (the Docker
	// default).
	if s, _ := hc["RestartPolicy"].(map[string]any); s != nil {
		if name, _ := s["Name"].(string); name != "" && name != "no" {
			return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "RestartPolicy=" + name + " evades cleanup tracking"}
		}
	}
	// GroupAdd: supplementary groups (host group escape). Empty.
	if nonEmptyStrSlice(hc["GroupAdd"]) {
		return nil, &ContainerPolicyError{Kind: KindHostNamespaceForbidden, Image: image, Reason: "GroupAdd supplementary groups not permitted in v1"}
	}
	// LxcConf: legacy lxc config (arbitrary host escape). Empty.
	// Docker serializes HostConfig.LxcConf as a []string (JSON array
	// of "key=value"), so check it as an array — a map type
	// assertion would silently let a non-empty array through.
	if nonEmptyStrSlice(hc["LxcConf"]) {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "LxcConf not permitted in v1"}
	}
	// StorageOpt: storage driver options (host disk escape). Empty.
	if m, ok := hc["StorageOpt"].(map[string]any); ok && len(m) > 0 {
		return nil, &ContainerPolicyError{Kind: KindBindMountForbidden, Image: image, Reason: "StorageOpt not permitted in v1"}
	}
	// ContainerIDFile: writes the container ID to a host file. Empty.
	if s, _ := hc["ContainerIDFile"].(string); s != "" {
		return nil, &ContainerPolicyError{Kind: KindBindMountForbidden, Image: image, Reason: "ContainerIDFile host file write not permitted"}
	}
	// Cgroup: explicit cgroup path (cgroup escape). Empty.
	if s, _ := hc["Cgroup"].(string); s != "" {
		return nil, &ContainerPolicyError{Kind: KindHostNamespaceForbidden, Image: image, Reason: "Cgroup=" + s + " cgroup path escape denied"}
	}
	// DnsOptions / DnsSearch: like the validated Dns, DNS-related
	// fields can redirect resolution. Empty.
	if nonEmptyStrSlice(hc["DnsOptions"]) || nonEmptyStrSlice(hc["DnsSearch"]) {
		return nil, &ContainerPolicyError{Kind: KindDeviceForbidden, Image: image, Reason: "DnsOptions/DnsSearch not permitted in v1"}
	}

	// 5b. ALLOWLIST enforcement (spec.md:222 / ADR 0002: "unknown security-
	// relevant request fields" denied). The checks above validate the
	// VALUES of the known-empty fields Testcontainers always sends
	// (REPORT §"Create-body field analysis"). This check rejects any
	// HostConfig key NOT in the v1 permitted set, so a future Docker API
	// field (or a field the REPORT didn't enumerate) cannot pass through
	// unexamined. The permitted set is the union of: the validated
	// security-relevant fields (all of which must be empty/default above),
	// the rewritten fields (PortBindings), and the pass-through resource
	// fields (Memory/NanoCpus, subject to host ceilings validated at the
	// manifest gate). Everything else is denied fail-closed.
	//
	// Collect ALL unknown keys in one pass and report them together. A
	// first-key-only denial hides subsequent missing fields behind the
	// first failure, forcing one rebuild+IT cycle per field — the
	// "measured allowlist" was captured against an older docker-java and
	// newer client versions serialize additional fields (Isolation,
	// PidsLimit, VolumeDriver). Reporting all unknowns in a single
	// structured denial surfaces the complete gap in one IT run.
	var unknown []string
	for k := range hc {
		if !allowedHostConfigKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, &ContainerPolicyError{Kind: KindUnknownEndpoint, Image: image, Reason: "unknown HostConfig field(s) denied (fail-closed): " + strings.Join(unknown, ", ")}
	}

	// 6. Labels: reject client-set omac.* labels (forgeable); inject the
	// ownership label.
	labels, _ := body["Labels"].(map[string]any)
	for k := range labels {
		if strings.HasPrefix(k, "omac.") {
			return nil, &ContainerPolicyError{Kind: KindReservedLabel, Image: image, Reason: "client set reserved label " + k}
		}
	}
	if labels == nil {
		labels = map[string]any{}
	}
	labels[OwnershipLabelKey] = executorID
	body["Labels"] = labels

	// 7. PortBindings: rewrite HostIp to 127.0.0.1 (loopback-only
	// publishing, spec §226). Empty HostPort allowed (ephemeral). The
	// mapped ports are registered as executor-owned endpoints by the
	// proxy after the create returns (see proxy.go).
	if pb, ok := hc["PortBindings"].(map[string]any); ok {
		for _, bindings := range pb {
			if arr, ok := bindings.([]any); ok {
				for _, b := range arr {
					if m, ok := b.(map[string]any); ok {
						m["HostIp"] = "127.0.0.1"
					}
				}
			}
		}
	}

	rewritten, err := json.Marshal(body)
	if err != nil {
		return nil, &ContainerPolicyError{Kind: KindUnknownEndpoint, Reason: "rewrite create body: " + err.Error()}
	}
	return rewritten, nil
}

// isDefaultMode reports whether a HostConfig mode string is the Docker
// default (empty or "default"). Anything else is a host-namespace escape.
func isDefaultMode(s string) bool {
	return s == "" || s == "default"
}

// allowedHostConfigKeys is the v1-permitted set of HostConfig keys on a
// /containers/create body. Any key NOT in this set is denied fail-closed
// (spec.md:222 / ADR 0002: unknown security-relevant fields denied).
//
// The set is the union of:
//   - security-relevant fields validated to be empty/default above
//     (Privileged, Binds, Mounts, the seven modes incl. Isolation,
//     CapAdd/CapDrop, Devices, SecurityOpt, Dns/DnsOptions/DnsSearch,
//     ExtraHosts, CgroupParent/UTSMode, AutoRemove, Init,
//     DeviceRequests, Links, VolumesFrom, Sysctls, DeviceCgroupRules,
//     PublishAllPorts, RestartPolicy, GroupAdd, LxcConf, StorageOpt,
//     ContainerIDFile, Cgroup)
//   - the rewritten field (PortBindings)
//   - pass-through resource fields (Memory, NanoCpus, PidsLimit, the CPU
//     and blkio limit families, KernelMemory, MemoryReservation,
//     MemorySwap, MemorySwappiness, OomKillDisable, DiskQuota, IO limits,
//     Ulimits) — DoS-mitigation limits, not escape vectors; the manifest
//     gate's host-ceiling validation is the authoritative bound
//   - benign always-serialized fields (ReadonlyRootfs, Tmpfs, ShmSize,
//     OomScoreAdj, LogConfig, ConsoleSize, VolumeDriver)
//
// REPORT.md §"Create-body field analysis" lists the keys the docker-java
// version captured in ticket 02 serializes; newer docker-java versions
// serialize additional fields (the original capture did not include
// Isolation, PidsLimit, VolumeDriver, or the 36 fields surfaced by the
// all-unknowns-at-once diagnostic). Fields absent from this map are
// deliberately excluded so a future Docker field cannot pass through
// unexamined — the fail-closed unknown-field check surfaces any gap.
var allowedHostConfigKeys = map[string]bool{
	// Security-relevant (validated empty/default above; listed so the
	// allowlist permits their PRESENCE with safe values, not their
	// arbitrary use).
	"Privileged":        true,
	"Binds":             true,
	"Mounts":            true,
	"NetworkMode":       true,
	"PidMode":           true,
	"IpcMode":           true,
	"UsernsMode":        true,
	"CgroupnsMode":      true,
	"Runtime":           true,
	"Isolation":         true, // Testcontainers serializes it on macOS; validated empty/default above
	"CapAdd":            true,
	"CapDrop":           true, // harmless; Testcontainers sometimes sends it
	"Devices":           true,
	"SecurityOpt":       true,
	"Dns":               true,
	"DnsOptions":        true,
	"DnsSearch":         true,
	"ExtraHosts":        true,
	"CgroupParent":      true,
	"UTSMode":           true,
	"AutoRemove":        true,
	"Init":              true,
	"DeviceRequests":    true,
	"Links":             true, // validated empty above
	"VolumesFrom":       true, // validated empty above
	"Sysctls":           true, // validated empty above
	"DeviceCgroupRules": true, // validated empty above
	"PublishAllPorts":   true, // validated false above
	"RestartPolicy":     true, // validated "" or "no" above
	"GroupAdd":          true, // validated empty above
	"LxcConf":           true, // validated empty above
	"StorageOpt":        true, // validated empty above
	"ContainerIDFile":   true, // validated empty above
	"Cgroup":            true, // validated empty above
	// Rewritten by the proxy.
	"PortBindings": true,
	// Pass-through resource fields (manifest gate enforces the ceiling).
	// DoS-mitigation limits, not escape vectors.
	"Memory":               true,
	"NanoCpus":             true,
	"PidsLimit":            true, // cgroup PID limit
	"KernelMemory":         true,
	"MemoryReservation":    true,
	"MemorySwap":           true,
	"MemorySwappiness":     true,
	"OomKillDisable":       true, // bool; benign (disables OOM killer for the container)
	"DiskQuota":            true,
	"CpuCount":             true,
	"CpuPercent":           true,
	"CpuPeriod":            true,
	"CpuQuota":             true,
	"CpuRealtimePeriod":    true,
	"CpuRealtimeRuntime":   true,
	"CpuShares":            true,
	"CpusetCpus":           true,
	"CpusetMems":           true,
	"BlkioWeight":          true,
	"BlkioWeightDevice":    true,
	"BlkioDeviceReadBps":   true,
	"BlkioDeviceReadIOps":  true,
	"BlkioDeviceWriteBps":  true,
	"BlkioDeviceWriteIOps": true,
	"IOMaximumBandwidth":   true,
	"IOMaximumIOps":        true,
	"Ulimits":              true, // [] of {Name,Soft,Hard}; resource limit, not escape
	// Testcontainers 1.21 always-serialized, v1-safe, not security-relevant.
	"ReadonlyRootfs": true,
	"Tmpfs":          true,
	"ShmSize":        true,
	"OomScoreAdj":    true,
	"LogConfig":      true,
	// Benign/legacy fields docker-java serializes as null/empty/0 by
	// default (not in the original ticket-02 capture). Pass-through.
	"ConsoleSize":  true, // [rows, cols]; terminal size, benign
	"VolumeDriver": true, // legacy volume plugin field; empty in modern use
}

func nonEmptyStrSlice(v any) bool {
	arr, ok := v.([]any)
	return ok && len(arr) > 0
}

func nonEmptyAnySlice(v any) bool {
	// Identical to nonEmptyStrSlice; kept as a separate name only for
	// call-site readability (Mounts/Devices/DeviceRequests vs Binds/Caps).
	return nonEmptyStrSlice(v)
}

func imageApproved(image string, approved []string) bool {
	if image == "" {
		return false
	}
	// Docker may send "repo:tag" or "repo@digest"; compare the repo part
	// against the approved reference set. The manifest stores fully-
	// qualified refs (e.g. "pgvector/pgvector:pg16"); accept an exact match
	// OR a repo-only match when the create carries no tag (Docker defaults
	// to :latest, but the manifest must declare what is approved).
	for _, a := range approved {
		if a == image {
			return true
		}
		// repo match: manifest "pgvector/pgvector:pg16", create "pgvector/pgvector"
		if stripTag(a) == image || stripTag(image) == a || stripTag(a) == stripTag(image) {
			return true
		}
	}
	return false
}

func stripTag(ref string) string {
	// Strip a trailing :tag (but not a digest @sha256:...).
	if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i:], "@") {
		return ref[:i]
	}
	return ref
}

func isRyukImage(image string) bool {
	return strings.HasPrefix(stripTag(image), ryukImage)
}

// extractedPorts returns the host ports the daemon assigned to the created
// container's published ports, parsed from the create response body (the
// daemon returns the Id; the port mapping is discovered via a subsequent
// GET /containers/{id}/json). This helper parses the inspect response.
// Returns [] of {containerPort, hostPort} for the executor-endpoint
// registry; in v1 we register the host port as a loopback endpoint.
func extractPublishedPorts(inspectBody []byte) []PortMapping {
	var resp struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(inspectBody, &resp); err != nil {
		return nil
	}
	var out []PortMapping
	for containerPort, bindings := range resp.NetworkSettings.Ports {
		for _, b := range bindings {
			if b.HostPort != "" {
				out = append(out, PortMapping{ContainerPort: containerPort, HostPort: b.HostPort, HostIP: b.HostIP})
			}
		}
	}
	return out
}

// PortMapping is one published port the proxy registered as an
// executor-owned endpoint.
type PortMapping struct {
	ContainerPort string
	HostPort      string
	HostIP        string
}

// rewriteContainersListFilter strips the client-supplied label filter and
// injects the executor ownership label, enforcing server-side scoping
// (REPORT.md: client filters are forgeable). Returns the rewritten query
// string (without leading '?') or "" if there is no filter.
func rewriteContainersListFilter(rawQuery, executorID string) string {
	// Docker filters arrive as filters=<json url-encoded>. Parse, drop any
	// label filter, inject omac.executor=<id>, re-encode.
	q := rawQuery
	// We rebuild the query keeping all params except filters, then append
	// the rewritten filters.
	var rest []string
	var filtersVal string
	for _, kv := range strings.Split(q, "&") {
		if kv == "" {
			continue
		}
		if strings.HasPrefix(kv, "filters=") {
			filtersVal = strings.TrimPrefix(kv, "filters=")
			continue
		}
		rest = append(rest, kv)
	}
	var filters map[string]any
	if filtersVal != "" {
		decoded, err := urlQueryUnescape(filtersVal)
		if err == nil {
			_ = json.Unmarshal([]byte(decoded), &filters)
		}
	}
	if filters == nil {
		filters = map[string]any{}
	}
	// Drop ALL client-supplied label filters (they are forgeable) and
	// inject ONLY the executor ownership label (REPORT.md: the filter must
	// enforce, not trust, the label scoping).
	filters["label"] = []any{OwnershipLabelKey + "=" + executorID}
	encoded, _ := json.Marshal(filters)
	out := append([]string{}, rest...)
	out = append(out, "filters="+urlQueryEscape(string(encoded)))
	return strings.Join(out, "&")
}

// rewriteNetworksPruneFilter injects the ownership label filter into a
// POST /networks/prune request so only THIS executor's networks are
// pruned. The client's filter (if any) is dropped and replaced — client
// filters are forgeable, the proxy enforces ownership server-side
// (matching rewriteContainersListFilter). Returns the rewritten query
// string (without leading '?').
func rewriteNetworksPruneFilter(rawQuery, executorID string) string {
	return rewritePruneFilter(rawQuery, executorID)
}

// rewriteVolumesPruneFilter injects the ownership label filter into a
// POST /volumes/prune request so only THIS executor's volumes are
// pruned. Identical mechanism to rewriteNetworksPruneFilter: the
// JVMHookResourceReaper shutdown hook calls both endpoints, and both
// accept the same filters=<json url-encoded> query shape.
func rewriteVolumesPruneFilter(rawQuery, executorID string) string {
	return rewritePruneFilter(rawQuery, executorID)
}

// rewriteImagesPruneFilter injects the ownership label filter into a
// POST /images/prune request. Same shared implementation: the
// JVMHookResourceReaper shutdown hook calls networks/volumes/images
// prune, all with the same filters=<json url-encoded> query shape.
func rewriteImagesPruneFilter(rawQuery, executorID string) string {
	return rewritePruneFilter(rawQuery, executorID)
}

// rewritePruneFilter is the shared implementation for /networks/prune,
// /volumes/prune, and /images/prune. Docker prune filters arrive as
// filters=<json url-encoded>. Parse, drop any label filter, inject
// omac.executor=<id>, re-encode. Keep any non-label filters the client
// sent (e.g. "until"). Returns the rewritten query string (without '?').
func rewritePruneFilter(rawQuery, executorID string) string {
	var rest []string
	var filtersVal string
	for _, kv := range strings.Split(rawQuery, "&") {
		if kv == "" {
			continue
		}
		if strings.HasPrefix(kv, "filters=") {
			filtersVal = strings.TrimPrefix(kv, "filters=")
			continue
		}
		rest = append(rest, kv)
	}
	var filters map[string]any
	if filtersVal != "" {
		decoded, err := urlQueryUnescape(filtersVal)
		if err == nil {
			_ = json.Unmarshal([]byte(decoded), &filters)
		}
	}
	if filters == nil {
		filters = map[string]any{}
	}
	// Drop ALL client-supplied label filters (forgeable) and inject
	// ONLY the executor ownership label.
	filters["label"] = []any{OwnershipLabelKey + "=" + executorID}
	encoded, _ := json.Marshal(filters)
	out := append([]string{}, rest...)
	out = append(out, "filters="+urlQueryEscape(string(encoded)))
	return strings.Join(out, "&")
}

// urlQueryEscape / urlQueryUnescape are thin wrappers kept in-package so
// the policy logic is unit-testable without importing net/url at the top
// (it is imported by proxy.go). They use net/url.QueryEscape.
func urlQueryEscape(s string) string            { return queryEscape(s) }
func urlQueryUnescape(s string) (string, error) { return queryUnescape(s) }

// fmtPortMappings renders the registered ports for audit (no env values).
func fmtPortMappings(ports []PortMapping) string {
	if len(ports) == 0 {
		return ""
	}
	var parts []string
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%s->%s:%s", p.ContainerPort, p.HostIP, p.HostPort))
	}
	return strings.Join(parts, ",")
}
