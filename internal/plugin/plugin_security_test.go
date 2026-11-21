package plugin

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSecurityPluginSendsCachedDirToken asserts that the harness plugin
// remembers the token issued by the first activation and presents it on every
// later control-plane request for the same directory. The embedded asset and
// its checked-in mirror must stay identical.
func TestSecurityPluginSendsCachedDirToken(t *testing.T) {
	canonicalPath := filepath.Join("..", "..", ".opencode", "plugins", MultiDirFileName)
	canonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical plugin: %v", err)
	}
	if !bytes.Equal(canonical, MultiDirSource()) {
		t.Fatalf("embedded %s drifted from %s; the two copies must be byte-identical", MultiDirFileName, canonicalPath)
	}

	assetPath, err := filepath.Abs(filepath.Join("assets", MultiDirFileName))
	if err != nil {
		t.Fatalf("resolve plugin asset: %v", err)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to exercise the plugin bridge")
	}

	script := filepath.Join(t.TempDir(), "dir-token-check.mjs")
	if err := os.WriteFile(script, []byte(pluginDirTokenScript), 0o600); err != nil {
		t.Fatalf("write plugin check: %v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Env = append(os.Environ(), "OMAC_TEST_ASSET="+assetPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plugin did not send the cached dir token on later calls: %v\n%s", err, out)
	}
}

const pluginDirTokenScript = `
import assert from "node:assert/strict"
import { pathToFileURL } from "node:url"

process.env.OMAC_CONTROL_BASE = "http://omac.test"
process.env.OMAC_CONTROL_TOKEN = "control-token"
delete process.env.OMAC_SANDBOX_BRIEFING

const token = "cached-dir-token"
const calls = []
let issued = false

function header(headers, name) {
  if (!headers) return undefined
  if (typeof headers.get === "function") return headers.get(name) ?? undefined
  for (const [key, value] of Object.entries(headers)) {
    if (key.toLowerCase() === name.toLowerCase()) return value
  }
  return undefined
}

globalThis.fetch = async (url, options = {}) => {
  const path = new URL(url).pathname
  const dir = JSON.parse(options.body).dir
  const sent = header(options.headers, "X-Omac-Dir-Token")
  calls.push({ path, dir, sent })
  if (sent !== undefined && sent !== token) {
    return new Response("forbidden", { status: 403 })
  }
  const manifest = { dir, state: "active", skills: [] }
  if (path === "/__omac__/activate" && !issued) {
    issued = true
    manifest.dir_token = token
  }
  return Response.json(manifest)
}

const asset = process.env.OMAC_TEST_ASSET
const mod = await import(pathToFileURL(asset).href)
const hooks = await mod.default.server({
  directory: "/app",
  worktree: "/app",
  client: { session: { get: async () => ({ data: { directory: "/app" } }) } },
})

const activations = () => calls.filter((call) => call.path === "/__omac__/activate")
assert.ok(activations().length >= 1, "plugin did not activate its directory")
assert.equal(activations()[0].sent, undefined, "first activation must not send a token")

await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1", directory: "/app" } } } })
await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, { system: [] })
await hooks["shell.env"]({ sessionID: "s1" }, { env: {} })

assert.ok(activations().length >= 2, "plugin did not refresh its activation")
for (const call of activations().slice(1)) {
  assert.equal(call.sent, token, "re-activation did not send the cached dir token")
}

await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "s1", directory: "/app" } } } })
const deactivations = calls.filter((call) => call.path === "/__omac__/deactivate")
assert.ok(deactivations.length >= 1, "plugin did not deactivate the directory")
for (const call of deactivations) {
  assert.equal(call.sent, token, "deactivate did not send the cached dir token")
}
`
