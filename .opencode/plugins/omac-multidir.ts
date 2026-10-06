/**
 * omac multi-directory plugin
 * ===========================
 *
 * Bridges OpenCode (running as `opencode serve`, wrapped by `omac serve`) to
 * the omac control plane so that each directory a session opens gets its
 * skills brought online lazily, isolated per workdir.
 *
 * See docs/contributing/serve-spec.md. The omac side is implemented in
 * internal/cli/serve.go; this plugin is the OpenCode-side counterpart that
 * closes milestone M4.
 *
 * Responsibilities (and the spec section each maps to):
 *   1. Activate-on-directory-open  — POST /__omac__/activate {dir}      (§5.2 pull trigger)
 *   2. Surface skills to the agent — system transform / model hooks    (§6.3 manifest)
 *   3. Skill env                   — shell hooks inject OMAC_D_* vars   (§4.1, §5.5)
 *   4. Session→directory mapping   — so each session only ever sees its
 *                                    own dir's token (§8 isolation)
 *   5. Lifecycle                   — deactivate on session delete         (§5.2)
 *
 * What this plugin does NOT do (omac owns it): minting tokens, namespacing
 * routes, spawning/health-checking sidecars, secret resolution, and the
 * shared `__global__` skills (those are injected into the process env at
 * cold start as OMAC_G_<SKILL> and OMAC_SKILLS, needing no per-session work).
 */

// Requires OpenCode 1.18.29+ or 2.x. Older v1 loaders cannot read an object
// entrypoint. Package imports are type-only; node:path is built into the host.
import type { Plugin as V1Plugin } from "@opencode-ai/plugin"
import type { Plugin as V2Plugin } from "@opencode/plugin"
import { isAbsolute, relative, sep } from "node:path"

// Minimal ambient declaration so this file typechecks without pulling in
// @types/node. The OpenCode plugin host (bun/node) provides `process` at
// runtime; we only read OMAC_CONTROL_BASE from the environment.
declare const process: { env: Record<string, string | undefined> }

// ---- manifest shapes (mirror serve.go skillJSON / manifestFor) ----

type SkillScope = "workdir" | "global"
type SkillState = "ready" | "pending-credentials" | "broken"

interface ManifestSkill {
  name: string
  scope: SkillScope
  mount: string
  state: SkillState
  base?: string
  socket_base?: string
  missing?: string[]
  detail?: string
}

interface DirManifest {
  dir: string
  dir_token: string
  state: "activating" | "active" | "active_partial"
  skills: ManifestSkill[]
}

// envVarName mirrors sandbox.OmacDirEnvName / OmacGlobalEnvName so the
// env we inject per session matches what the Go side would produce. A
// workdir-local skill uses OMAC_D_<TOKEN>_<MOUNT>_BASE; a global skill
// uses OMAC_G_<MOUNT>_BASE (those are already in the process env, but we
// re-assert them per-session for completeness).
function envIdent(s: string): string {
  let out = ""
  for (const ch of s) {
    if ((ch >= "a" && ch <= "z")) out += ch.toUpperCase()
    else if ((ch >= "A" && ch <= "Z") || (ch >= "0" && ch <= "9")) out += ch
    else out += "_"
  }
  return out
}
function dirEnvName(token: string, mount: string): string {
  return `OMAC_D_${envIdent(token)}_${envIdent(mount)}_BASE`
}
function globalEnvName(mount: string): string {
  return `OMAC_G_${envIdent(mount)}_BASE`
}

// Upper bound on a single control-plane request, so a hung control plane
// cannot stall plugin init or a hook forever. Generous by default; the
// env override exists so tests can use a short value.
const CONTROL_TIMEOUT_MS = 10_000

function controlTimeoutMs(): number {
  const raw = process.env.OMAC_CONTROL_TIMEOUT_MS
  if (!raw) return CONTROL_TIMEOUT_MS
  const n = Number(raw)
  return Number.isFinite(n) && n > 0 ? n : CONTROL_TIMEOUT_MS
}

async function createBridge(
  pluginDir: string,
  lookupSessionDir: (sessionID: string) => Promise<string | undefined>,
) {
  const controlBase = process.env.OMAC_CONTROL_BASE?.replace(/\/+$/, "")

  // Aborts every in-flight control-plane fetch when the plugin unloads, so
  // shutdown does not wait for a slow or stuck control plane.
  const shutdown = new AbortController()

  // OpenCode instantiates this plugin once per project directory it
  // bootstraps (not once per session), and binds `directory` to that
  // project root. That — not a session event — is the reliable activation
  // trigger: many flows (reopening an existing session, headless API use)
  // never emit session.created. So we activate `directory` immediately at
  // construction. `pluginDir` is this instance's bound directory.

  // sessionID -> absolute directory, learned from session lifecycle events.
  const sessionDir = new Map<string, string>()
  // absolute directory -> latest manifest (cache; refreshed on activate/reload).
  const manifests = new Map<string, DirManifest>()
  // directories we've already issued an activate for (dedupe; omac itself is
  // idempotent, but this avoids needless round-trips).
  const activated = new Set<string>()
  // Session ids already reported to omac (see the session.* handler), so the
  // report fires once per session rather than on every session.updated.
  const reportedSessions = new Set<string>()

  function enabled(): boolean {
    return typeof controlBase === "string" && controlBase.length > 0
  }

  async function controlPost(path: string, body: unknown): Promise<DirManifest | null> {
    if (!enabled()) return null
    // One controller serves both bounds: the request timeout and plugin
    // shutdown. This avoids depending on AbortSignal.any, which older type
    // libraries may not declare.
    const controller = new AbortController()
    const onShutdown = () => controller.abort()
    if (shutdown.signal.aborted) controller.abort()
    else shutdown.signal.addEventListener("abort", onShutdown, { once: true })
    const timer = setTimeout(() => controller.abort(), controlTimeoutMs())
    try {
      // omac requires the control token on every /__omac__/* call; omitted
      // when unset so this plugin also works against pre-token omac builds.
      const headers: Record<string, string> = { "content-type": "application/json" }
      if (process.env.OMAC_CONTROL_TOKEN) {
        headers["X-Omac-Control-Token"] = process.env.OMAC_CONTROL_TOKEN
      }
      const res = await fetch(`${controlBase}${path}`, {
        method: "POST",
        headers,
        body: JSON.stringify(body),
        signal: controller.signal,
      })
      if (!res.ok) {
        // 4xx/5xx from the control plane (e.g. dir outside allowed --root).
        // Surface as a warning; do not break the session.
        const text = await res.text().catch(() => "")
        console.error(`[omac] ${path} -> ${res.status}: ${text}`)
        return null
      }
      const m = (await res.json()) as DirManifest
      // omac serializes an empty skills slice as JSON null (not []). Normalize
      // so every downstream `.skills` access is safe.
      if (m && !Array.isArray(m.skills)) m.skills = []
      return m
    } catch (err) {
      // A request cancelled by shutdown is expected; only log real failures.
      if (!shutdown.signal.aborted) console.error(`[omac] ${path} request failed:`, err)
      return null
    } finally {
      clearTimeout(timer)
      shutdown.signal.removeEventListener("abort", onShutdown)
    }
  }

  async function activate(dir: string, force = false): Promise<DirManifest | null> {
    if (!dir) return null
    // session.updated fires often; avoid re-POSTing for a dir we've already
    // activated unless a refresh is explicitly requested (omac's activate is
    // idempotent, so this is purely to cut chatter).
    if (!force && activated.has(dir)) {
      return manifests.get(dir) ?? null
    }
    const m = await controlPost("/__omac__/activate", { dir })
    if (m) {
      // The server emits dir_token only on the first activation. A refresh
      // response therefore omits it; carry the token learned earlier forward,
      // or the env names built from it break.
      if (!m.dir_token) {
        const cached = manifests.get(dir)?.dir_token
        if (cached) m.dir_token = cached
      }
      manifests.set(dir, m)
      activated.add(dir)
    }
    return m
  }

  async function deactivate(dir: string): Promise<void> {
    if (!dir || !activated.has(dir)) return
    // Only deactivate when no remaining session still uses this dir.
    for (const d of sessionDir.values()) {
      if (d === dir) return
    }
    await controlPost("/__omac__/deactivate", { dir })
    manifests.delete(dir)
    activated.delete(dir)
  }

  // Resolve a session's directory: prefer the cached mapping, else ask the
  // server (model hooks only give us a sessionID).
  async function dirForSession(sessionID: string | undefined): Promise<string | undefined> {
    if (!sessionID) return undefined
    const cached = sessionDir.get(sessionID)
    if (cached) return cached
    try {
      const dir = await lookupSessionDir(sessionID)
      if (dir) sessionDir.set(sessionID, dir)
      return dir
    } catch {
      return undefined
    }
  }

  // Collapse newlines and strip markdown bold markers from a skill-supplied
  // field so it cannot inject new blocks or forged structure into the manifest.
  function sanitizeField(s: string): string {
    return s.replace(/[\n\r]/g, " ").replace(/\*/g, "").replace(/[\x00-\x1f\x7f-\x9f]/g, "")
  }

  // Only emit omac secrets set when the name is a valid identifier and each
  // missing var name looks like a conventional env var ([A-Z_][A-Z0-9_]*).
  function secretsHint(name: string, missing: string[]): string {
    if (!/^[a-z0-9][a-z0-9-]*$/.test(name)) return ""
    const safe = missing.filter((m) => /^[A-Z_][A-Z0-9_]*$/.test(m))
    if (safe.length === 0) return ""
    return safe.map((m) => `omac secrets set ${name} ${m}`).join(" ; ")
  }

  // Build the system-prompt block describing the skills available to a dir.
  function renderManifest(m: DirManifest): string {
    const hasGlobal = (m.skills ?? []).some((s) => s.scope === "global" && s.state === "ready")
    const lines: string[] = []
    lines.push("## omac skills available in this workspace")
    lines.push("")
    lines.push(
      "You can call the following skill HTTP endpoints. Each `base` is the " +
        "root URL for that skill's sidecar; append the skill's documented path.",
    )
    lines.push("")
    lines.push(`This workspace's project directory is: \`${m.dir}\``)
    if (hasGlobal) {
      // The active harness's own skills dir (omac injects this). OpenCode →
      // .opencode/skills. Installs must land where this harness's loader reads
      // SKILL.md, so the manifest tells the agent the harness-correct path.
      const skillsDir = process.env.OMAC_HARNESS_SKILLS_DIR || ".opencode/skills"
      lines.push("")
      lines.push(
        "IMPORTANT: **global** skills are shared by every workspace and run as a " +
          "single process; they do NOT know which project you are in. When a global " +
          "skill writes into the project (e.g. the marketplace installing a skill), " +
          "you MUST pass this workspace's project directory explicitly — for the " +
          `marketplace use \`"target_path": "${m.dir}/${skillsDir}"\` in the /install ` +
          "request body (this is the active harness's skills directory). Otherwise it " +
          "installs into the wrong directory.",
      )
    }
    lines.push("")
    let anyUnavailable = false
    for (const sk of (m.skills ?? []).slice().sort((a, b) => a.name.localeCompare(b.name))) {
      const name = sanitizeField(sk.name)
      const scope = sanitizeField(sk.scope)
      if (sk.state === "ready" && sk.base) {
        lines.push(`- **${name}** (${scope}) — ready — base: \`${sk.base}\``)
      } else if (sk.state === "pending-credentials") {
        anyUnavailable = true
        const miss = (sk.missing ?? []).map(sanitizeField).join(", ")
        const cmds = secretsHint(sk.name, sk.missing ?? [])
        const cmdSuffix = cmds
          ? `**You (the user) must run this in your own terminal** — it prompts for a ` +
            `masked value the agent cannot type:\n  \`${cmds}\`\n  ` +
            `That command auto-reloads the running omac serve, so the skill becomes ` +
            `available right after; no restart and no manual reload needed.`
          : `**You (the user) must run \`omac register\` from a host terminal to approve this skill.**`
        lines.push(`- **${name}** (${scope}) — UNAVAILABLE (missing credentials: ${miss}). ` + cmdSuffix)
      } else if (sk.state === "broken") {
        anyUnavailable = true
        lines.push(`- **${name}** (${scope}) — BROKEN: ${sanitizeField(sk.detail ?? "see omac logs")}`)
      }
    }
    // Tell the agent how to recover WITHOUT a full restart. The running
    // server is `omac serve`, NOT `omac start`; the way to re-activate a
    // directory after installing/fixing a skill is the control-plane reload
    // endpoint, which re-discovers, re-registers, re-resolves secrets and
    // re-spawns sidecars for the directory in place.
    if (anyUnavailable && controlBase) {
      lines.push("")
      lines.push(
        "To make a newly installed or just-fixed skill available, do NOT run " +
          "`omac start` (this is an `omac serve` deployment). Instead reload this " +
          `directory in place by POSTing to the omac control plane:\n` +
          "```\n" +
          `curl -s -X POST "${controlBase}/__omac__/reload" ` +
          `-H 'content-type: application/json' -d '{"dir":"${m.dir}"}'\n` +
          "```\n" +
          "Then re-read this list — the skill should move from BROKEN/UNAVAILABLE to ready.",
      )
    }
    return lines.join("\n")
  }

  // Eagerly activate this instance's bound project directory. This is the
  // primary trigger (the session-event handler below is a best-effort
  // supplement for directories learned from session payloads). A dir
  // outside the server's --root is refused by omac and logged, not fatal.
  if (enabled() && pluginDir) {
    await activate(pluginDir)
  }

  async function sessionOpened(id: string, dir: string): Promise<void> {
    if (!enabled() || !id || !dir) return
    const previous = sessionDir.get(id)
    sessionDir.set(id, dir)
    await activate(dir)
    if (previous && previous !== dir) await deactivate(previous)
    if (!reportedSessions.has(id)) {
      reportedSessions.add(id)
      await controlPost("/__omac__/session", { session: id })
    }
  }

  async function sessionDeleted(id: string): Promise<void> {
    const dir = sessionDir.get(id)
    sessionDir.delete(id)
    reportedSessions.delete(id)
    if (dir) await deactivate(dir)
  }

  // V2 shell events have cwd but no session ID. Choose the most specific
  // known project containing cwd. Path components avoid matching /app-other
  // to /app, and relative() handles roots, trailing separators and Windows.
  function dirForCwd(cwd: string): string | undefined {
    if (!isAbsolute(cwd)) return undefined
    let best: string | undefined
    const candidates = new Set([pluginDir, ...sessionDir.values(), ...manifests.keys()])
    for (const dir of candidates) {
      if (!dir || !isAbsolute(dir)) continue
      const child = relative(dir, cwd)
      if (child === ".." || child.startsWith(`..${sep}`) || isAbsolute(child)) continue
      if (!best || dir.length > best.length) best = dir
    }
    return best
  }

  async function systemText(sessionID: string | undefined): Promise<string> {
    const blocks: string[] = []
    const brief = process.env.OMAC_SANDBOX_BRIEFING
    if (brief && brief.trim().length > 0) blocks.push(brief)
    if (enabled()) {
      const dir = await dirForSession(sessionID)
      if (dir) {
        const m = (await activate(dir, true)) ?? manifests.get(dir)
        if (m && m.skills.length > 0) blocks.push(renderManifest(m))
      }
    }
    return blocks.join("\n\n")
  }

  async function shellEnv(dir: string | undefined, env: Record<string, string | undefined>): Promise<void> {
    if (!enabled() || !dir) return
    // Refresh on each command so skills installed mid-session are available.
    const m = (await activate(dir, true)) ?? manifests.get(dir)
    if (!m) return
    for (const sk of m.skills) {
      if (sk.state !== "ready" || !sk.base) continue
      if (sk.scope === "global") {
        env[globalEnvName(sk.mount)] = sk.base
      } else {
        env[dirEnvName(m.dir_token, sk.mount)] = sk.base
        env[`OMAC_${envIdent(sk.mount)}_BASE`] = sk.base
      }
    }
  }

  return {
    sessionOpened,
    sessionDeleted,
    dirForSession,
    dirForCwd,
    systemText,
    shellEnv,
    // The event subscription uses `signal`; unload calls `abort`, which also
    // cancels any control-plane fetch still in flight.
    signal: shutdown.signal,
    abort: () => shutdown.abort(),
  }
}

const server: V1Plugin = async ({ client, directory, worktree }) => {
  const bridge = await createBridge(directory || worktree || "", async (sessionID) => {
    const response = await client.session.get({ path: { id: sessionID } })
    return response.data?.directory
  })
  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.updated":
          await bridge.sessionOpened(event.properties.info.id, event.properties.info.directory)
          break
        case "session.deleted":
          await bridge.sessionDeleted(event.properties.info.id)
          break
      }
    },
    "experimental.chat.system.transform": async (input, output) => {
      const text = await bridge.systemText(input.sessionID)
      if (!text) return
      // Keep the existing number of system messages for strict providers.
      const last = output.system.length - 1
      if (last < 0) output.system.push(text)
      else output.system[last] = `${output.system[last]}\n\n${text}`
    },
    "shell.env": async (input, output) => {
      await bridge.shellEnv(await bridge.dirForSession(input.sessionID), output.env)
    },
    // v1 has no event-subscription signal to abort, but dispose still cancels
    // any control-plane fetch in flight so shutdown is not held up.
    dispose: async () => {
      bridge.abort()
    },
  }
}

// Plugin.define is an identity helper. `satisfies` checks the same definition
// without importing the v2 runtime into v1 installations. Export ONLY this
// object: v1 also discovers named exports and could initialize hooks twice.
export default {
  id: "omac.multidir",
  server,
  async setup(ctx) {
    const bridge = await createBridge(ctx.location.directory, async (sessionID) => {
      const info = await ctx.session.get({ sessionID })
      return info.location.directory
    })

    // Auxiliary model requests use separate hooks in v2. All carry the same
    // briefing, just as they did through v1's system transform.
    for (const hook of ["context", "compaction", "generate", "title"] as const) {
      await ctx.session.hook(hook, async (event) => {
        const text = await bridge.systemText(event.sessionID)
        if (!text) return
        const last = event.system.length - 1
        if (last < 0) event.system.push({ type: "text", text })
        else {
          // Retain the last part's cache policy and metadata, including when
          // its readonly text belongs to a frozen object from another plugin.
          event.system[last] = { ...event.system[last], text: `${event.system[last].text}\n\n${text}` }
        }
      })
    }
    await ctx.shell.hook("create.before", async (event) => {
      await bridge.shellEnv(bridge.dirForCwd(event.cwd), event.env)
    })

    const events = (async () => {
      try {
        for await (const event of ctx.event.subscribe({ signal: bridge.signal })) {
          if (bridge.signal.aborted) break
          switch (event.type) {
            case "session.created":
            case "session.moved":
              await bridge.sessionOpened(event.data.sessionID, event.data.location.directory)
              break
            case "session.deleted":
              await bridge.sessionDeleted(event.data.sessionID)
              break
          }
        }
      } catch (err) {
        if (!bridge.signal.aborted) console.error("[omac] event subscription ended:", err)
      }
    })()
    return async () => {
      // One abort cancels the event subscription and any in-flight fetch.
      bridge.abort()
      await events
    }
  },
} satisfies V2Plugin.Plugin & { server: V1Plugin }
