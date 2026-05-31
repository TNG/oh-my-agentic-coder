/**
 * omac multi-directory plugin
 * ===========================
 *
 * Bridges OpenCode (running as `opencode serve`, wrapped by `omac serve`) to
 * the omac control plane so that each directory a session opens gets its
 * skills brought online lazily, isolated per workdir.
 *
 * See docs/MULTI_DIR_DESKTOP.md. The omac side is implemented in
 * internal/cli/serve.go; this plugin is the OpenCode-side counterpart that
 * closes milestone M4.
 *
 * Responsibilities (and the spec section each maps to):
 *   1. Activate-on-directory-open  — POST /__omac__/activate {dir}      (§5.2 pull trigger)
 *   2. Surface skills to the agent — experimental.chat.system.transform  (§6.3 manifest)
 *   3. Per-session skill env       — shell.env injects OMAC_D_* vars     (§4.1, §5.5)
 *   4. Session→directory mapping   — so each session only ever sees its
 *                                    own dir's token (§8 isolation)
 *   5. Lifecycle                   — deactivate on session delete         (§5.2)
 *
 * What this plugin does NOT do (omac owns it): minting tokens, namespacing
 * routes, spawning/health-checking sidecars, secret resolution, and the
 * shared `__global__` skills (those are injected into the process env at
 * cold start as OMAC_G_<SKILL> and OMAC_SKILLS, needing no per-session work).
 */

import type { Plugin } from "@opencode-ai/plugin"

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

export const OmacMultiDirPlugin: Plugin = async ({ client }) => {
  const controlBase = process.env.OMAC_CONTROL_BASE?.replace(/\/+$/, "")

  // sessionID -> absolute directory, learned from session lifecycle events.
  const sessionDir = new Map<string, string>()
  // absolute directory -> latest manifest (cache; refreshed on activate/reload).
  const manifests = new Map<string, DirManifest>()
  // directories we've already issued an activate for (dedupe; omac itself is
  // idempotent, but this avoids needless round-trips).
  const activated = new Set<string>()

  function enabled(): boolean {
    return typeof controlBase === "string" && controlBase.length > 0
  }

  async function controlPost(path: string, body: unknown): Promise<DirManifest | null> {
    if (!enabled()) return null
    try {
      const res = await fetch(`${controlBase}${path}`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        // 4xx/5xx from the control plane (e.g. dir outside allowed --root).
        // Surface as a warning; do not break the session.
        const text = await res.text().catch(() => "")
        console.error(`[omac] ${path} -> ${res.status}: ${text}`)
        return null
      }
      return (await res.json()) as DirManifest
    } catch (err) {
      console.error(`[omac] ${path} request failed:`, err)
      return null
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
  // server (system.transform only gives us a sessionID).
  async function dirForSession(sessionID: string | undefined): Promise<string | undefined> {
    if (!sessionID) return undefined
    const cached = sessionDir.get(sessionID)
    if (cached) return cached
    try {
      const resp: any = await (client as any).session.get({ path: { id: sessionID } })
      const dir: string | undefined = resp?.data?.directory ?? resp?.directory
      if (dir) sessionDir.set(sessionID, dir)
      return dir
    } catch {
      return undefined
    }
  }

  // Build the system-prompt block describing the skills available to a dir.
  function renderManifest(m: DirManifest): string {
    const lines: string[] = []
    lines.push("## omac skills available in this workspace")
    lines.push("")
    lines.push(
      "You can call the following skill HTTP endpoints. Each `base` is the " +
        "root URL for that skill's sidecar; append the skill's documented path.",
    )
    lines.push("")
    for (const sk of m.skills.sort((a, b) => a.name.localeCompare(b.name))) {
      if (sk.state === "ready" && sk.base) {
        lines.push(`- **${sk.name}** (${sk.scope}) — ready — base: \`${sk.base}\``)
      } else if (sk.state === "pending-credentials") {
        const miss = (sk.missing ?? []).join(", ")
        lines.push(
          `- **${sk.name}** (${sk.scope}) — UNAVAILABLE (missing credentials: ${miss}). ` +
            `Tell the user to run \`omac secrets set ${sk.name} <NAME>\` then reload.`,
        )
      } else if (sk.state === "broken") {
        lines.push(`- **${sk.name}** (${sk.scope}) — BROKEN: ${sk.detail ?? "see omac logs"}`)
      }
    }
    return lines.join("\n")
  }

  return {
    // --- 1+4: learn session→dir and activate on session open (§5.2) ---
    event: async ({ event }) => {
      if (!enabled()) return
      const e: any = event
      switch (e?.type) {
        case "session.created":
        case "session.updated": {
          const info = e.properties?.info
          const id: string | undefined = info?.id
          const dir: string | undefined = info?.directory
          if (id && dir) {
            sessionDir.set(id, dir)
            await activate(dir)
          }
          break
        }
        case "session.deleted": {
          const info = e.properties?.info
          const id: string | undefined = info?.id
          if (id) {
            const dir = sessionDir.get(id)
            sessionDir.delete(id)
            if (dir) await deactivate(dir)
          }
          break
        }
      }
    },

    // --- 2: surface the per-dir skills to the model (§6.3) ---
    "experimental.chat.system.transform": async (input, output) => {
      if (!enabled()) return
      const dir = await dirForSession(input.sessionID)
      if (!dir) return
      // Refresh the manifest so the prompt reflects the current skill
      // state (e.g. a pending-credentials skill that has since been
      // supplied a secret and reloaded).
      let m = (await activate(dir, true)) ?? manifests.get(dir)
      if (!m || m.skills.length === 0) return
      output.system.push(renderManifest(m))
    },

    // --- 3: inject per-session skill env so SKILL.md env-var reads resolve (§4.1/§5.5) ---
    "shell.env": async (input, output) => {
      if (!enabled()) return
      const dir = await dirForSession(input.sessionID)
      if (!dir) return
      const m = manifests.get(dir)
      if (!m) return
      for (const sk of m.skills) {
        if (sk.state !== "ready" || !sk.base) continue
        if (sk.scope === "global") {
          output.env[globalEnvName(sk.mount)] = sk.base
        } else {
          output.env[dirEnvName(m.dir_token, sk.mount)] = sk.base
          // Single-dir convenience alias (§5.5): also expose the flat name.
          // Harmless when multiple dirs are active because each session only
          // ever sees its own dir's manifest here.
          output.env[`OMAC_${envIdent(sk.mount)}_BASE`] = sk.base
        }
      }
    },
  }
}

export default OmacMultiDirPlugin
