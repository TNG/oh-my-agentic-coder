import assert from "node:assert/strict"
import { once } from "node:events"
import { EventEmitter } from "node:events"
import { test } from "node:test"
import * as entry from "../assets/omac-multidir.ts"

const plugin = entry.default

// setControlTimeout overrides the control-plane request timeout for one test.
function setControlTimeout(t, ms) {
  const old = process.env.OMAC_CONTROL_TIMEOUT_MS
  process.env.OMAC_CONTROL_TIMEOUT_MS = String(ms)
  t.after(() => old === undefined ? delete process.env.OMAC_CONTROL_TIMEOUT_MS : process.env.OMAC_CONTROL_TIMEOUT_MS = old)
}

function fixture(t, enabled = true, opts = {}) {
  for (const [key, value] of Object.entries({
    OMAC_CONTROL_BASE: enabled ? "http://omac.test" : "",
    OMAC_CONTROL_TOKEN: "control-token",
    OMAC_SANDBOX_BRIEFING: "omac sandbox briefing",
  })) {
    const old = process.env[key]
    process.env[key] = value
    t.after(() => old === undefined ? delete process.env[key] : process.env[key] = old)
  }
  const calls = []
  const sessions = new Map()
  const manifests = new Map()
  // Directories that have already received a dir_token, mirroring serve.go's
  // emit-once contract: only the first /activate response carries the token.
  const emitted = new Set()
  const originalFetch = globalThis.fetch
  globalThis.fetch = async (url, options) => {
    assert.equal(options.headers["X-Omac-Control-Token"], "control-token")
    const body = JSON.parse(options.body)
    calls.push({ path: new URL(url).pathname, ...body })
    const dir = body.dir
    // Simulate a control plane that never answers: reject only when the
    // caller's signal aborts (as a real fetch would). Without a signal this
    // request never settles, which is exactly the hang under test.
    if (opts.stallAll || (opts.stallDirs && opts.stallDirs.has(dir))) {
      return new Promise((_, reject) => {
        const signal = options.signal
        if (!signal) return
        if (signal.aborted) reject(new Error("aborted"))
        else signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true })
      })
    }
    // A manifest a test placed in the map is returned verbatim.
    const custom = manifests.get(dir)
    if (custom) return Response.json(custom)
    const manifest = {
      dir, state: "active",
      skills: [
        { name: "demo", mount: "demo", scope: "workdir", state: "ready", base: `http://skill.test${dir}` },
        { name: "shared", mount: "shared", scope: "global", state: "ready", base: "http://global.test" },
        { name: "broken", mount: "broken", scope: "workdir", state: "broken" },
      ],
    }
    if (!emitted.has(dir)) {
      emitted.add(dir)
      manifest.dir_token = dir?.replaceAll("/", "_")
    }
    return Response.json(manifest)
  }
  t.after(() => globalThis.fetch = originalFetch)
  return { calls, sessions, manifests }
}

async function adapter(t, version, state, dir = "/app", opts = {}) {
  if (version === 1) {
    const hooks = await plugin.server({
      directory: dir, worktree: dir,
      client: { session: { get: async ({ path }) => ({ data: { directory: state.sessions.get(path.id) } }) } },
    })
    return {
      event: (type, id, directory) => hooks.event({ event: {
        type: type === "session.moved" ? "session.updated" : type,
        properties: { info: { id, directory } },
      } }),
      prompt: async (id, system = ["existing"]) => {
        await hooks["experimental.chat.system.transform"]({ sessionID: id }, { system })
        return system
      },
      shell: async (id, cwd, env = {}) => {
        await hooks["shell.env"]({ sessionID: id }, { env })
        return env
      },
      hasDispose: typeof hooks.dispose === "function",
      dispose: () => hooks.dispose?.(),
    }
  }
  const hooks = new Map()
  const emitter = new EventEmitter()
  const queue = []
  let signal
  let closed = false
  const cleanup = await plugin.setup({
    location: { directory: dir },
    session: {
      get: async ({ sessionID }) => ({ location: { directory: state.sessions.get(sessionID) } }),
      hook: async (name, callback) => hooks.set(name, callback),
    },
    shell: { hook: async (name, callback) => hooks.set(name, callback) },
    event: { async *subscribe(options) {
      signal = options.signal
      const abort = () => emitter.emit("ready")
      signal.addEventListener("abort", abort)
      try {
        while (!signal.aborted) {
          if (!queue.length) await once(emitter, "ready")
          const next = queue.shift()
          if (!next) continue
          yield next.event
          next.done()
        }
      } finally {
        closed = true
        signal.removeEventListener("abort", abort)
      }
    } },
  })
  // A test that exercises cleanup against a stalled fetch manages it itself;
  // an automatic t.after(cleanup) would hang the runner on a failed cleanup.
  if (!opts.deferCleanup) t.after(cleanup)
  return {
    hooks,
    cleanup,
    get closed() { return closed },
    get signal() { return signal },
    event: (type, sessionID, directory) => new Promise((done) => {
      queue.push({ event: { type, data: { sessionID, location: { directory } } }, done })
      emitter.emit("ready")
    }),
    prompt: async (id, system = [{ type: "text", text: "existing" }], hook = "context") => {
      await hooks.get(hook)({ sessionID: id, system })
      return system
    },
    shell: async (id, cwd, env = {}) => {
      await hooks.get("create.before")({ cwd, env })
      return env
    },
  }
}

test("one object entrypoint, without duplicate named exports or npm runtime imports", () => {
  assert.deepEqual(Object.keys(entry), ["default"])
  assert.equal(plugin.id, "omac.multidir")
  assert.equal(typeof plugin.server, "function")
  assert.equal(typeof plugin.setup, "function")
})

for (const version of [1, 2]) {
  test(`v${version}: activation, manifest refresh, session isolation and deletion`, async (t) => {
    const state = fixture(t)
    const api = await adapter(t, version, state)
    assert.deepEqual(state.calls[0], { path: "/__omac__/activate", dir: "/app" })
    await api.event("session.created", "a", "/app")
    await api.event("session.created", "a2", "/app")
    await api.event("session.created", "b", "/other")
    const system = await api.prompt("b")
    assert.equal(system.length, 1)
    const text = version === 1 ? system[0] : system[0].text
    assert.match(text, /omac sandbox briefing/)
    assert.match(text, /http:\/\/skill.test\/other/)
    assert.doesNotMatch(text, /http:\/\/skill.test\/app/)
    const env = await api.shell("b", "/other/src", { USER_VAR: "kept" })
    assert.equal(env.OMAC_DEMO_BASE, "http://skill.test/other")
    assert.equal(env.OMAC_D__OTHER_DEMO_BASE, "http://skill.test/other")
    assert.equal(env.OMAC_G_SHARED_BASE, "http://global.test")
    assert.equal(env.OMAC_BROKEN_BASE, undefined)
    assert.equal(env.USER_VAR, "kept")
    state.manifests.set("/other", {
      dir: "/other", dir_token: "_other", state: "active",
      skills: [{ name: "demo", mount: "demo", scope: "workdir", state: "ready", base: "http://reloaded.test" }],
    })
    assert.equal((await api.shell("b", "/other")).OMAC_DEMO_BASE, "http://reloaded.test")
    await api.event("session.deleted", "a")
    assert.equal(state.calls.filter((c) => c.path === "/__omac__/deactivate").length, 0)
    await api.event("session.deleted", "a2")
    assert.deepEqual(state.calls.at(-1), { path: "/__omac__/deactivate", dir: "/app" })
  })

  test(`v${version}: restored sessions, moves, and report deduplication`, async (t) => {
    const state = fixture(t)
    state.sessions.set("restored", "/restored")
    const api = await adapter(t, version, state)
    assert.match(JSON.stringify(await api.prompt("restored")), /skill.test\/restored/)
    await api.event("session.created", "s", "/app")
    await api.event("session.moved", "s", "/moved")
    assert.equal(state.calls.filter((c) => c.path === "/__omac__/session" && c.session === "s").length, 1)
    assert.equal((await api.shell("s", "/moved")).OMAC_DEMO_BASE, "http://skill.test/moved")
    assert.ok(state.calls.some((c) => c.path === "/__omac__/deactivate" && c.dir === "/app"))
    await api.event("session.deleted", "s")
    assert.deepEqual(state.calls.at(-1), { path: "/__omac__/deactivate", dir: "/moved" })
  })

  test(`v${version}: briefing works without serve; empty system gets one part`, async (t) => {
    const state = fixture(t, false)
    const api = await adapter(t, version, state)
    const system = await api.prompt("s", [])
    assert.equal(system.length, 1)
    assert.match(JSON.stringify(system), /omac sandbox briefing/)
    assert.deepEqual(await api.shell("s", "/app", { KEEP: "yes" }), { KEEP: "yes" })
    assert.deepEqual(state.calls, [])
    delete process.env.OMAC_SANDBOX_BRIEFING
    assert.deepEqual(await api.prompt("s", []), [])
  })

  test(`v${version}: null skills and unavailable control plane do not break hooks`, async (t) => {
    const state = fixture(t)
    state.manifests.set("/app", { dir: "/app", dir_token: "app", skills: null })
    state.sessions.set("s", "/app")
    const api = await adapter(t, version, state)
    assert.deepEqual(await api.shell("s", "/app"), {})
    globalThis.fetch = async () => new Response("unavailable", { status: 503 })
    const originalError = console.error
    console.error = () => {}
    t.after(() => console.error = originalError)
    assert.match(JSON.stringify(await api.prompt("s")), /omac sandbox briefing/)
  })
}

test("v2: longest directory match, boundaries, root and trailing separators", async (t) => {
  const state = fixture(t)
  const api = await adapter(t, 2, state)
  await api.event("session.created", "nested", "/app/nested")
  assert.equal((await api.shell(undefined, "/app/nested/src")).OMAC_DEMO_BASE, "http://skill.test/app/nested")
  assert.deepEqual(await api.shell(undefined, "/app-other"), {})
  assert.deepEqual(await api.shell(undefined, "relative/path"), {})
  await api.event("session.created", "trailing", "/trailing/")
  assert.equal((await api.shell(undefined, "/trailing/src")).OMAC_DEMO_BASE, "http://skill.test/trailing/")
  await api.event("session.created", "root", "/")
  assert.equal((await api.shell(undefined, "/elsewhere")).OMAC_DEMO_BASE, "http://skill.test/")
})

test("v2 shell cwd resolves to the most specific known project and never to an unrelated directory", async (t) => {
  const state = fixture(t)
  const api = await adapter(t, 2, state)
  // Two active projects: a broader one and a nested, more specific one.
  await api.event("session.created", "broad", "/projects/main")
  await api.event("session.created", "specific", "/projects/main/vendor")
  const env = await api.shell(undefined, "/projects/main/vendor/src")
  // The more specific project wins, even though both are ancestors of cwd.
  assert.equal(env.OMAC_DEMO_BASE, "http://skill.test/projects/main/vendor")
  assert.equal(env.OMAC_D__PROJECTS_MAIN_VENDOR_DEMO_BASE, "http://skill.test/projects/main/vendor")
  // The broader project must not leak in.
  assert.equal(env.OMAC_D__PROJECTS_MAIN_DEMO_BASE, undefined)
  // A cwd outside every known project receives no skill variables.
  assert.deepEqual(await api.shell(undefined, "/unrelated/src"), {})
})

test("v2: all model hooks preserve frozen system parts, cache and metadata", async (t) => {
  const state = fixture(t, false)
  const api = await adapter(t, 2, state)
  for (const hook of ["context", "compaction", "generate", "title"]) {
    const first = Object.freeze({ type: "text", text: "first" })
    const last = Object.freeze({ type: "text", text: "last", cache: { mode: "auto" }, metadata: { owner: "another plugin" } })
    const system = await api.prompt("s", [first, last], hook)
    assert.equal(system.length, 2)
    assert.equal(system[0], first)
    assert.equal(system[1].text, "last\n\nomac sandbox briefing")
    assert.equal(system[1].cache, last.cache)
    assert.equal(system[1].metadata, last.metadata)
    assert.equal(last.text, "last")
  }
})

test("v2: unload aborts and closes the event subscription", async (t) => {
  const state = fixture(t)
  const api = await adapter(t, 2, state)
  assert.equal(api.signal.aborted, false)
  await api.cleanup()
  assert.equal(api.signal.aborted, true)
  assert.equal(api.closed, true)
})

for (const version of [1, 2]) {
  test(`v${version}: a stalled control-plane fetch is bounded by the request timeout`, async (t) => {
    setControlTimeout(t, 100)
    const state = fixture(t, true, { stallAll: true })
    const settled = await Promise.race([
      adapter(t, version, state).then(() => "settled"),
      new Promise((resolve) => setTimeout(() => resolve("hung"), 1500)),
    ])
    assert.equal(settled, "settled", "plugin init must settle, not hang, when a fetch stalls")
  })
}

test("v1: dispose cancels an in-flight control-plane fetch promptly", async (t) => {
  setControlTimeout(t, 2000)
  const state = fixture(t, true, { stallDirs: new Set(["/stalled"]) })
  const api = await adapter(t, 1, state)
  assert.ok(api.hasDispose, "v1 server must expose a dispose hook to cancel in-flight fetches")
  const pending = api.event("session.created", "stalled", "/stalled")
  await new Promise((resolve) => setTimeout(resolve, 20))
  const result = await Promise.race([
    api.dispose().then(() => "done"),
    new Promise((resolve) => setTimeout(() => resolve("hung"), 800)),
  ])
  assert.equal(result, "done", "v1 dispose must not wait for a stalled control-plane fetch")
  await pending
})

test("v2: unload cancels an in-flight control-plane fetch promptly", async (t) => {
  // Generous timeout so only the unload abort can end the stalled fetch.
  setControlTimeout(t, 2000)
  const state = fixture(t, true, { stallDirs: new Set(["/stalled"]) })
  const api = await adapter(t, 2, state, "/app", { deferCleanup: true })
  const pending = api.event("session.created", "stalled", "/stalled")
  // Give the event loop a moment to start the stalled fetch inside the handler.
  await new Promise((resolve) => setTimeout(resolve, 20))
  const start = Date.now()
  const result = await Promise.race([
    api.cleanup().then(() => "done"),
    new Promise((resolve) => setTimeout(() => resolve("hung"), 800)),
  ])
  const elapsed = Date.now() - start
  assert.equal(result, "done", "unload must not wait for a stalled control-plane fetch")
  assert.ok(elapsed < 500, `unload took ${elapsed}ms; it should cancel the fetch promptly`)
  await pending
})
