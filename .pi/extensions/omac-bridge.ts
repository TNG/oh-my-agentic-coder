/**
 * omac Pi bridge extension
 * ========================
 *
 * Bridges Pi (running as `pi`, wrapped by `omac start pi`) to the omac
 * control plane so that each directory a session opens gets its skills
 * brought online lazily, and the skills manifest + sandbox briefing are
 * injected into the system prompt.
 *
 * This is the Pi-side counterpart to the OpenCode plugin
 * (.opencode/plugins/omac-multidir.ts). It implements the common omac
 * bridge interface (see docs/MULTI_DIR_DESKTOP.md and the agent-bridge
 * spec):
 *
 *   1. Activate on session start — POST /__omac__/activate {dir}
 *   2. Surface skills to the agent — inject manifest + briefing via
 *      before_agent_start
 *   3. Expose per-skill base URLs — OMAC_<MOUNT>_BASE / OMAC_G_<MOUNT>_BASE
 *      (already in process env from omac launch)
 *
 * Degradation: if OMAC_CONTROL_BASE is unset (Pi not running under omac),
 * every branch is a no-op. The extension is inert and safe to ship anywhere.
 *
 * File layout: this MUST be a single flat file directly under
 * .pi/extensions/ (not a subdirectory with an index.ts). A subdirectory
 * form (.pi/extensions/omac-bridge/index.ts) was tried and reproducibly
 * hung `pi` indefinitely on every launch in this repo's real CLI (0.80.6) —
 * the exact same file content loaded instantly as a flat .ts file. Pi's
 * docs (https://pi.dev/docs/latest/extensions) only document flat-file
 * auto-discovery under .pi/extensions/*.ts and ~/.pi/agent/extensions/*.ts.
 *
 * Requirements: pi's extension system auto-discovers .pi/extensions/*.ts
 * (project-local) and ~/.pi/agent/extensions/*.ts (global). This file uses
 * only bundled modules (no package.json or npm install needed).
 */

// Minimal ambient declaration so this file typechecks without pulling in
// @types/node. The Pi extension host (bun/node) provides `process` at
// runtime; we only read OMAC_* vars from the environment.
declare const process: {
  env: Record<string, string | undefined>
  cwd: () => string
}

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
    })
    if (!resp.ok) return null
    return (await resp.json()) as DirManifest
  } catch {
    return null
  }
}

function renderManifest(manifest: DirManifest): string {
  const skillsDir = process.env.OMAC_HARNESS_SKILLS_DIR || ".pi/skills"
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

export default function (api: {
  on: (event: string, handler: (event: any) => void | Promise<void>) => void
}) {
  let cachedManifest: DirManifest | null = null

  api.on("session_start", async (event: any) => {
    const base = controlBase()
    if (!base) return

    const dir = event?.cwd || event?.directory || process.cwd()
    cachedManifest = await controlPost("/__omac__/activate", { dir })
  })

  api.on("before_agent_start", async (event: any) => {
    const base = controlBase()
    if (!base) return

    if (!cachedManifest) {
      const dir = event?.cwd || event?.directory || process.cwd()
      cachedManifest = await controlPost("/__omac__/activate", { dir })
    }

    if (cachedManifest) {
      const manifestText = renderManifest(cachedManifest)
      const briefing = process.env.OMAC_SANDBOX_BRIEFING || ""
      const contextBlock = briefing
        ? `${briefing}\n\n${manifestText}`
        : manifestText

      // Prefer the documented systemPrompt return contract (pi's
      // before_agent_start docs: "can return modified systemPrompt or
      // inject a message"). Also unshift a system message when the event
      // exposes a mutable messages array, for hosts that apply mutations
      // in place rather than the return value — belt-and-suspenders since
      // this hook's exact application semantics aren't fully documented.
      if (event?.messages && Array.isArray(event.messages)) {
        event.messages.unshift({ role: "system", content: contextBlock })
      }

      const original = event?.systemPrompt ?? ""
      return {
        systemPrompt: original ? `${original}\n\n${contextBlock}` : contextBlock,
      }
    }
  })
}
