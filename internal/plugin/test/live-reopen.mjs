import assert from "node:assert/strict"
import plugin from "../assets/omac-multidir.ts"

const dir = process.env.OMAC_TEST_PROJECT
const hooks = await plugin.server({
  directory: dir, worktree: dir,
  client: { session: { get: async () => ({ data: { directory: dir } }) } },
})
const event = (type, id) => hooks.event({ event: { type, properties: { info: { id, directory: dir } } } })
try {
  let firstBase, firstKey
  for (const id of ["first", "second", "third"]) {
    await event("session.created", id)
    const output = { env: {} }
    await hooks["shell.env"]({ sessionID: id }, output)
    const keys = Object.keys(output.env).filter(k => k.startsWith("OMAC_D_"))
    assert.equal(keys.length, 1)
    assert.equal(output.env[keys[0]], output.env.OMAC_DEMO_BASE)
    if (firstBase) {
      assert.equal(output.env.OMAC_DEMO_BASE, firstBase)
      assert.equal(keys[0], firstKey)
    } else {
      firstBase = output.env.OMAC_DEMO_BASE
      firstKey = keys[0]
    }
    const response = await fetch(`${process.env.OMAC_CONTROL_BASE}/__omac__/reload`, {
      method: "POST", headers: { "Content-Type": "application/json", "X-Omac-Control-Token": process.env.OMAC_CONTROL_TOKEN },
      body: JSON.stringify({ dir }),
    })
    assert.equal(response.status, 200)
    assert.equal((await response.json()).dir_token, undefined)
    const refreshed = { env: {} }
    await hooks["shell.env"]({ sessionID: id }, refreshed)
    assert.deepEqual(refreshed.env, output.env)
    await event("session.deleted", id)
  }
} finally {
  await hooks.dispose()
}
