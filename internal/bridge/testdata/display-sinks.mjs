import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import plugin from "../../plugin/assets/omac-multidir.ts"
import pi from "../../../.pi/extensions/omac-bridge.ts"

// Replay the actual authenticated activation response produced by the CLI test.
const manifest = JSON.parse(await readFile(process.argv[2], "utf8"))
process.env.OMAC_CONTROL_BASE = "http://display.test"
process.env.OMAC_CONTROL_TOKEN = "display-test-token"
delete process.env.OMAC_SANDBOX_BRIEFING
globalThis.fetch = async (_url, options) => {
  assert.equal(options.headers["X-Omac-Control-Token"], "display-test-token")
  return Response.json(manifest)
}
function check(text) {
  assert.equal((text.match(/— BROKEN:/g) ?? []).length, manifest.skills.length)
  // Manifest renderers intentionally turn CR/LF into spaces before stripping.
  assert.equal((text.match(/café 東京 عربي left ?right/g) ?? []).length, manifest.skills.length)
  for (const sk of manifest.skills) {
    assert.ok(!text.includes(sk.detail), "raw metadata detail reached context")
  }
}
const hooks = await plugin.server({
  client: { session: { get: async () => ({ data: { directory: manifest.dir } }) } },
  directory: manifest.dir, worktree: manifest.dir,
})
try {
  const output = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "display" }, output)
  check(output.system.join("\n"))
} finally {
  await hooks.dispose?.()
}
const registered = new Map()
const cleanup = await plugin.setup({
  location: { directory: manifest.dir },
  session: {
    get: async () => ({ location: { directory: manifest.dir } }),
    hook: async (name, cb) => registered.set(name, cb),
  },
  shell: { hook: async () => {} },
  event: { async *subscribe({ signal }) {
    if (!signal.aborted) await new Promise(resolve => signal.addEventListener("abort", resolve, { once: true }))
  } },
})
try {
  const output = { system: [] }
  await registered.get("context")({ sessionID: "display", system: output.system })
  check(output.system.map(p => p.text).join("\n"))
} finally {
  await cleanup()
}
// Pi's inherited missing control-token header is out of scope: renderer-only replay.
globalThis.fetch = async () => Response.json(manifest)
const piHooks = new Map()
pi({ on: (name, cb) => piHooks.set(name, cb) })
await piHooks.get("session_start")({ cwd: manifest.dir })
const result = await piHooks.get("before_agent_start")({ cwd: manifest.dir, systemPrompt: "ordinary context" })
assert.ok(result.systemPrompt.startsWith("ordinary context"))
check(result.systemPrompt)
