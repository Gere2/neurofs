// app.js — vanilla client for the NeuroFS local UI.
// No framework. Tabs are show/hide; state is kept in module-level vars plus
// localStorage for the repo path. Every network call goes through j().

const state = {
  repo: localStorage.getItem("neurofs.repo") || "",
  lastBundlePath: null, // snapshot path from the last pack, if any
  lastPrompt: "",       // the bundle prompt returned by /api/pack
  lastPackStats: null,
  selectedRecords: [],
  mode: localStorage.getItem("neurofs.mode") || "build",
  templateDirty: false, // true when the user edited the template manually
  records: [],          // last list fetched from /api/records, pre-filter
  recordsFilter: "all", // "all" | "strategy" | "build" | "review" | "unknown"
  // Journal shares the same state.records store as the Records table —
  // one fetch feeds both views. Filter + search are kept separate so the
  // two tabs do not interfere with each other's state.
  journalFilter: "all",
  journalSearch: "",
  // Cache for full AuditRecord payloads keyed by path, populated as the
  // user expands cards. Keeps expand→collapse→expand instant and
  // survives re-renders triggered by filter/search keystrokes.
  journalFull: {},
  // task carries the human annotations (title + brief) captured at pack
  // time so the Response tab can pre-fill its own fields. Lives only in
  // memory — we deliberately don't persist it across page reloads: a new
  // page load usually means a new task, and stale briefs silently
  // attaching themselves to the next record would be a bad default.
  task: { title: "", brief: "" },
};

// ------------------------------ modes ------------------------------
//
// A "mode" is a small config bundle: preset defaults for the ranker/packer,
// an editable prompt template, and a short guide telling the user what to
// do with the output. Modes are pure UI sugar — nothing server-side changes.
// The template is concatenated in front of the bundle prompt when copying,
// so Claude sees the mode framing first, then the repo context.

const MODES = {
  strategy: {
    label: "Strategy",
    subtitle: "Decide the approach before writing code.",
    when: "You're starting an iteration and want a plan, not an implementation.",
    output: "A short technical design, key decisions, probable files, and a minimal test plan.",
    next: "Read the plan, agree on scope, then switch to Build for the actual change.",
    presets: {
      budget: 2200, maxFiles: 5, maxFragments: 10,
      changed: false, signatures: true,
    },
    slug: "strategy",
    template: [
      "You are helping me plan an iteration of a software project.",
      "Do NOT implement code in this turn. I want a plan first.",
      "",
      "Task:",
      "  <put the task description here; the user filled \"Question\" below with a short label>",
      "",
      "Please return, in this exact order:",
      "  1. Short technical design (under 200 words).",
      "  2. Key technical decisions and trade-offs.",
      "  3. Probable files to touch (cite from the bundle when possible).",
      "  4. Minimal test plan — what must hold true for this to be done.",
      "  5. Limitations and what we are explicitly NOT doing this iteration.",
      "  6. Suggested next iteration (one sentence).",
      "",
      "Constraints:",
      "  - No big re-architecture.",
      "  - No speculative abstractions.",
      "  - Stay inside the bundle: if something you need is missing, say so.",
      "",
      "---",
      "",
    ].join("\n"),
  },

  build: {
    label: "Build",
    subtitle: "Implement an iteration that is already defined.",
    when: "You already have a plan (from Strategy or from your own head) and want working code.",
    output: "A diff-shaped proposal: files to change, code, test notes, how to run it.",
    next: "Paste the response in the Response tab and run replay; if grounded/drift look good, apply the diff.",
    presets: {
      budget: 3500, maxFiles: 8, maxFragments: 16,
      changed: true, signatures: true,
    },
    slug: "build",
    template: [
      "You are helping me implement an already-defined iteration of a software project.",
      "The plan is below; execute it with the minimum code change that makes it work.",
      "",
      "Iteration to implement:",
      "  <paste the iteration spec here; the user filled \"Question\" below with a short label>",
      "",
      "Please return, in this exact order:",
      "  1. Short design note (1 paragraph) — only if the plan needs clarification.",
      "  2. Files to create/modify, in order.",
      "  3. Full code for each file (or the exact edit).",
      "  4. Minimum tests that prove the iteration works.",
      "  5. How to run / verify locally.",
      "  6. Limitations of this implementation.",
      "",
      "Constraints:",
      "  - The project must remain compilable / runnable after the change.",
      "  - Touch the minimum surface area. No unrelated refactors.",
      "  - Cite files from the bundle as `path:line` when quoting existing code.",
      "  - If the bundle is missing something you need, stop and say so.",
      "",
      "---",
      "",
    ].join("\n"),
  },

  review: {
    label: "Review",
    subtitle: "Evaluate a response, diff, or proposal before integrating.",
    when: "You have something (someone else's patch, a previous Claude answer, a refactor) and need a second read.",
    output: "A structured review: what is correct, what is risky, what looks hallucinated, what to do next.",
    next: "Apply the fixes you agree with, discard the rest, then run a Build iteration if needed.",
    presets: {
      budget: 2500, maxFiles: 6, maxFragments: 12,
      changed: true, signatures: true,
    },
    slug: "review",
    template: [
      "You are reviewing a change, response, or proposal against the code I gave you.",
      "Be specific, conservative, and grounded in the bundle.",
      "",
      "What to review:",
      "  <paste the diff, response, or proposal here; the user filled \"Question\" below with a short label>",
      "",
      "Please return, in this exact order:",
      "  1. Short summary of what the change does.",
      "  2. What looks correct or well-reasoned.",
      "  3. Risks — bugs, broken invariants, performance, security.",
      "  4. Likely hallucinations or unverified assumptions (flag them explicitly).",
      "  5. Concrete fixes or counter-proposals.",
      "  6. Suggested next step (most useful single action).",
      "",
      "Constraints:",
      "  - Cite from the bundle as `path:line` when pointing at existing code.",
      "  - If a claim in the change cannot be verified from the bundle, say so — do not guess.",
      "",
      "---",
      "",
    ].join("\n"),
  },
};

function applyMode(name, { preserveUserEdits = false } = {}) {
  const m = MODES[name];
  if (!m) return;
  state.mode = name;
  localStorage.setItem("neurofs.mode", name);

  // Pill selector visual state.
  document.querySelectorAll("#mode-pills button").forEach(b =>
    b.classList.toggle("active", b.dataset.mode === name));

  // Presets — always overwrite. Mode change = explicit intent.
  document.getElementById("q-budget").value = m.presets.budget;
  document.getElementById("q-maxfiles").value = m.presets.maxFiles;
  document.getElementById("q-maxfrags").value = m.presets.maxFragments;
  document.getElementById("q-changed").checked = m.presets.changed;
  document.getElementById("q-signatures").checked = m.presets.signatures;

  // Template — only overwrite if the user hasn't edited it manually.
  if (!preserveUserEdits || !state.templateDirty) {
    document.getElementById("q-template").value = m.template;
    state.templateDirty = false;
  }

  // Guide card.
  document.getElementById("mode-card").innerHTML = `
    <h3>${esc(m.label)} <span class="muted" style="font-weight:400">— ${esc(m.subtitle)}</span></h3>
    <dl class="kv">
      <dt>When to use</dt><dd>${esc(m.when)}</dd>
      <dt>Expected output</dt><dd>${esc(m.output)}</dd>
      <dt>Next step</dt><dd>${esc(m.next)}</dd>
    </dl>`;

  updateRunPreview();
  // Programmatic .value assignment does not fire "input", so refresh the
  // combined-prompt preview manually once the new template is in place.
  if (typeof refreshFullPrompt === "function") refreshFullPrompt();
}

// slugify turns "008 UI hardening!" → "008-ui-hardening". Minimal — we only
// need stable filenames, not a general i18n slugger.
function slugify(s) {
  return String(s).toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 60);
}

// updateRunPreview builds `<slug>-<mode>` and auto-fills the snapshot path if
// the user has not typed one manually. Explicitly skips overwriting when the
// user already customised q-snapshot — their edit wins.
function updateRunPreview() {
  const mode = state.mode;
  const slugInput = document.getElementById("q-slug").value.trim();
  const question = document.getElementById("q-input").value.trim();
  const base = slugInput ? slugify(slugInput) : slugify(question);
  const runName = base ? `${base}-${mode}` : `(untitled)-${mode}`;
  document.getElementById("run-preview").innerHTML =
    `Run name will be: <code>${esc(runName)}</code>`;

  const snap = document.getElementById("q-snapshot");
  if (!snap.dataset.touched && base) {
    snap.value = `audit/bundles/${runName}.json`;
  }
}

// ------------------------------ helpers ------------------------------

async function j(method, url, body) {
  const opts = { method, headers: { "Content-Type": "application/json" } };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const r = await fetch(url, opts);
  const text = await r.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
  if (!r.ok) throw new Error(data.error || text || `HTTP ${r.status}`);
  return data;
}

function fmtPct(x) { return (x * 100).toFixed(1) + "%"; }
function fmtDelta(x) {
  const sign = x > 0 ? "+" : "";
  const cls = x > 0.0001 ? "delta-pos" : x < -0.0001 ? "delta-neg" : "delta-zero";
  return `<span class="${cls}">${sign}${(x*100).toFixed(1)}</span>`;
}
function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));
}

// modeBadge renders a small pill for a record's mode. An empty mode (legacy
// record generated before iteration 9) gets a neutral "unknown" badge so the
// user can see at a glance which records pre-date the tracking.
function modeBadge(mode) {
  const m = (mode || "").toLowerCase();
  if (m === "strategy" || m === "build" || m === "review") {
    return `<span class="mode-badge mode-${m}">${m}</span>`;
  }
  return `<span class="mode-badge mode-unknown">unknown</span>`;
}
function requireRepo() {
  if (!state.repo) {
    alert("Set a repo path in the Workspace tab first.");
    return false;
  }
  return true;
}

// ------------------------------ tab nav ------------------------------

function switchTab(name) {
  document.querySelectorAll("nav#tabs button").forEach(b =>
    b.classList.toggle("active", b.dataset.tab === name));
  document.querySelectorAll(".tab").forEach(t =>
    t.classList.toggle("active", t.id === "tab-" + name));
  if (name === "home") renderHome();
  if (name === "workspace") renderWorkspace();
  if (name === "records") loadRecords();
  if (name === "journal") loadJournal();
}

document.querySelectorAll("nav#tabs button").forEach(b => {
  b.addEventListener("click", () => switchTab(b.dataset.tab));
});

// ------------------------------ mode wiring ------------------------------

document.querySelectorAll("#mode-pills button").forEach(b => {
  b.addEventListener("click", () => applyMode(b.dataset.mode));
});
document.getElementById("q-slug").addEventListener("input", updateRunPreview);
document.getElementById("q-input").addEventListener("input", updateRunPreview);
document.getElementById("q-snapshot").addEventListener("input", (e) => {
  // Flag as user-edited so updateRunPreview stops auto-filling it.
  e.target.dataset.touched = "1";
});
document.getElementById("q-template").addEventListener("input", () => {
  state.templateDirty = true;
});
document.getElementById("reset-mode").addEventListener("click", () => {
  state.templateDirty = false;
  applyMode(state.mode);
});

// ------------------------------ home ------------------------------

async function renderHome() {
  const body = document.getElementById("home-stats-body");
  if (!state.repo) {
    body.innerHTML = `<span class="muted">No workspace set. Open the Workspace tab.</span>`;
    return;
  }
  body.innerHTML = `<span class="muted">Loading stats for <code>${esc(state.repo)}</code>…</span>`;
  try {
    const s = await j("GET", `/api/stats?repo=${encodeURIComponent(state.repo)}`);
    body.innerHTML = renderStatsCard(s);
  } catch (e) {
    body.innerHTML = `<span class="muted">Could not read index: ${esc(e.message)}. Run <code>scan</code> from the Workspace tab.</span>`;
  }
}

function renderStatsCard(s) {
  const langRows = Object.entries(s.languages || {})
    .sort((a,b) => b[1]-a[1])
    .map(([k,v]) => `<div><span class="badge">${esc(k)}</span> ${v}</div>`).join("");
  let audit = "";
  if (s.audit && s.audit.records) {
    audit = `
      <div class="bucket">
        <h4>Audit aggregate (${s.audit.records} records)</h4>
        <div>grounded ${fmtPct(s.audit.grounded_ratio)} · drift ${fmtPct(s.audit.drift_rate)}${
          s.audit.answer_recall ? ` · recall ${fmtPct(s.audit.answer_recall)}` : ""
        }</div>
      </div>`;
  }
  return `
    <dl class="kv">
      <dt>repo</dt><dd>${esc(s.repo_root)}</dd>
      <dt>files</dt><dd>${s.files} indexed</dd>
      <dt>symbols</dt><dd>${s.symbols}</dd>
      <dt>imports</dt><dd>${s.imports}</dd>
      <dt>index size</dt><dd>${s.db_bytes} bytes</dd>
    </dl>
    <div class="row">${langRows || '<span class="muted">no language breakdown</span>'}</div>
    ${audit}`;
}

// ------------------------------ workspace ------------------------------

function renderWorkspace() {
  document.getElementById("repo-input").value = state.repo;
  document.getElementById("workspace-stats").innerHTML = "";
  if (state.repo) refreshWorkspaceStats();
}

document.getElementById("save-repo").addEventListener("click", () => {
  const v = document.getElementById("repo-input").value.trim();
  if (!v) { alert("Enter an absolute path."); return; }
  state.repo = v;
  localStorage.setItem("neurofs.repo", v);
  document.getElementById("scan-status").textContent = "Repo set. Run scan to index.";
  refreshWorkspaceStats();
});

document.getElementById("scan-btn").addEventListener("click", async () => {
  if (!requireRepo()) return;
  const btn = document.getElementById("scan-btn");
  const out = document.getElementById("scan-output");
  const status = document.getElementById("scan-status");
  btn.disabled = true; status.textContent = "scanning…"; out.textContent = "";
  try {
    const r = await j("POST", "/api/scan", {
      repo: state.repo,
      verbose: document.getElementById("scan-verbose").checked,
    });
    status.textContent = `done in ${r.summary.elapsed_ms}ms`;
    out.textContent = JSON.stringify(r.summary, null, 2);
    refreshWorkspaceStats();
  } catch (e) {
    status.textContent = "error";
    out.textContent = e.message;
  } finally {
    btn.disabled = false;
  }
});

async function refreshWorkspaceStats() {
  const el = document.getElementById("workspace-stats");
  el.innerHTML = `<span class="muted">loading…</span>`;
  try {
    const s = await j("GET", `/api/stats?repo=${encodeURIComponent(state.repo)}`);
    el.innerHTML = renderStatsCard(s);
  } catch (e) {
    el.innerHTML = `<span class="muted">${esc(e.message)}</span>`;
  }
}

// ------------------------------ new task ------------------------------

document.getElementById("pack-btn").addEventListener("click", async () => {
  if (!requireRepo()) return;
  const query = document.getElementById("q-input").value.trim();
  if (!query) { alert("Enter a question."); return; }
  const btn = document.getElementById("pack-btn");
  const status = document.getElementById("pack-status");
  btn.disabled = true; status.textContent = "packing…";

  // Capture the human annotations the moment we pack. Even if the user
  // wanders off and never comes back, the Response tab can prefill these
  // when it opens — they stick in state.task until the next pack.
  state.task.title = document.getElementById("q-title").value.trim();
  state.task.brief = document.getElementById("q-brief").value.trim();

  try {
    const r = await j("POST", "/api/pack", {
      repo: state.repo,
      query,
      budget: parseInt(document.getElementById("q-budget").value, 10) || 8000,
      focus: document.getElementById("q-focus").value.trim(),
      changed: document.getElementById("q-changed").checked,
      max_files: parseInt(document.getElementById("q-maxfiles").value, 10) || 0,
      max_fragments: parseInt(document.getElementById("q-maxfrags").value, 10) || 0,
      prefer_signatures: document.getElementById("q-signatures").checked,
      snapshot_name: document.getElementById("q-snapshot").value.trim(),
    });
    // The "full prompt" users copy is the mode template concatenated in
    // front of the bundle prompt. We keep the server response verbatim so
    // re-copying after edits picks up the latest template text.
    state.lastBundlePrompt = r.prompt;
    state.lastBundlePath = r.bundle_path || null;
    state.lastPackStats = r.stats;
    refreshFullPrompt();
    renderPackStats(r);
    ["copy-prompt", "download-prompt", "go-response"].forEach(id =>
      document.getElementById(id).disabled = false);
    status.textContent = `packed (${r.stats.tokens_used}/${r.stats.tokens_budget})`;
  } catch (e) {
    status.textContent = "error";
    document.getElementById("pack-stats").innerHTML =
      `<span class="muted">${esc(e.message)}</span>`;
  } finally {
    btn.disabled = false;
  }
});

function renderPackStats(r) {
  const s = r.stats;
  const frags = (r.fragments || []).map(f =>
    `<tr><td><code>${esc(f.rel_path)}</code></td><td>${esc(f.representation)}</td><td>${f.tokens}</td><td>${f.score.toFixed(2)}</td></tr>`
  ).join("");
  document.getElementById("pack-stats").innerHTML = `
    <h3>Bundle</h3>
    <dl class="kv">
      <dt>tokens</dt><dd>${s.tokens_used} / ${s.tokens_budget}</dd>
      <dt>files included</dt><dd>${s.files_included}</dd>
      <dt>compression</dt><dd>${s.compression_ratio ? s.compression_ratio.toFixed(2) + "×" : "—"}</dd>
      <dt>snapshot</dt><dd>${r.bundle_path ? `<code>${esc(r.bundle_path)}</code>` : '<span class="muted">not saved (no snapshot name given)</span>'}</dd>
    </dl>`;
  document.getElementById("pack-fragments").innerHTML = frags
    ? `<table class="records"><thead><tr><th>path</th><th>representation</th><th>tokens</th><th>score</th></tr></thead><tbody>${frags}</tbody></table>`
    : `<span class="muted">no fragments</span>`;
}

// refreshFullPrompt re-renders the preview pane with the current template
// concatenated in front of the bundle prompt. Invoked on pack success and
// on template edits so the preview never gets stale.
function refreshFullPrompt() {
  const tpl = document.getElementById("q-template").value;
  const bundle = state.lastBundlePrompt || "";
  state.lastPrompt = (tpl ? tpl.trimEnd() + "\n\n" : "") + bundle;
  document.getElementById("pack-prompt").textContent = state.lastPrompt;
}

document.getElementById("q-template").addEventListener("input", refreshFullPrompt);

document.getElementById("copy-prompt").addEventListener("click", async () => {
  refreshFullPrompt();
  try {
    await navigator.clipboard.writeText(state.lastPrompt);
    document.getElementById("pack-status").textContent = "copied (template + bundle)";
  } catch {
    document.getElementById("pack-status").textContent = "clipboard denied — use download";
  }
});

document.getElementById("download-prompt").addEventListener("click", () => {
  refreshFullPrompt();
  const runName = suggestRunName();
  const blob = new Blob([state.lastPrompt], { type: "text/plain" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = `${runName || "neurofs"}.prompt.txt`;
  a.click();
});

// suggestRunName mirrors the run-preview logic for the download filename.
function suggestRunName() {
  const slugInput = document.getElementById("q-slug").value.trim();
  const question = document.getElementById("q-input").value.trim();
  const base = slugInput ? slugify(slugInput) : slugify(question);
  if (!base) return "";
  return `${base}-${state.mode}`;
}

document.getElementById("go-response").addEventListener("click", () => {
  // Prefill Title/Brief on Response only when they are empty — users who
  // already typed a custom title there keep their edit. This matches the
  // "snapshot path touched" pattern used in New task.
  const rTitle = document.getElementById("r-title");
  const rBrief = document.getElementById("r-brief");
  if (!rTitle.value.trim() && state.task.title) rTitle.value = state.task.title;
  if (!rBrief.value.trim() && state.task.brief) rBrief.value = state.task.brief;
  switchTab("response");
});

// ------------------------------ response / replay ------------------------------

document.getElementById("r-bundle-source").addEventListener("change", (e) => {
  document.getElementById("r-snapshot-wrap").style.display =
    e.target.value === "snapshot" ? "block" : "none";
});

document.getElementById("replay-btn").addEventListener("click", async () => {
  if (!requireRepo()) return;
  const text = document.getElementById("r-text").value;
  if (!text.trim()) { alert("Paste the model response."); return; }

  const src = document.getElementById("r-bundle-source").value;
  let bundlePath = "";
  if (src === "snapshot") {
    bundlePath = document.getElementById("r-snapshot").value.trim();
    if (!bundlePath) { alert("Snapshot path is required in snapshot mode."); return; }
  } else {
    if (!state.lastBundlePath) {
      alert("No in-memory bundle. Either re-pack with a snapshot name, or switch bundle source to snapshot.");
      return;
    }
    bundlePath = state.lastBundlePath;
  }

  const btn = document.getElementById("replay-btn");
  const status = document.getElementById("replay-status");
  btn.disabled = true; status.textContent = "replaying…";
  try {
    const r = await j("POST", "/api/replay", {
      repo: state.repo,
      bundle_path: bundlePath,
      response: text,
      model: document.getElementById("r-model").value.trim() || "claude-manual",
      // Mode is the one set in the New task tab; the Response tab does not
      // have its own selector because response-for-bundle-X should inherit
      // the mode bundle-X was packed with. Sent as "" for legacy flows.
      mode: state.mode || "",
      facts: document.getElementById("r-facts").value.trim(),
      save: document.getElementById("r-save").checked,
      // Human annotations. Title/Brief may have been edited here, so we
      // read from the Response-tab inputs, not state.task. Server-side
      // they're trimmed and length-capped before being written to disk.
      title: document.getElementById("r-title").value.trim(),
      brief: document.getElementById("r-brief").value.trim(),
      note:  document.getElementById("r-note").value.trim(),
    });
    renderReplayReport(r);
    status.textContent = r.saved_path ? `saved: ${r.saved_path}` : "done";
  } catch (e) {
    document.getElementById("replay-report").innerHTML =
      `<span class="muted">${esc(e.message)}</span>`;
    status.textContent = "error";
  } finally {
    btn.disabled = false;
  }
});

function renderReplayReport(r) {
  const rec = r.record;
  const valid = (rec.citations || []).filter(c => c.valid).length;
  const total = (rec.citations || []).length;
  const driftClass = rec.drift.rate > 0.3 ? "bad" : rec.drift.rate > 0.1 ? "warn" : "good";
  const groundedClass = rec.grounded_ratio >= 0.8 ? "good" : rec.grounded_ratio >= 0.5 ? "warn" : "bad";

  const bucket = (label, items) => {
    if (!items || !items.length) return "";
    return `<div class="bucket"><h4>${label} (${items.length})</h4><ul>${
      items.slice(0, 10).map(s => `<li><code>${esc(s)}</code></li>`).join("")
    }${items.length > 10 ? `<li class="muted">… ${items.length-10} more</li>` : ""}</ul></div>`;
  };

  const invalid = (rec.citations || []).filter(c => !c.valid);
  const invalidList = invalid.length ? `
    <div class="bucket">
      <h4>Invalid citations (${invalid.length})</h4>
      <ul>${invalid.slice(0,5).map(c =>
        `<li><code>${esc(c.raw)}</code> — ${esc(c.reason)}</li>`).join("")}</ul>
    </div>` : "";

  // Annotations are shown at the top of the report as "what this run was
  // trying to do". Empty fields collapse to nothing so legacy + untitled
  // runs don't leak empty rows.
  const annRows = [
    rec.title ? `<dt>title</dt><dd>${esc(rec.title)}</dd>` : "",
    rec.brief ? `<dt>brief</dt><dd class="prose">${esc(rec.brief)}</dd>` : "",
    rec.note  ? `<dt>note</dt><dd class="prose">${esc(rec.note)}</dd>`   : "",
  ].join("");

  document.getElementById("replay-report").innerHTML = `
    <h3>Audit</h3>
    <dl class="kv">
      ${annRows}
      <dt>question</dt><dd>${esc(rec.question || "—")}</dd>
      <dt>mode</dt><dd>${modeBadge(rec.mode)}</dd>
      <dt>model</dt><dd>${esc(rec.model)}</dd>
      <dt>bundle hash</dt><dd><code>${esc((rec.bundle_hash || "").slice(0,16))}…</code></dd>
      <dt>grounded</dt><dd><span class="badge ${groundedClass}">${fmtPct(rec.grounded_ratio)}</span> ${valid}/${total} citations</dd>
      <dt>drift</dt><dd><span class="badge ${driftClass}">${fmtPct(rec.drift.rate)}</span> ${rec.drift.unknown_count} unknown of ${rec.drift.known_count + rec.drift.unknown_count}</dd>
      ${rec.expects_facts && rec.expects_facts.length ? `<dt>fact recall</dt><dd>${fmtPct(rec.answer_recall)} (${(rec.facts_hit||[]).length}/${rec.expects_facts.length})</dd>` : ""}
      ${r.saved_path ? `<dt>record</dt><dd><code>${esc(r.saved_path)}</code></dd>` : ""}
    </dl>
    ${bucket("unknown paths", rec.drift.unknown_paths)}
    ${bucket("unknown apis", rec.drift.unknown_apis)}
    ${bucket("unknown symbols", rec.drift.unknown_symbols)}
    ${invalidList}
  `;
}

// ------------------------------ records ------------------------------

document.getElementById("records-refresh").addEventListener("click", loadRecords);

async function loadRecords() {
  if (!state.repo) {
    document.getElementById("records-status").textContent = "Set a repo in the Workspace tab.";
    return;
  }
  const status = document.getElementById("records-status");
  status.textContent = "loading…";
  try {
    const r = await j("GET", `/api/records?repo=${encodeURIComponent(state.repo)}`);
    state.records = r.records || [];
    state.selectedRecords = [];
    renderRecords();
    status.textContent = `${state.records.length} records`;
  } catch (e) {
    status.textContent = "error: " + e.message;
  }
}

// renderContextCell composes the "Context" column of the Records table.
// Title is the headline; the question is the secondary line; brief and
// note render as small muted footnotes. Each annotation is already
// truncated server-side (see previewText in api.go) so the cell stays
// compact even if the originals were pages long. Full text ships in the
// Compare endpoint.
function renderContextCell(rec) {
  const parts = [];
  if (rec.title) {
    parts.push(`<div class="rec-title">${esc(rec.title)}</div>`);
    if (rec.question) {
      parts.push(`<div class="rec-question">${esc((rec.question || "").slice(0, 80))}</div>`);
    }
  } else {
    parts.push(`<div class="rec-question">${esc((rec.question || "—").slice(0, 80))}</div>`);
  }
  if (rec.brief) {
    parts.push(`<div class="rec-note" title="${esc(rec.brief)}"><span class="rec-note-tag">brief</span> ${esc(rec.brief)}</div>`);
  }
  if (rec.note) {
    parts.push(`<div class="rec-note" title="${esc(rec.note)}"><span class="rec-note-tag">note</span> ${esc(rec.note)}</div>`);
  }
  return parts.join("");
}

// renderRecords draws the table filtered by state.recordsFilter. Kept
// separate from loadRecords so the filter pills can re-render without
// re-fetching. Legacy records (mode="") match only the "all" and "unknown"
// filters so they remain visible rather than silently dropping.
function renderRecords() {
  const tbody = document.querySelector("#records-table tbody");
  const filter = state.recordsFilter;
  const all = state.records || [];
  const visible = all.filter(rec => {
    if (filter === "all") return true;
    const m = (rec.mode || "").toLowerCase();
    if (filter === "unknown") return !m;
    return m === filter;
  });

  if (!visible.length) {
    const msg = all.length
      ? `no records match filter "${esc(filter)}"`
      : `no records yet — run a replay and enable "persist"`;
    tbody.innerHTML = `<tr><td colspan="9" class="muted" style="padding:1rem;text-align:center">${msg}</td></tr>`;
  } else {
    tbody.innerHTML = visible.map(rec => `
      <tr>
        <td><input type="checkbox" data-path="${esc(rec.path)}" class="rec-check"
             ${state.selectedRecords.includes(rec.path) ? "checked" : ""}></td>
        <td>${esc(rec.timestamp)}</td>
        <td>${modeBadge(rec.mode)}</td>
        <td>${renderContextCell(rec)}</td>
        <td>${esc(rec.model)}</td>
        <td>${fmtPct(rec.grounded_ratio)}</td>
        <td>${fmtPct(rec.drift_rate)}</td>
        <td>${rec.expects_facts ? fmtPct(rec.answer_recall) : "—"}</td>
        <td><code>${esc((rec.bundle_hash || "").slice(0, 10))}</code></td>
      </tr>`).join("");
    document.querySelectorAll(".rec-check").forEach(c =>
      c.addEventListener("change", onRecSelect));
  }

  const countEl = document.getElementById("records-status");
  if (all.length) {
    countEl.textContent = filter === "all"
      ? `${all.length} records`
      : `${visible.length} / ${all.length} records (filter: ${filter})`;
  }
}

// Wire the filter pills once, at module load.
document.querySelectorAll("#records-filter button").forEach(b => {
  b.addEventListener("click", () => {
    state.recordsFilter = b.dataset.filter;
    document.querySelectorAll("#records-filter button").forEach(x =>
      x.classList.toggle("active", x.dataset.filter === state.recordsFilter));
    renderRecords();
  });
});

function onRecSelect() {
  const picked = Array.from(document.querySelectorAll(".rec-check"))
    .filter(c => c.checked).map(c => c.dataset.path);
  state.selectedRecords = picked;
  document.getElementById("records-selected").textContent =
    picked.length ? `${picked.length} selected` : "";
  document.getElementById("records-diff-btn").disabled = picked.length !== 2;
}

document.getElementById("records-diff-btn").addEventListener("click", () => {
  document.getElementById("cmp-a").value = state.selectedRecords[0] || "";
  document.getElementById("cmp-b").value = state.selectedRecords[1] || "";
  switchTab("compare");
});

// ------------------------------ journal ------------------------------
//
// Journal is the Records table re-imagined as a timeline. It reuses the
// same /api/records payload — no new backend — and the same in-memory
// state.records array, so opening Journal after Records is free.
//
// Grouping is by local calendar day. Chose "day" over "title" because:
//   - day is always present (every record has a timestamp), title is
//     often empty on legacy records, so title-grouping would leave most
//     cards in an "untitled" bucket,
//   - day matches how a developer remembers work ("what did I do on
//     Tuesday?") more naturally than "which label did I type",
//   - timestamps are already pre-formatted "YYYY-MM-DD HH:MM:SS" by the
//     server (local zone), so splitting on the space is trivial and
//     unambiguous.

document.querySelectorAll("#journal-filter button").forEach(b => {
  b.addEventListener("click", () => {
    state.journalFilter = b.dataset.filter;
    document.querySelectorAll("#journal-filter button").forEach(x =>
      x.classList.toggle("active", x.dataset.filter === state.journalFilter));
    renderJournal();
  });
});

document.getElementById("journal-search").addEventListener("input", (e) => {
  state.journalSearch = e.target.value.toLowerCase().trim();
  renderJournal();
});

// loadJournal fetches fresh records only when we don't already have them.
// The Records and Journal tabs deliberately share state.records so hopping
// between them feels instant. Clicking "Refresh" explicitly forces a
// re-fetch (see the refresh-button handler above, which lives on /records
// but is also what journal-refresh does — we route through loadRecords so
// both tabs update in sync).
async function loadJournal() {
  if (!state.repo) {
    document.getElementById("journal-status").textContent =
      "Set a repo in the Workspace tab.";
    document.getElementById("journal-body").innerHTML =
      `<div class="journal-empty">no workspace</div>`;
    return;
  }
  const status = document.getElementById("journal-status");
  // If we already have records in memory (e.g. Records was visited first)
  // render from that cache so tab switches feel instant. The Refresh
  // button always forces a new fetch.
  if (!state.records.length) {
    status.textContent = "loading…";
    try {
      const r = await j("GET", `/api/records?repo=${encodeURIComponent(state.repo)}`);
      state.records = r.records || [];
    } catch (e) {
      status.textContent = "error: " + e.message;
      document.getElementById("journal-body").innerHTML =
        `<div class="journal-empty">${esc(e.message)}</div>`;
      return;
    }
  }
  renderJournal();
}

// Explicit refresh: re-fetch via loadRecords so the Records table gets
// updated too. Cheap even on slow filesystems.
document.getElementById("journal-refresh").addEventListener("click", async () => {
  await loadRecords();
  renderJournal();
});

function journalMatches(rec) {
  const mode = (rec.mode || "").toLowerCase();
  const filter = state.journalFilter;
  if (filter !== "all") {
    if (filter === "unknown" && mode) return false;
    if (filter !== "unknown" && mode !== filter) return false;
  }
  const q = state.journalSearch;
  if (!q) return true;
  const hay = [
    rec.title || "",
    rec.brief || "",
    rec.note  || "",
    rec.question || "",
  ].join(" \u0001 ").toLowerCase();
  return hay.includes(q);
}

// renderJournal groups the filtered records by local date (YYYY-MM-DD)
// and emits one "day header" followed by its cards. state.records is
// already sorted newest-first by the server, so simply iterating in order
// preserves chronology without an extra sort.
function renderJournal() {
  const body = document.getElementById("journal-body");
  const status = document.getElementById("journal-status");
  const all = state.records || [];
  const visible = all.filter(journalMatches);

  if (!all.length) {
    status.textContent = "0 records";
    body.innerHTML = `<div class="journal-empty">No records yet. Run a replay and tick "persist".</div>`;
    return;
  }
  if (!visible.length) {
    status.textContent = `0 / ${all.length} (filtered out)`;
    body.innerHTML = `<div class="journal-empty">No records match the current filter.</div>`;
    return;
  }
  status.textContent = (state.journalFilter === "all" && !state.journalSearch)
    ? `${visible.length} records`
    : `${visible.length} / ${all.length} records`;

  // Group preserving order. Map keeps insertion order in JS.
  const byDay = new Map();
  for (const rec of visible) {
    const day = (rec.timestamp || "").slice(0, 10) || "unknown";
    if (!byDay.has(day)) byDay.set(day, []);
    byDay.get(day).push(rec);
  }

  const parts = [];
  for (const [day, recs] of byDay) {
    parts.push(`
      <div class="journal-day">
        <span class="journal-day-date">${esc(day)}</span>
        <span class="journal-day-rule"></span>
        <span class="journal-day-count">${recs.length} run${recs.length === 1 ? "" : "s"}</span>
      </div>`);
    for (const rec of recs) parts.push(renderJournalCard(rec));
  }
  body.innerHTML = parts.join("");
  wireJournalCardActions();
}

function renderJournalCard(rec) {
  const mode = (rec.mode || "").toLowerCase();
  const cardCls = ["journal-card"];
  if (mode === "strategy" || mode === "build" || mode === "review") {
    cardCls.push("mode-" + mode);
  }

  const headline = rec.title || rec.question || "(untitled)";
  const time = (rec.timestamp || "").slice(11, 16) || "—";

  const questionBlock = rec.title && rec.question
    ? `<div class="journal-question">${esc(rec.question)}</div>`
    : "";
  const briefBlock = rec.brief
    ? `<div class="journal-block"><span class="journal-block-tag">brief</span>${esc(rec.brief)}</div>`
    : "";
  const noteBlock = rec.note
    ? `<div class="journal-block note"><span class="journal-block-tag">note</span>${esc(rec.note)}</div>`
    : "";

  const driftClass = rec.drift_rate > 0.3 ? "bad"
    : rec.drift_rate > 0.1 ? "warn" : "good";
  const groundedClass = rec.grounded_ratio >= 0.8 ? "good"
    : rec.grounded_ratio >= 0.5 ? "warn" : "bad";
  const recallBadge = rec.expects_facts
    ? `<span class="badge">recall ${fmtPct(rec.answer_recall)}</span>`
    : "";

  // Path trimmed for display; the data-path attr carries the full path so
  // the actions still wire correctly.
  const displayPath = (rec.path || "").split("/").slice(-2).join("/");

  return `
    <div class="${cardCls.join(" ")}" data-path="${esc(rec.path)}">
      <div class="journal-head">
        <span class="journal-title">${esc(headline)}</span>
        ${modeBadge(rec.mode)}
        <span class="journal-time">${esc(time)}</span>
      </div>
      ${questionBlock}
      ${briefBlock}
      ${noteBlock}
      <div class="journal-metrics">
        <span class="badge ${groundedClass}">grounded ${fmtPct(rec.grounded_ratio)}</span>
        <span class="badge ${driftClass}">drift ${fmtPct(rec.drift_rate)}</span>
        ${recallBadge}
      </div>
      <div class="journal-actions">
        <button data-action="expand">Expand</button>
        <button data-action="cmp-a">Use as Compare A</button>
        <button data-action="cmp-b">Use as Compare B</button>
        <button data-action="copy">Copy path</button>
        <span class="path" title="${esc(rec.path)}">${esc(displayPath)}</span>
      </div>
      <div class="journal-expand" hidden></div>
    </div>`;
}

// wireJournalCardActions binds the action buttons on every card.
// Delegating on the body once would be lighter, but we re-render after
// every filter keystroke, so rebinding is cheap and keeps the code local.
function wireJournalCardActions() {
  document.querySelectorAll(".journal-card").forEach(card => {
    const path = card.dataset.path;
    card.querySelectorAll("[data-action]").forEach(btn => {
      btn.addEventListener("click", () => {
        const act = btn.dataset.action;
        if (act === "cmp-a") {
          document.getElementById("cmp-a").value = path;
          switchTab("compare");
        } else if (act === "cmp-b") {
          document.getElementById("cmp-b").value = path;
          switchTab("compare");
        } else if (act === "copy") {
          navigator.clipboard.writeText(path).then(
            () => flashButton(btn, "copied"),
            () => flashButton(btn, "copy failed"),
          );
        } else if (act === "expand") {
          toggleJournalExpand(card, btn);
        }
      });
    });
  });
}

// toggleJournalExpand opens/closes the expanded panel of a card. The full
// AuditRecord is cached in state.journalFull keyed by path, so an expand
// → collapse → expand sequence costs exactly one network round-trip.
// Collapse just hides the panel — we don't destroy it — so a later
// re-expand paints instantly from the DOM it already built.
async function toggleJournalExpand(card, btn) {
  const panel = card.querySelector(".journal-expand");
  if (!panel) return;
  const path = card.dataset.path;

  if (!panel.hidden) {
    panel.hidden = true;
    btn.textContent = "Expand";
    return;
  }

  // If we've already fetched this record, reuse the cached DOM.
  if (panel.dataset.loaded === "1") {
    panel.hidden = false;
    btn.textContent = "Collapse";
    return;
  }

  btn.disabled = true;
  btn.textContent = "loading…";
  try {
    let full = state.journalFull[path];
    if (!full) {
      const url = `/api/record?repo=${encodeURIComponent(state.repo)}&path=${encodeURIComponent(path)}`;
      full = await j("GET", url);
      state.journalFull[path] = full;
    }
    panel.innerHTML = renderJournalExpand(full);
    panel.dataset.loaded = "1";
    panel.hidden = false;
    btn.textContent = "Collapse";
  } catch (e) {
    panel.innerHTML = `<div class="muted">could not load record: ${esc(e.message)}</div>`;
    panel.hidden = false;
    btn.textContent = "Expand";
  } finally {
    btn.disabled = false;
  }
}

// renderJournalExpand is the read-only detailed view of an AuditRecord.
// It never mutates state — just shows the full text and the three drift
// buckets alongside the metrics. Legacy records (no mode/title/brief/
// note) render cleanly: every section that has no content collapses.
function renderJournalExpand(rec) {
  const driftClass = rec.drift && rec.drift.rate > 0.3 ? "bad"
    : rec.drift && rec.drift.rate > 0.1 ? "warn" : "good";
  const groundedClass = rec.grounded_ratio >= 0.8 ? "good"
    : rec.grounded_ratio >= 0.5 ? "warn" : "bad";
  const recallRow = rec.expects_facts && rec.expects_facts.length
    ? `<dt>fact recall</dt><dd>${fmtPct(rec.answer_recall || 0)} (${(rec.facts_hit||[]).length}/${rec.expects_facts.length})</dd>`
    : "";

  const bucket = (label, items) => {
    if (!items || !items.length) return "";
    // Cap the list at 20 entries — more than that is almost always noise
    // and would make the panel scroll for no practical gain.
    const shown = items.slice(0, 20).map(s => `<li><code>${esc(s)}</code></li>`).join("");
    const tail = items.length > 20 ? `<li class="muted">… ${items.length - 20} more</li>` : "";
    return `<div class="bucket"><h4>${label} (${items.length})</h4><ul>${shown}${tail}</ul></div>`;
  };

  const block = (label, text) => text
    ? `<div class="journal-block"><span class="journal-block-tag">${label}</span>${esc(text)}</div>`
    : "";

  const drift = rec.drift || {};
  const ts = rec.timestamp
    ? new Date(rec.timestamp).toLocaleString()
    : "—";

  return `
    <dl class="kv">
      ${rec.title ? `<dt>title</dt><dd>${esc(rec.title)}</dd>` : ""}
      <dt>mode</dt><dd>${modeBadge(rec.mode)}</dd>
      <dt>when</dt><dd>${esc(ts)}</dd>
      <dt>model</dt><dd>${esc(rec.model || "—")}</dd>
      <dt>question</dt><dd class="prose">${esc(rec.question || "—")}</dd>
      <dt>grounded</dt><dd><span class="badge ${groundedClass}">${fmtPct(rec.grounded_ratio || 0)}</span></dd>
      <dt>drift</dt><dd><span class="badge ${driftClass}">${fmtPct(drift.rate || 0)}</span> ${drift.unknown_count || 0} unknown of ${((drift.known_count||0) + (drift.unknown_count||0))}</dd>
      ${recallRow}
      <dt>bundle</dt><dd><code>${esc((rec.bundle_hash || "").slice(0, 16))}…</code></dd>
    </dl>
    ${block("brief", rec.brief)}
    ${block("note",  rec.note)}
    ${bucket("unknown paths",   drift.unknown_paths)}
    ${bucket("unknown apis",    drift.unknown_apis)}
    ${bucket("unknown symbols", drift.unknown_symbols)}
  `;
}

// flashButton shows a transient confirmation in the button itself. Used
// instead of a toast because we have no toast system and a one-off label
// flip is enough signal for a local tool.
function flashButton(btn, msg) {
  const prev = btn.textContent;
  btn.textContent = msg;
  btn.disabled = true;
  setTimeout(() => { btn.textContent = prev; btn.disabled = false; }, 900);
}

// ------------------------------ compare ------------------------------

document.getElementById("compare-btn").addEventListener("click", async () => {
  const a = document.getElementById("cmp-a").value.trim();
  const b = document.getElementById("cmp-b").value.trim();
  if (!a || !b) { alert("Need both record paths."); return; }
  const status = document.getElementById("compare-status");
  status.textContent = "diffing…";
  try {
    const d = await j("POST", "/api/diff", { a, b });
    renderDiff(d);
    status.textContent = "";
  } catch (e) {
    document.getElementById("compare-report").innerHTML =
      `<span class="muted">${esc(e.message)}</span>`;
    status.textContent = "error";
  }
});

function renderDiff(d) {
  const bucket = (label, sd) => {
    if ((!sd.added || !sd.added.length) && (!sd.removed || !sd.removed.length)) return "";
    return `<div class="bucket">
      <h4>${label}</h4>
      ${(sd.added||[]).map(s => `<div class="added">+ <code>${esc(s)}</code></div>`).join("")}
      ${(sd.removed||[]).map(s => `<div class="removed">− <code>${esc(s)}</code></div>`).join("")}
    </div>`;
  };

  // Mode row is always rendered so the user sees the intent behind each
  // record even when SameMode is true. Legacy records (mode="") render as
  // "unknown" — same convention as the Records table.
  const modeRow = `
    <dt>mode</dt>
    <dd>
      A: ${modeBadge(d.mode_a)} &nbsp;→&nbsp; B: ${modeBadge(d.mode_b)}
      ${d.same_mode ? "" : ` <span class="muted">(different — interpret deltas with care)</span>`}
    </dd>`;

  // annotationPair renders a "A: … / B: …" row, hiding the whole row when
  // both sides are empty so legacy-vs-legacy diffs don't render phantom
  // "title: A: — / B: —" entries that carry zero information.
  const annotationPair = (label, a, b) => {
    if (!a && !b) return "";
    const fmt = v => v ? `<span class="prose">${esc(v)}</span>` : `<span class="muted">—</span>`;
    return `
      <dt>${label}</dt>
      <dd>
        <div><span class="muted">A:</span> ${fmt(a)}</div>
        <div><span class="muted">B:</span> ${fmt(b)}</div>
      </dd>`;
  };

  document.getElementById("compare-report").innerHTML = `
    <h3>Diff</h3>
    <dl class="kv">
      ${annotationPair("title", d.title_a, d.title_b)}
      ${annotationPair("brief", d.brief_a, d.brief_b)}
      ${annotationPair("note",  d.note_a,  d.note_b)}
      <dt>same bundle</dt><dd>${d.same_bundle ? "yes" : "<span class=\"delta-neg\">no</span>"}</dd>
      <dt>same question</dt><dd>${d.same_question ? "yes" : "<span class=\"delta-neg\">no</span>"}</dd>
      <dt>same model</dt><dd>${d.same_model ? "yes" : "<span class=\"delta-neg\">no</span>"}</dd>
      ${modeRow}
      <dt>grounded Δ</dt><dd>${fmtDelta(d.grounded_delta)}</dd>
      <dt>drift Δ</dt><dd>${fmtDelta(d.drift_delta)}</dd>
      ${d.recall_applies ? `<dt>recall Δ</dt><dd>${fmtDelta(d.recall_delta)}</dd>` : ""}
    </dl>
    ${bucket("paths", d.paths || {})}
    ${bucket("apis", d.apis || {})}
    ${bucket("symbols", d.symbols || {})}
  `;
}

// ------------------------------ init ------------------------------

applyMode(state.mode);
switchTab("home");
