/**
 * omac oh-my-pi (omp) bridge extension
 * ====================================
 *
 * Bridges omp (running as `omp`, wrapped by `omac start omp`) to the omac
 * control plane so that each directory a session opens gets its skills
 * brought online lazily, and the skills manifest + sandbox briefing are
 * injected into the system prompt.
 *
 * omp is a fork of Pi with a diverged extension surface;
 * the three differences that matter here are called out inline.
 *
 *   1. Activate on session start — POST /__omac__/activate {dir}
 *   2. Surface skills to the agent — inject manifest + briefing via
 *      before_agent_start
 *   3. Expose per-skill base URLs — OMAC_<MOUNT>_BASE / OMAC_G_<MOUNT>_BASE
 *      (already in process env from omac launch)
 *
 * Degradation: if OMAC_CONTROL_BASE is unset (omp not running under omac),
 * every branch is a no-op. The extension is inert and safe to ship anywhere.
 *
 * How omp differs from Pi:
 *
 *   - Discovery root is `.omp`, not `.pi`. omp auto-discovers flat *.ts
 *     files under <cwd>/.omp/extensions (project) and ~/.omp/agent/extensions
 *     (user). `.pi/extensions` is NOT a native root in omp, so this file
 *     must live under .omp/extensions to be loaded at all.
 *
 *   - The working directory is on the handler CONTEXT (2nd arg), not the
 *     event. omp's session_start / before_agent_start events carry no
 *     `cwd`/`directory`; read `ctx.cwd` instead (it is the live per-session
 *     directory, correct under multi-session hosts where process.cwd() is not).
 *
 *   - before_agent_start's `event.systemPrompt` is a string[] (the already
 *     rendered system blocks), not a string. We append our block as a new
 *     array element and return { systemPrompt: string[] }.
 *
 * System prompt: the briefing and manifest are injected ONLY via the
 * systemPrompt returned from before_agent_start. In omp this value flows to
 * setTurnSystemPromptOverride -> agent.setSystemPrompt — the SAME channel
 * that renders the base system block. It REPLACES the single system block; it
 * never adds a second message. Never inject via a returned `message`: that is
 * a separate channel and would put additional content at index > 0.
 *
 * IMPORTANT: this file targets omp's EXTENSION subsystem (default export +
 * api.on), NOT omp's parallel `hooks/` subsystem — whose before_agent_start
 * result only supports { message } and would silently drop a returned
 * systemPrompt. It must land on the extension discovery path
 * (.omp/extensions/…), a flat *.ts file, to work.
 *
 * Requirements: omp's extension system auto-discovers .omp/extensions/*.ts
 * (project-local) and ~/.omp/agent/extensions/*.ts (user). This file uses
 * only bundled modules (no package.json or npm install needed).
 */

// Minimal ambient declaration so this file typechecks without pulling in
// @types/node. The omp extension host (Bun) provides `process` and global
// `fetch`/`AbortSignal` at runtime; we only read OMAC_* vars from the
// environment.
declare const process: {
  env: Record<string, string | undefined>
  cwd: () => string
}

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

function controlBase(): string | undefined {
  return process.env.OMAC_CONTROL_BASE?.replace(/\/+$/, "")
}

async function controlPost(path: string, body: unknown): Promise<DirManifest | null> {
  const base = controlBase()
  if (!base) return null
  try {
    const resp = await fetch(`${base}${path}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      // omp force-times-out each event handler at 30s. Bound the request
      // well under that so a hung control plane can never blow the budget
      // (which would silently drop the injection for that turn).
      signal: AbortSignal.timeout(20_000),
    })
    if (!resp.ok) return null
    return (await resp.json()) as DirManifest
  } catch {
    return null
  }
}

function renderManifest(manifest: DirManifest): string {
  const skillsDir = process.env.OMAC_HARNESS_SKILLS_DIR || ".omp/skills"
  const lines: string[] = [
    "## omac skills available in this workspace",
    "",
    "You can call the following skill HTTP endpoints. Each `base` is the root URL for that skill's sidecar; append the skill's documented path.",
    "",
    `This workspace's project directory is: \`${manifest.dir || ""}\``,
  ]

  const globalReady = manifest.skills?.filter(
    (s) => s.scope === "global" && s.state === "ready",
  )
  if (globalReady && globalReady.length > 0) {
    lines.push(
      "",
      `IMPORTANT: **global** skills are shared by every workspace. When a global skill writes into the project (e.g. the marketplace installing a skill), you MUST pass this workspace's project directory explicitly — for the marketplace use \`"target_path": "${manifest.dir || ""}/${skillsDir}"\` (the active harness's skills directory) in the /install request body.`,
    )
  }

  lines.push("")
  const sorted = [...(manifest.skills || [])].sort((a, b) =>
    a.name.localeCompare(b.name),
  )
  for (const sk of sorted) {
    if (sk.state === "ready" && sk.base) {
      lines.push(`- **${sk.name}** (${sk.scope || ""}) — ready — base: \`${sk.base}\``)
    } else if (sk.state === "pending-credentials") {
      const missing = (sk.missing || []).join(", ")
      lines.push(
        `- **${sk.name}** (${sk.scope || ""}) — UNAVAILABLE (missing credentials: ${missing}). Run in your own terminal: ${(sk.missing || []).map((m) => `omac secrets set ${sk.name} ${m}`).join(" ; ")}`,
      )
    } else if (sk.state === "broken") {
      lines.push(
        `- **${sk.name}** (${sk.scope || ""}) — BROKEN: ${sk.detail || "see omac logs"}`,
      )
    }
  }

  return lines.join("\n")
}

// sessionDir resolves the live working directory for a handler. In omp the
// cwd is on the context (2nd arg), not the event; fall back to the event
// fields (Pi compatibility) and finally process.cwd().
function sessionDir(event: any, ctx: any): string {
  return ctx?.cwd || event?.cwd || event?.directory || process.cwd()
}

export default function (api: {
  on: (event: string, handler: (event: any, ctx: any) => void | Promise<void>) => void
}) {
  // Keyed by resolved session directory so a multi-session host (one
  // process serving several cwds) never injects another session's manifest.
  const manifests = new Map<string, DirManifest>()

  api.on("session_start", async (event: any, ctx: any) => {
    const base = controlBase()
    if (!base) return

    const dir = sessionDir(event, ctx)
    const m = await controlPost("/__omac__/activate", { dir })
    if (m) manifests.set(dir, m)
  })

  api.on("before_agent_start", async (event: any, ctx: any) => {
    const base = controlBase()
    if (!base) return

    // Refresh every turn so skills installed/fixed after the first turn
    // surface on the next. controlPost bounds the request at 20s and
    // returns null on failure; keep the prior manifest if the fresh fetch
    // returns null so a transient control-plane blip doesn't erase the
    // injection for the turn.
    const dir = sessionDir(event, ctx)
    const fresh = await controlPost("/__omac__/activate", { dir })
    if (fresh) manifests.set(dir, fresh)
    const manifest = manifests.get(dir)
    if (!manifest) return

    const manifestText = renderManifest(manifest)
    const briefing = process.env.OMAC_SANDBOX_BRIEFING || ""
    const contextBlock = briefing
      ? `${briefing}\n\n${manifestText}`
      : manifestText

    // The returned systemPrompt is the ONLY injection path: omp folds it
    // back through setTurnSystemPromptOverride -> agent.setSystemPrompt,
    // the same setter used for the base prompt, so no second
    // {role:"system"} message is ever created. omp's event.systemPrompt is
    // a string[] (the already-rendered blocks); append our block as a new
    // element rather than string-concatenating, preserving prior blocks.
    const original: string[] = Array.isArray(event?.systemPrompt)
      ? event.systemPrompt
      : event?.systemPrompt
        ? [String(event.systemPrompt)]
        : []
    return {
      systemPrompt: [...original, contextBlock],
    }
  })
}
