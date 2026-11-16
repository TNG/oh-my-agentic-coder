# Explainer content format

An explainer is one HTML file: a copy of `template/page.html` with every `{{…}}` placeholder replaced. The template holds the page's look (title band, section index, styles); this file says what goes into the placeholders.

**Reader:** a maintainer who knows the project and has used it, but hasn't worked on the part of the code this PR touches. Explain anything specific to that part (a mode, a file, a protocol, a term) the first time it appears, ideally with a concrete example.

## Where to look

Read these before writing. Every claim about the code must come from something you actually read or ran; if you describe behavior you didn't verify, say so on the page.

| Source | What you get from it |
|---|---|
| PR description, linked issues, comments and reviews | the problem in the reporter's words, constraints, objections, open decisions |
| The diff against the PR's real base (for a stacked PR, the parent PR's head, not the default branch) | what actually changes |
| The code around the diff on the default branch: callers, the components the changed code talks to | how this part works today, for the "context" section |
| The project's docs for this area (architecture, security model, user guides) | terms and guarantees the change must respect |
| Other open PRs and issues touching the same files or problem | competing approaches and stacking order |
| Earlier reviews or analyses of this PR, if any | known issues to check, not to repeat unverified |
| Agent review comments on the PR (top-level comments starting with "🤖 **Agent-generated review**"), latest round only | numbered findings (F1, F2, …) to fold into the "review" section |

## Placeholders

| Placeholder | What goes in | Example |
|---|---|---|
| `{{title}}` (twice) | a short name for the page, two to four words, specific to the subject. No "PR #…" and no colon with an explanation. Plain text. | `uv in the Sandbox` |
| `{{lede}}` | one or two sentences: what this PR makes possible or prevents, in plain words. May contain `<code>`. | `Lets agents run <code>uv</code> inside the sandbox.` |
| `{{facts}}` | 2–4 short facts (size, stack position, linked issues), each in a `<span>` | `<span>+268 −0 lines in 5 files</span><span>Closes #484</span>` |
| `{{repo}}` | the repository as `owner/name` | `TNG/oh-my-agentic-coder` |
| `{{repo_name}}` | the repository name without the owner | `oh-my-agentic-coder` |
| `{{number}}` (three times) | the PR number | `487` |
| `{{head_short}}` | the first 7 characters of the head SHA you read | `e7d52b0` |
| `{{toc}}` | one entry per section, with the same id and heading as the section | `<li><a href="#what">What this PR changes</a></li>` |
| `{{sections}}` | the five sections below | |

## Sections

Exactly these five, in this order, each as `<section id="…"><h2>…</h2> … </section>`:

1. **`what`: What this PR changes.** A short plain summary, then the concrete changes grouped by component, each naming the file(s) and what is different. A small before/after example of the behavior (a command and its output, a config, a request) helps.
2. **`context`: Where it fits.** Explain the part of the codebase this PR lives in from the ground up: which components are involved, what each does, and how data or control flows between them today. A diagram that marks where the PR changes something usually helps. Name files as `<code>path/to/file.go</code>`, with function names. End with what the change means for that flow.
3. **`scenarios`: Usage scenarios.** A few concrete situations: who does what, with which setup (OS, mode, configuration), and what happens before and after the PR. Include one where behavior must *not* change (a regression check), and for a security change one from the attacker's side.
4. **`approaches`: Approaches.** The viable ways to solve the problem: this PR's approach, competing or related PRs, and credible approaches nobody has proposed yet, each with pros and cons. Close with a recommendation, the reasoning, and any open decision that blocks the choice.
5. **`review`: Reviewing it.** Where to start reading (files in a sensible order), the riskiest parts and why, open questions for the author, and how to try it by hand. Put what to verify in a checklist.

   **If the PR has an agent review comment,** fold its findings into this section, each linked to the comment:
   - Start the section with a callout naming the review (round, head it reviewed, how many findings of each kind), linked to the comment.
   - "Needs a decision" findings go under the questions for the author, with the options and the review's suggestion in one or two sentences.
   - "Directly addressable" findings go into the checklist, one item each: the problem in one sentence, then the suggested fix in one sentence.
   - "Not verified" findings go under the riskiest parts, marked as unverified.
   - If one of your own points matches a finding, keep only the finding item. If a finding contradicts something elsewhere on the page, correct that part too.
   - If the PR has commits after the head the review covered, check each finding against the new head and mark it "addressed in `<sha>`" or "still open".

## Building blocks

The template styles these in light and dark mode; use them when they help. Don't write `<script>`, `<style>`, `<link>`, `<iframe>`, `<img>`, `on…=` attributes or `javascript:` links: the page's Content-Security-Policy blocks them anyway.

```html
<!-- Text -->
<h3>…</h3> <p>…</p> <ul><li>…</li></ul> <ol>…</ol> <code>…</code> <pre><code>…</code></pre>
<a href="https://github.com/OWNER/REPO/pull/123">#123</a>

<!-- A key takeaway -->
<div class="callout"><span class="k">Label</span><p>…</p></div>

<!-- Before/after cards, e.g. for scenarios -->
<div class="cards">
  <div class="card"><h3>Scenario name</h3><p>Setup in one or two sentences.</p>
    <div class="ba">
      <div class="before"><span class="k">Today</span>What happens now.</div>
      <div class="after"><span class="k">With this PR</span>What happens after.</div>
    </div></div>
</div>

<!-- Pros/cons cards, e.g. for approaches: pill p-this for the PR's own approach, p-mut otherwise -->
<div class="cards">
  <div class="card"><h3>Approach name <span class="pill p-this">This PR</span></h3><p>What it does.</p>
    <div class="pc">
      <div><span class="k">Pros</span><ul><li>…</li></ul></div>
      <div><span class="k">Cons</span><ul><li>…</li></ul></div>
    </div></div>
</div>

<!-- Tables, e.g. comparing approaches -->
<div class="tbl"><table><thead><tr><th>…</th></tr></thead><tbody><tr><td>…</td></tr></tbody></table></div>

<!-- Review checklist -->
<ul class="checks"><li>…</li></ul>

<!-- A finding from an agent review, in the checklist or the questions -->
<li><a href="COMMENT_URL">F2</a> <strong>Short title.</strong> The problem in one sentence. Fix: the suggested fix in one sentence.</li>

<!-- Status pills: p-good, p-warn, p-bad, p-mut -->
<span class="pill p-warn">needs decision</span>
```

Escape `<`, `>` and `&` in all text and code as `&lt;`, `&gt;`, `&amp;`, including text copied from the PR.

### Diagrams

Use Mermaid; the page loads it from cdn.jsdelivr.net. Without network access the reader sees the diagram's source instead.

```html
<figure class="diagram"><pre class="mermaid">
flowchart LR
  A["omac start"] --> B["sandbox launcher"]
  B --> C["harness (claude)"]
  classDef changed stroke-width:3px
  class B changed
</pre><figcaption>What the diagram shows; the thick outline marks what this PR changes.</figcaption></figure>
```

- At most about 12 nodes; `flowchart` or `sequenceDiagram`.
- Quote node labels, and avoid `<`, `>` and `&` inside the diagram (reword instead).
- In `classDef`, set only `stroke-width` or `stroke-dasharray`, never colors, so the diagram works in light and dark mode.

## Writing

- Plain words, short sentences, concrete examples. If something fits in two sentences, use two.
- Explain jargon the first time it appears.
- Few em-dashes. No praise, no filler, no restating the diff line by line.
- Be fair to the PR: show the real strengths and weaknesses of its approach next to the others.
