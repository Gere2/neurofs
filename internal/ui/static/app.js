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
  // Global Search (iteration 14). searchResults is null before the first
  // query so we can show a friendly "type something" placeholder
  // instead of a misleading "no results" state. searchScope is
  // "all" | "metadata" | "paths" | "content"; searchMode uses the
  // same vocabulary as the Journal mode filter.
  searchQuery: "",
  searchScope: "all",
  searchMode: "all",
  searchResults: null,
  searchTotalMatches: 0,
  // When the user clicks "Open in Journal" from a Search result, we
  // stash the path here so renderJournal (next render) can find the
  // card, scroll it into view, expand it, and flash it.
  pendingJournalFocus: null,
  // task carries the human annotations (title + brief) captured at pack
  // time so the Response tab can pre-fill its own fields. Lives only in
  // memory — we deliberately don't persist it across page reloads: a new
  // page load usually means a new task, and stale briefs silently
  // attaching themselves to the next record would be a bad default.
  task: { title: "", brief: "" },
  // Parent-context reuse. null when starting from scratch; an object
  // { parentPath, parentTitle, focusPaths[] } after the user clicks
  // "Resume from this" on a Journal card. In-memory only — page reloads
  // reset to a clean slate so a stale parent can't silently attach
  // itself to the next run.
  resume: null,
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

// data-tab-target buttons (landing page CTAs) jump to a tab by name.
// Kept as a single delegate so any future in-UI call-to-action can reuse it.
document.querySelectorAll("[data-tab-target]").forEach(b => {
  b.addEventListener("click", () => switchTab(b.dataset.tabTarget));
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
  // The landing itself is static copy. We only surface a thin status line
  // at the bottom when a workspace is already configured — just enough so
  // returning users know which repo they're operating on without turning
  // the hero back into a dashboard.
  const wrap = document.getElementById("home-stats");
  const body = document.getElementById("home-stats-body");
  if (!wrap || !body) return;
  if (!state.repo) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;
  body.textContent = `${t("home.workingOn")} ${state.repo}`;
  try {
    const s = await j("GET", `/api/stats?repo=${encodeURIComponent(state.repo)}`);
    if (s && typeof s.files === "number") {
      body.textContent = `${t("home.workingOn")} ${s.repo_root || state.repo} · ${s.files} ${t("home.filesIndexedSuffix")}`;
    }
  } catch {
    body.textContent = `${t("home.workingOn")} ${state.repo} · ${t("home.indexNotReady")}`;
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
        <h4>${t("workspace.auditAggregate")} (${s.audit.records} ${t("workspace.recordsSuffix")})</h4>
        <div>${t("audit.grounded")} ${fmtPct(s.audit.grounded_ratio)} · ${t("audit.drift")} ${fmtPct(s.audit.drift_rate)}${
          s.audit.answer_recall ? ` · ${t("workspace.recall")} ${fmtPct(s.audit.answer_recall)}` : ""
        }</div>
      </div>`;
  }
  return `
    <dl class="kv">
      <dt>${t("workspace.repoKv")}</dt><dd>${esc(s.repo_root)}</dd>
      <dt>${t("workspace.files")}</dt><dd>${s.files} ${t("workspace.indexedSuffix")}</dd>
      <dt>${t("workspace.symbols")}</dt><dd>${s.symbols}</dd>
      <dt>${t("workspace.imports")}</dt><dd>${s.imports}</dd>
      <dt>${t("workspace.indexSize")}</dt><dd>${s.db_bytes} ${t("workspace.bytes")}</dd>
    </dl>
    <div class="row">${langRows || `<span class="muted">${t("workspace.langBreakdown")}</span>`}</div>
    ${audit}`;
}

// ------------------------------ workspace ------------------------------

function renderWorkspace() {
  document.getElementById("repo-input").value = state.repo;
  document.getElementById("workspace-stats").innerHTML = "";
  if (state.repo) refreshWorkspaceStats();
}

document.getElementById("save-repo").addEventListener("click", async () => {
  const raw = document.getElementById("repo-input").value.trim();
  if (!raw) { alert(t("alert.enterPath")); return; }
  // Tilde and non-absolute paths would be rejected by the server as
  // "repo root does not exist"; catch them up front so the user gets
  // the actionable message instead of the generic one.
  if (raw.startsWith("~")) { alert(t("alert.tilde")); return; }
  if (!raw.startsWith("/")) { alert(t("alert.absolute")); return; }
  state.repo = raw;
  localStorage.setItem("neurofs.repo", raw);
  document.getElementById("repo-input").value = raw;
  // Success/failure line is set only after refreshWorkspaceStats verifies
  // the path — otherwise "repo set" would appear next to an error card.
  const status = document.getElementById("scan-status");
  status.textContent = t("common.checking");
  const ok = await refreshWorkspaceStats();
  status.textContent = ok ? t("home.repoSet") : t("home.repoRejected");
});

document.getElementById("scan-btn").addEventListener("click", async () => {
  if (!requireRepo()) return;
  const btn = document.getElementById("scan-btn");
  const out = document.getElementById("scan-output");
  const status = document.getElementById("scan-status");
  btn.disabled = true; status.textContent = t("common.scanning"); out.textContent = "";
  try {
    const r = await j("POST", "/api/scan", {
      repo: state.repo,
      verbose: document.getElementById("scan-verbose").checked,
    });
    status.textContent = `${t("common.doneIn")} ${r.summary.elapsed_ms}${t("common.ms")}`;
    out.textContent = JSON.stringify(r.summary, null, 2);
    refreshWorkspaceStats();
  } catch (e) {
    status.textContent = t("common.error");
    out.textContent = e.message;
  } finally {
    btn.disabled = false;
  }
});

async function refreshWorkspaceStats() {
  const el = document.getElementById("workspace-stats");
  el.innerHTML = `<span class="muted">${t("common.loading")}</span>`;
  try {
    const s = await j("GET", `/api/stats?repo=${encodeURIComponent(state.repo)}`);
    el.innerHTML = renderStatsCard(s);
    return true;
  } catch (e) {
    el.innerHTML = `<span class="error">${esc(e.message)}</span>`;
    return false;
  }
}

// ------------------------------ new task ------------------------------

// One-shot generator: same flow as `neurofs task <q>` from the CLI. The
// server auto-scans on first use, caches the prompt by (query, budget),
// and returns the Claude-shaped text plus a small TopPicks list for
// transparency. Deliberately decoupled from the full Pack form below
// so a beginner can hit Generate without learning every flag.
document.getElementById("oneshot-btn").addEventListener("click", async () => {
  if (!requireRepo()) return;
  const query = document.getElementById("oneshot-q").value.trim();
  if (!query) { alert(t("alert.enterQuestion") || "Enter a question."); return; }
  const btn = document.getElementById("oneshot-btn");
  const status = document.getElementById("oneshot-status");
  const out = document.getElementById("oneshot-result");
  btn.disabled = true;
  status.textContent = t("oneshot.running");
  out.hidden = true;
  try {
    const r = await j("POST", "/api/task", {
      repo: state.repo,
      query,
      budget: parseInt(document.getElementById("q-budget").value, 10) || 0,
      force: document.getElementById("oneshot-force").checked,
    });
    renderOneshotResult(r);
    out.hidden = false;
    status.textContent = r.reused ? t("oneshot.cached") : t("oneshot.fresh");
  } catch (e) {
    status.textContent = t("common.error");
    out.hidden = false;
    out.innerHTML = `<span class="muted">${esc(e.message)}</span>`;
  } finally {
    btn.disabled = false;
  }
});

function renderOneshotResult(r) {
  const out = document.getElementById("oneshot-result");
  const stats = r.stats || {};
  const used = stats.tokens_used || 0;
  const ratio = stats.compression_ratio || 0;
  const rawEstimate = ratio > 0 ? Math.round(used * ratio) : 0;
  const saved = rawEstimate > used ? rawEstimate - used : 0;
  const headline = saved >= 500
    ? `<div class="pack-savings-big">${t("pack.savedBig").replace("{n}", fmtTokens(saved))}</div>`
    : `<div class="pack-savings-big">${t("pack.ready")}</div>`;

  const picks = (r.top_picks || []).map(p =>
    `<li><code>${esc(p.rel_path)}</code> <span class="muted">· ${p.tokens} tok · ${esc(p.representation)}</span></li>`
  ).join("");
  const picksBlock = picks
    ? `<details class="pack-details" open><summary>${t("oneshot.topPicks")}</summary><ul class="oneshot-picks">${picks}</ul></details>`
    : "";

  const noteCached = r.reused
    ? `<span class="muted">· ${t("oneshot.cachedNote")}</span>`
    : "";
  const noteScanned = r.auto_scanned
    ? `<span class="muted">· ${t("oneshot.autoScanned")}</span>`
    : "";

  out.innerHTML = `
    <div class="pack-savings">${headline}</div>
    <div class="pack-meta muted">
      ${stats.files_included || 0} ${(stats.files_included === 1) ? t("pack.fileOne") : t("pack.fileMany")} ·
      ${used} / ${stats.tokens_budget || 0} tok
      ${noteCached} ${noteScanned}
    </div>
    <div class="row">
      <button id="oneshot-copy" class="primary" data-i18n="oneshot.copy">Copy prompt</button>
      <button id="oneshot-download" data-i18n="oneshot.download">Download .prompt.txt</button>
      ${r.prompt_path ? `<span class="muted"><code>${esc(r.prompt_path)}</code></span>` : ""}
    </div>
    ${picksBlock}
    <details class="pack-details">
      <summary>${t("oneshot.previewPrompt")}</summary>
      <pre class="log tall">${esc(r.prompt || "")}</pre>
    </details>`;
  // Re-apply i18n labels to the freshly-injected buttons so a language
  // switch before/after Generate doesn't leave English text in an ES UI.
  applyLang(landingReadLang());

  document.getElementById("oneshot-copy").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(r.prompt || "");
      document.getElementById("oneshot-status").textContent = t("oneshot.copied");
    } catch {
      document.getElementById("oneshot-status").textContent = t("oneshot.clipboardDenied");
    }
  });
  document.getElementById("oneshot-download").addEventListener("click", () => {
    const blob = new Blob([r.prompt || ""], { type: "text/plain" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `${(r.query || "task").replace(/[^a-z0-9]+/gi, "-").slice(0, 40) || "task"}.prompt.txt`;
    a.click();
    URL.revokeObjectURL(a.href);
  });
}

document.getElementById("oneshot-q").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    document.getElementById("oneshot-btn").click();
  }
});

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

  // When resuming, read the freshly-edited focus textarea rather than
  // state.resume.focusPaths — the user may have pruned entries since
  // the seed was applied, and the record should reflect what actually
  // ran.
  const inheritedFocus = state.resume ? readResumeFocusPaths() : [];
  if (state.resume) state.resume.focusPaths = inheritedFocus;

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
      inherited_focus: inheritedFocus,
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
  const used = s.tokens_used || 0;
  const ratio = s.compression_ratio || 0;
  const rawEstimate = ratio > 0 ? Math.round(used * ratio) : 0;
  const saved = rawEstimate > used ? rawEstimate - used : 0;
  const files = s.files_included || 0;

  // Headline: turn compression_ratio into plain language so the user sees
  // the value right after Pack. Threshold (~500 tokens) keeps us from
  // bragging about negligible savings on tiny repos — there we fall back
  // to a neutral confirmation. The benefit line is identical in both
  // branches because it describes the product, not this particular run.
  const savingIsMeaningful = saved >= 500;
  const headline = savingIsMeaningful
    ? `<div class="pack-savings-big">${t("pack.savedBig").replace("{n}", fmtTokens(saved))}</div>`
    : `<div class="pack-savings-big">${t("pack.ready")}</div>`;
  const benefit = `<div class="pack-savings-sub">${t("pack.benefit")}</div>`;

  const filesLabel = files === 1 ? t("pack.fileOne") : t("pack.fileMany");
  const meta = `
    <div class="pack-meta muted">
      ${files} ${filesLabel} · ${t("pack.readyToPaste")}${
        r.bundle_path ? ` · ${t("pack.savedAs")} <code>${esc(r.bundle_path)}</code>` : ""
      }
    </div>`;

  document.getElementById("pack-stats").innerHTML = `
    <h3>${t("pack.headline")}</h3>
    <div class="pack-savings">${headline}${benefit}</div>
    ${meta}
    <details class="pack-details">
      <summary>${t("pack.technical")}</summary>
      <dl class="kv">
        <dt>tokens</dt><dd>${s.tokens_used} / ${s.tokens_budget}</dd>
        <dt>files included</dt><dd>${s.files_included}</dd>
        <dt>compression</dt><dd>${s.compression_ratio ? s.compression_ratio.toFixed(2) + "×" : "—"}</dd>
        <dt>snapshot</dt><dd>${r.bundle_path ? `<code>${esc(r.bundle_path)}</code>` : '<span class="muted">not saved (no snapshot name given)</span>'}</dd>
      </dl>
    </details>`;

  const frags = (r.fragments || []).map(f =>
    `<tr><td><code>${esc(f.rel_path)}</code></td><td>${esc(f.representation)}</td><td>${f.tokens}</td><td>${f.score.toFixed(2)}</td></tr>`
  ).join("");
  document.getElementById("pack-fragments").innerHTML = frags
    ? `<table class="records"><thead><tr><th>path</th><th>representation</th><th>tokens</th><th>score</th></tr></thead><tbody>${frags}</tbody></table>`
    : `<span class="muted">no fragments</span>`;
}

// fmtTokens prints an approximate token count in a form a human can grok
// at a glance: "24k" for ten-thousands, "2.4k" for thousands, bare numbers
// with thousands separators below that. Rounding is deliberate — exact-
// looking counts on estimates invite false precision.
function fmtTokens(n) {
  n = Math.max(0, Math.round(n));
  if (n >= 10000) return Math.round(n / 1000) + "k";
  if (n >= 1000) return (n / 1000).toFixed(1) + "k";
  return n.toLocaleString();
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
      // Parent linkage. When resuming, send both fields so the record
      // persists the breadcrumb + the exact focus list that ran.
      parent_record:   state.resume ? state.resume.parentPath : "",
      inherited_focus: state.resume ? (state.resume.focusPaths || []) : [],
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
  // Parent breadcrumb: shown only when this record was resumed from
  // another. Click delegates to focusJournalCard, which silently no-ops
  // if the parent isn't currently in the DOM (filtered / deleted) — the
  // static parent_title still tells the user where this came from.
  const parentBlock = rec.parent_record
    ? `<div class="journal-parent">
         <button type="button" data-action="goto-parent"
                 data-parent-path="${esc(rec.parent_record)}"
                 title="${esc(t("journal.gotoParentTitle"))}">
           ↳ ${esc(t("journal.continuesFrom"))} ${esc(rec.parent_title || rec.parent_record.split("/").pop() || "parent")}
         </button>
       </div>`
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
      ${parentBlock}
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
        <button data-action="resume">${t("journal.resumeFromThis")}</button>
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
        } else if (act === "resume") {
          resumeFromRecord(path);
        } else if (act === "goto-parent") {
          // focusJournalCard no-ops when the target isn't in the current
          // DOM (filtered out / deleted / different run). The static
          // breadcrumb label still tells the user where this came from,
          // so a silent no-op is acceptable — flash the button so the
          // click visibly registered.
          const parentPath = btn.dataset.parentPath || "";
          if (parentPath) {
            flashButton(btn, "jump");
            focusJournalCard(parentPath);
          }
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

  if (!panel.hidden) {
    panel.hidden = true;
    btn.textContent = "Expand";
    return;
  }

  await ensureJournalCardExpanded(card, btn);
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
    ${renderJournalFragments(rec.fragments)}
  `;
}

// renderJournalFragments lists every AuditFragment in the record as a
// nested <details>. The outer <details> is collapsed by default so the
// expand panel stays compact — you opt in to inspecting what the model
// actually saw. Native <details> means no extra JS wiring, and the
// browser handles toggle state per-node for free.
//
// Legacy records where Fragments is null/empty show a single muted line
// instead of an empty block, so the user knows the record predates
// fragment persistence rather than thinking the bundle was empty.
// Fragments with a missing `content` (possible if a very old record
// was written before content was kept) show a similar explanation in
// the body slot — metadata is still visible.
function renderJournalFragments(frags) {
  if (!frags || !frags.length) {
    return `<div class="fragments-block">
      <div class="fragments-empty">no fragments persisted in this record</div>
    </div>`;
  }

  const total = frags.reduce((n, f) => n + (f.tokens || 0), 0);
  const items = frags.map((f, i) => renderJournalFragment(f, i)).join("");

  return `<details class="fragments-block">
    <summary>Bundle fragments (${frags.length}) · ${total} tokens</summary>
    <div class="fragments-list">${items}</div>
  </details>`;
}

// renderJournalFragment renders one fragment as a nested <details>. The
// summary line carries the three metadata columns (rel_path ·
// representation · tokens) with the lang as a dim tail so the row reads
// at a glance. The body is a <pre class="log"> with the escaped content
// — the same style as the rest of the app's log blocks so it inherits
// the max-height scroll and mono font.
function renderJournalFragment(f, i) {
  const path = f.rel_path || `fragment-${i}`;
  const rep  = f.representation || "—";
  const lang = f.lang ? `<span class="frag-lang">${esc(f.lang)}</span>` : "";
  const tokens = f.tokens || 0;

  const body = (f.content && f.content.length)
    ? `<pre class="log tall">${esc(f.content)}</pre>`
    : `<div class="muted fragments-empty">no content persisted for this fragment — only metadata is available</div>`;

  return `<details class="fragment" data-rel-path="${esc(path)}">
    <summary>
      <code class="frag-path">${esc(path)}</code>
      <span class="frag-rep">${esc(rep)}</span>
      ${lang}
      <span class="frag-tokens">${tokens} tok</span>
    </summary>
    ${body}
  </details>`;
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

// ------------------------------ search ------------------------------
//
// Global substring search across every audit record. The backend does
// the walking; this side wires the input, scope/mode pills, renders
// results, and handles navigation back into Journal.

document.getElementById("search-btn").addEventListener("click", runSearch);

// Enter in the input triggers search. We don't debounce per-keystroke
// because the server walks the filesystem; one click / one Enter / one
// query keeps the behaviour obvious.
document.getElementById("search-q").addEventListener("keydown", (ev) => {
  if (ev.key === "Enter") {
    ev.preventDefault();
    runSearch();
  } else if (ev.key === "Escape") {
    ev.target.value = "";
  }
});

// Scope pills: all | metadata | paths | content. Switching the pill
// does NOT auto-re-run the query — the user may have been typing a
// different term. If results already exist, re-run so the UI matches
// the pill state.
document.querySelectorAll("#search-scope button").forEach(b => {
  b.addEventListener("click", () => {
    document.querySelectorAll("#search-scope button")
      .forEach(x => x.classList.toggle("active", x === b));
    state.searchScope = b.dataset.scope;
    if (state.searchResults !== null) runSearch();
  });
});

document.querySelectorAll("#search-mode button").forEach(b => {
  b.addEventListener("click", () => {
    document.querySelectorAll("#search-mode button")
      .forEach(x => x.classList.toggle("active", x === b));
    state.searchMode = b.dataset.filter;
    if (state.searchResults !== null) runSearch();
  });
});

async function runSearch() {
  const q = document.getElementById("search-q").value.trim();
  const status = document.getElementById("search-status");
  if (!state.repo) {
    status.textContent = "Set a repo in the Workspace tab.";
    return;
  }
  if (!q) {
    state.searchResults = null;
    state.searchTotalMatches = 0;
    status.textContent = "";
    renderSearchResults();
    return;
  }
  state.searchQuery = q;
  status.textContent = "searching…";
  try {
    const url = `/api/search?repo=${encodeURIComponent(state.repo)}`
      + `&q=${encodeURIComponent(q)}`
      + `&scope=${encodeURIComponent(state.searchScope)}`
      + `&mode=${encodeURIComponent(state.searchMode)}`;
    const r = await j("GET", url);
    state.searchResults = r.results || [];
    state.searchTotalMatches = r.total_matches || state.searchResults.length;
    const shown = state.searchResults.length;
    const total = state.searchTotalMatches;
    status.textContent = total === 0
      ? "no matches"
      : shown < total
        ? `${shown} of ${total} matches shown`
        : `${shown} match${shown === 1 ? "" : "es"}`;
    renderSearchResults();
  } catch (e) {
    status.textContent = "error: " + e.message;
    state.searchResults = [];
    renderSearchResults();
  }
}

function renderSearchResults() {
  const body = document.getElementById("search-body");
  if (state.searchResults === null) {
    body.innerHTML = `<div class="search-empty">type a query and hit Enter.</div>`;
    return;
  }
  if (state.searchResults.length === 0) {
    body.innerHTML = `<div class="search-empty">no matches for <code>${esc(state.searchQuery)}</code> under current filters.</div>`;
    return;
  }
  body.innerHTML = state.searchResults.map(renderSearchCard).join("");
  wireSearchCardActions();
}

// matchFieldLabel maps the server's field names to short UI labels.
// Kept next to the color classes so a glance at the code tells you
// which fields exist and how they render.
const MATCH_FIELD_LABEL = {
  title: "title",
  brief: "brief",
  note: "note",
  question: "question",
  mode: "mode",
  bundle_hash: "bundle",
  fragment_path: "path",
  fragment_content: "content",
};

function renderSearchCard(rec) {
  const headline = rec.title && rec.title.trim()
    ? rec.title
    : (rec.question || "(no title · no question)");
  const shortPath = (rec.path || "").split("/").slice(-2).join("/");
  const groundedClass = rec.grounded_ratio >= 0.8 ? "good"
    : rec.grounded_ratio >= 0.5 ? "warn" : "bad";
  const driftClass = rec.drift_rate > 0.3 ? "bad"
    : rec.drift_rate > 0.1 ? "warn" : "good";

  const matchesHtml = (rec.matches || []).map(m => {
    const label = MATCH_FIELD_LABEL[m.field] || m.field;
    const relPath = m.rel_path
      ? `<code class="match-relpath">${esc(m.rel_path)}</code>`
      : "";
    const snippet = highlightSnippet(m.snippet || "", state.searchQuery);
    const targetClass = m.rel_path ? " match-target" : "";
    const targetAttr = m.rel_path
      ? ` data-rel-path="${esc(m.rel_path)}" title="Open matching fragment in Journal"`
      : "";

    return `<div class="match${targetClass}"${targetAttr}>
      <span class="match-field match-${esc(m.field)}">${esc(label)}</span>
      ${relPath}
      <span class="match-snippet">${snippet}</span>
    </div>`;
  }).join("");

  return `<div class="search-card mode-${esc(rec.mode || "unknown")}"
    data-path="${esc(rec.path)}">
    <div class="search-head">
      <span class="search-title">${esc(headline)}</span>
      ${modeBadge(rec.mode)}
      <span class="search-time">${esc(rec.timestamp || "")}</span>
    </div>
    <div class="search-metrics">
      <span class="badge ${groundedClass}">grounded ${fmtPct(rec.grounded_ratio || 0)}</span>
      <span class="badge ${driftClass}">drift ${fmtPct(rec.drift_rate || 0)}</span>
      <span class="badge">score ${rec.score}</span>
    </div>
    <div class="search-matches">${matchesHtml}</div>
    <div class="search-actions">
      <button data-action="open">Open in Journal</button>
      <button data-action="cmp-a">Use as Compare A</button>
      <button data-action="cmp-b">Use as Compare B</button>
      <button data-action="copy">Copy path</button>
      <span class="path" title="${esc(rec.path)}">${esc(shortPath)}</span>
    </div>
  </div>`;
}

// highlightSnippet wraps every case-insensitive occurrence of `q` in
// <mark>. We escape both sides before interleaving so a query like
// "<tag>" cannot inject markup.
function highlightSnippet(snippet, q) {
  if (!q) return esc(snippet);
  const low = snippet.toLowerCase();
  const lowQ = q.toLowerCase();
  if (!lowQ) return esc(snippet);
  let out = "";
  let i = 0;
  while (i < snippet.length) {
    const idx = low.indexOf(lowQ, i);
    if (idx < 0) { out += esc(snippet.slice(i)); break; }
    out += esc(snippet.slice(i, idx));
    out += `<mark>${esc(snippet.slice(idx, idx + q.length))}</mark>`;
    i = idx + q.length;
  }
  return out;
}

function wireSearchCardActions() {
  document.querySelectorAll(".search-card").forEach(card => {
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
        } else if (act === "open") {
          openInJournal(path);
        }
      });
    });

    card.querySelectorAll(".match.match-target").forEach(row => {
      row.addEventListener("click", () => {
        openInJournal(path, {
          relPath: row.dataset.relPath || "",
          needle: state.searchQuery || "",
        });
      });
    });
  });
}

function waitNextFrame() {
  return new Promise(resolve => requestAnimationFrame(resolve));
}

async function ensureJournalCardExpanded(card, btn) {
  const panel = card.querySelector(".journal-expand");
  if (!panel) return null;
  const path = card.dataset.path;

  if (panel.dataset.loaded === "1") {
    panel.hidden = false;
    if (btn) btn.textContent = "Collapse";
    return panel;
  }

  if (btn) {
    btn.disabled = true;
    btn.textContent = "loading…";
  }

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
    if (btn) btn.textContent = "Collapse";
    return panel;
  } catch (e) {
    panel.innerHTML = `<div class="muted">could not load record: ${esc(e.message)}</div>`;
    panel.hidden = false;
    if (btn) btn.textContent = "Expand";
    return panel;
  } finally {
    if (btn) btn.disabled = false;
  }
}

function clearJournalPreHighlights() {
  document.querySelectorAll(".fragment pre.log[data-raw-text]").forEach(pre => {
    if (pre.querySelector("mark.match-focus")) {
      pre.textContent = pre.dataset.rawText || pre.textContent;
    }
  });
}

function highlightFirstInPre(pre, needle) {
  const source = pre.dataset.rawText || pre.textContent || "";
  pre.dataset.rawText = source;

  if (!needle || !needle.trim()) {
    pre.textContent = source;
    return null;
  }

  const rawNeedle = needle.trim();
  const lowSource = source.toLowerCase();
  const lowNeedle = rawNeedle.toLowerCase();
  const idx = lowSource.indexOf(lowNeedle);

  pre.textContent = source;
  if (idx < 0) return null;

  const before = source.slice(0, idx);
  const hit = source.slice(idx, idx + rawNeedle.length);
  const after = source.slice(idx + rawNeedle.length);

  const mark = document.createElement("mark");
  mark.className = "match-focus";
  mark.textContent = hit;

  pre.replaceChildren(
    document.createTextNode(before),
    mark,
    document.createTextNode(after),
  );

  return mark;
}

function flashFragment(fragment) {
  fragment.classList.remove("fragment-focus");
  void fragment.offsetWidth;
  fragment.classList.add("fragment-focus");
}

async function focusFragmentInPanel(panel, relPath, needle) {
  if (!panel || !relPath) return;

  const block = panel.querySelector(".fragments-block");
  if (block && block.tagName === "DETAILS") {
    block.open = true;
  }

  await waitNextFrame();

  const fragment = panel.querySelector(`.fragment[data-rel-path="${cssEscape(relPath)}"]`);
  if (!fragment) return;

  fragment.open = true;
  flashFragment(fragment);
  fragment.scrollIntoView({ behavior: "smooth", block: "center" });

  await waitNextFrame();

  clearJournalPreHighlights();

  const pre = fragment.querySelector("pre.log");
  if (!pre) return;

  const mark = highlightFirstInPre(pre, needle);
  if (!mark) return;

  const top = Math.max(0, mark.offsetTop - (pre.clientHeight / 2));
  pre.scrollTo({ top, behavior: "smooth" });
}

// openInJournal is the bridge from Search back into the Journal flow.
// To make the target card actually visible we have to reset both
// filter and text search on the Journal side — otherwise a filter
// change since the user last used Journal would hide the very card
// they just asked for. After the render lands we scroll to the card,
// expand it, and flash it briefly so the eye locks on.
async function openInJournal(path, target = null) {
  state.journalFilter = "all";
  state.journalSearch = "";
  document.querySelectorAll("#journal-filter button")
    .forEach(b => b.classList.toggle("active", b.dataset.filter === "all"));

  const searchInput = document.getElementById("journal-search");
  if (searchInput) searchInput.value = "";

  state.pendingJournalFocus = path;
  switchTab("journal");
  await loadJournal();
  await focusJournalCard(path, target);
}

async function focusJournalCard(path, target = null) {
  if (!path) return;

  await waitNextFrame();

  const card = document.querySelector(`.journal-card[data-path="${cssEscape(path)}"]`);
  if (!card) return;

  card.scrollIntoView({ behavior: "smooth", block: "center" });

  const btn = card.querySelector('[data-action="expand"]');
  const panel = card.querySelector(".journal-expand");

  if (btn && panel && panel.hidden) {
    await ensureJournalCardExpanded(card, btn);
  }

  card.classList.remove("focus-flash");
  void card.offsetWidth;
  card.classList.add("focus-flash");

  if (panel && target && target.relPath) {
    await focusFragmentInPanel(panel, target.relPath, target.needle || "");
  }

  state.pendingJournalFocus = null;
}

// cssEscape escapes a string for use in a CSS selector. Paths contain
// characters (`/`, `.`, `-`) that CSS tolerates inside attribute
// selectors, but quotes/backslashes would break the selector — so we
// escape defensively with the native CSS.escape when present.
function cssEscape(s) {
  if (window.CSS && typeof window.CSS.escape === "function") {
    return window.CSS.escape(s);
  }
  return s.replace(/["\\]/g, "\\$&");
}

// ------------------------------ compare ------------------------------

document.getElementById("compare-btn").addEventListener("click", async () => {
  if (!requireRepo()) return;
  const a = document.getElementById("cmp-a").value.trim();
  const b = document.getElementById("cmp-b").value.trim();
  if (!a || !b) { alert("Need both record paths."); return; }
  const status = document.getElementById("compare-status");
  status.textContent = "diffing…";
  try {
    const d = await j("POST", "/api/diff", { repo: state.repo, a, b });
    renderDiff(d);
    status.textContent = "";
  } catch (e) {
    document.getElementById("compare-report").innerHTML =
      `<span class="error">${esc(e.message)}</span>`;
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

// ------------------------------ landing v2 (home) ------------------------------
// Scoped to the landing: header .lang-toggle, .landing-*, and the how-it-works
// modal. The rest of the app stays in English on purpose — entry page only.

const LANDING_LANG_KEY = "neurofs.lang";
const LANDING_DICT = {
  en: {
    "brand.sub": "context compiler — local UI",

    "nav.home": "Home",
    "nav.workspace": "Workspace",
    "nav.task": "New task",
    "nav.response": "Response",
    "nav.records": "Records",
    "nav.journal": "Journal",
    "nav.search": "Search",
    "nav.compare": "Compare",
    "nav.guide": "Guide",

    "common.refresh": "Refresh",
    "common.filter": "Filter",
    "common.none": "—",
    "common.yes": "yes",
    "common.no": "no",

    "mode.strategy": "Strategy",
    "mode.build": "Build",
    "mode.review": "Review",
    "mode.unknown": "Unknown",
    "filter.all": "All",
    "filter.unknown": "Unknown",

    "landing.eyebrow": "Local app — works with Claude & ChatGPT",
    "landing.title":
      'Stop <span class="landing-strike">re-explaining</span> your project.<br><span class="landing-accent-word">Continue.</span>',
    "landing.subtitle":
      "NeuroFS keeps your work focused, prepares only the context that matters, and helps you continue where you left off.",
    "landing.ctaPrimary": "Start a task",
    "landing.ctaSecondary": "Continue previous work",
    "landing.howTo": "See how it works",
    "landing.card1.h": "Start focused",
    "landing.card1.p": "Describe what you want to do. NeuroFS finds the files that matter.",
    "landing.card2.h": "Save context",
    "landing.card2.p": "Send less noise to Claude or ChatGPT and keep your prompts lighter.",
    "landing.card3.h": "Continue later",
    "landing.card3.p": "Pick up from previous work without explaining everything again.",
    "landing.edu.before": "Without NeuroFS",
    "landing.edu.beforeCaption": "You paste everything. Most of it is noise.",
    "landing.edu.after": "With NeuroFS",
    "landing.edu.afterCaption": "Only what matters. Focused and lighter.",
    "landing.footer.ready": "Ready?",
    "landing.footer.cta": "Create your first task",

    "modal.eyebrow": "How it works",
    "modal.title": "Three steps, thirty seconds",
    "modal.step1.caption": "\"Fix the login button on mobile\"",
    "modal.step1.title": "Describe your task",
    "modal.step1.body":
      "Write it like you'd tell a teammate. One sentence is enough — no need to list files or paths.",
    "modal.step2.title": "NeuroFS picks the right files",
    "modal.step2.body":
      "It scans your repo, keeps what matters for your task, and leaves the noise out. All local — nothing leaves your machine.",
    "modal.step3.title": "Paste into Claude or ChatGPT",
    "modal.step3.body":
      "Copy the focused bundle and drop it in. Your AI answers faster, with less confusion. Come back tomorrow and continue.",
    "modal.back": "← Back",
    "modal.next": "Next →",
    "modal.finish": "Start my first task",

    "workspace.h": "Workspace",
    "workspace.lead": "Pick an absolute path to a repo. The path is stored in <code>localStorage</code>, never sent anywhere besides this local server.",
    "workspace.repoPath": "Repo path",
    "workspace.useRepo": "Use this repo",
    "workspace.runScan": "Run scan",
    "workspace.verbose": "verbose",
    "workspace.scanning": "scanning…",
    "workspace.scanDone": "scan complete",
    "workspace.scanFailed": "scan failed",
    "workspace.saved": "saved",
    "workspace.noRepo": "No repo selected.",
    "workspace.indexedAt": "Indexed at",
    "workspace.files": "files",
    "workspace.symbols": "symbols",
    "workspace.imports": "imports",
    "workspace.indexSize": "index size",

    "task.h": "New task",
    "task.lead": "Say what you want to do. NeuroFS picks only the files that matter and builds a ready-to-paste prompt — so you don't re-send your whole project every time.",
    "task.mode": "Mode",
    "oneshot.h": "One-shot prompt",
    "oneshot.sub": "Type the question, get a paste-ready prompt. Auto-scans on first use; cached after.",
    "oneshot.questionLabel": "Your question",
    "oneshot.questionPh": "e.g. where is jwt verified",
    "oneshot.btn": "Generate prompt",
    "oneshot.force": "force re-rank (skip cache)",
    "oneshot.running": "generating…",
    "oneshot.cached": "served from cache",
    "oneshot.fresh": "freshly ranked",
    "oneshot.cachedNote": "cache hit",
    "oneshot.autoScanned": "scan ran first",
    "oneshot.copy": "Copy prompt",
    "oneshot.copied": "copied",
    "oneshot.clipboardDenied": "clipboard denied — use download",
    "oneshot.download": "Download .prompt.txt",
    "oneshot.topPicks": "Top picks",
    "oneshot.previewPrompt": "Preview prompt",
    "alert.enterQuestion": "Enter a question.",
    "resume.tag": "Resuming from",
    "resume.clear": "Clear",
    "resume.focusLabel": "Inherited focus paths (edit freely — removed paths won't boost the ranker)",
    "resume.focusPh": "one rel_path per line",
    "pack.headline": "Ready to send",
    "pack.savedBig": "You saved ~{n} tokens",
    "pack.ready": "Your bundle is ready",
    "pack.benefit": "Instead of re-sending your whole project, this bundle keeps only what matters.",
    "pack.fileOne": "file picked",
    "pack.fileMany": "files picked",
    "pack.readyToPaste": "ready to paste into Claude / ChatGPT",
    "pack.savedAs": "saved as",
    "pack.technical": "Technical detail",
    "journal.continuesFrom": "continues from:",
    "journal.gotoParentTitle": "Open parent record",
    "journal.resumeFromThis": "Resume from this",
    "task.titleLabel": "Title (short label for this run)",
    "task.titlePh": "e.g. 010 human metadata",
    "task.questionLabel": "Question (short, what you ask Claude to focus on)",
    "task.questionPh": "e.g. how does the ranker handle stemming",
    "task.briefLabel": "Brief (what you are trying to do and why — goes with the record)",
    "task.briefPh": "optional: the one-paragraph brief. This is what 'you-from-next-week' will thank 'you-from-today' for writing.",
    "task.budget": "Token budget",
    "task.focus": "Focus prefix (optional, comma-separated)",
    "task.boostChanged": "boost git-changed files",
    "task.maxFiles": "Max files <input type=\"number\" id=\"q-maxfiles\" value=\"8\" min=\"0\">",
    "task.maxFrags": "Max fragments <input type=\"number\" id=\"q-maxfrags\" value=\"16\" min=\"0\">",
    "task.preferSigs": "prefer signatures",
    "task.slug": "Task slug (optional, used to name the run)",
    "task.snapshot": "Bundle snapshot name (auto-filled from slug + mode)",
    "task.runPreview": "Run name will be: <code>—</code>",
    "task.template": "Claude prompt template (edit before copying)",
    "task.pack": "Pack bundle",
    "task.resetTemplate": "Reset template to mode default",
    "task.copyPrompt": "Copy full prompt (template + bundle)",
    "task.downloadPrompt": "Download prompt",
    "task.goResponse": "Next: paste response →",
    "task.previewPrompt": "Preview prompt (template + bundle)",
    "task.fragmentsIncluded": "Fragments included",
    "task.packing": "packing…",
    "task.packDone": "packed",
    "task.packFailed": "pack failed",
    "task.copied": "copied",
    "task.downloaded": "downloaded",
    "task.runName": "Run name will be:",

    "response.h": "Response",
    "response.lead": "Paste the model's answer. Replay runs the offline audit: citation validation, drift classifier, optional fact recall.",
    "response.modelId": "Model id (free-form label)",
    "response.bundleSource": "Bundle source",
    "response.bundleLast": "Use the last packed bundle (in-memory)",
    "response.bundleSnapshot": "Use a saved bundle JSON (audit/bundles/...)",
    "response.snapshotPath": "Snapshot path",
    "response.titleLabel": "Title (inherited from pack — editable)",
    "response.titlePh": "short label for this run",
    "response.facts": "Expected facts (comma-separated, optional)",
    "response.briefLabel": "Brief (inherited from pack — editable)",
    "response.briefPh": "what the run was trying to do",
    "response.textLabel": "Response text",
    "response.textPh": "Paste the answer here...",
    "response.noteLabel": "Note (your conclusion once you read the response and the audit)",
    "response.notePh": "what did we learn? keep / discard? next step?",
    "response.runReplay": "Run replay",
    "response.persist": "persist under audit/records/",
    "response.replaying": "replaying…",
    "response.replayDone": "replay complete",
    "response.replayFailed": "replay failed",

    "records.h": "Records",
    "records.lead": "All persisted audit records under <code>&lt;repo&gt;/audit/records/</code>. Select two to compare.",
    "records.col.when": "When",
    "records.col.mode": "Mode",
    "records.col.context": "Context",
    "records.col.model": "Model",
    "records.col.grounded": "Grounded",
    "records.col.drift": "Drift",
    "records.col.recall": "Recall",
    "records.col.bundle": "Bundle",
    "records.diffSelected": "Diff selected →",
    "records.loading": "loading…",
    "records.loadFailed": "failed to load",
    "records.empty": "no records yet",
    "records.selected": "selected",

    "journal.h": "Journal",
    "journal.lead": "A readable timeline of your audit records — each run rendered as a card with its title, brief, note and the metrics. Legacy records (no title/brief/note) show up as cards too, anchored to their question.",
    "journal.searchPh": "search title / brief / note / question",
    "journal.loading": "loading…",
    "journal.loadFailed": "failed to load",
    "journal.empty": "no records yet",
    "journal.noMatches": "no matches",
    "journal.brief": "Brief",
    "journal.note": "Note",
    "journal.question": "Question",
    "journal.expand": "Show fragments",
    "journal.collapse": "Hide fragments",
    "journal.fragmentsCount": "fragments",
    "journal.viewFile": "view file",

    "search.h": "Search",
    "search.lead": "Find prior runs by title, brief, note, question, mode, bundle hash, fragment paths, or fragment content. Case-insensitive substring — quick and local.",
    "search.ph": "text to find across your audit history (Enter to search)",
    "search.btn": "Search",
    "search.scope": "Scope",
    "search.metadata": "Metadata",
    "search.paths": "Paths",
    "search.content": "Content",
    "search.searching": "searching…",
    "search.noResults": "no results",
    "search.typeToSearch": "type something and press Enter.",
    "search.resultsCount": "results",

    "compare.h": "Compare",
    "compare.lead": "Deltas are B − A. Positive drift is worse; positive grounded and recall are better.",
    "compare.recA": "Record A <input type=\"text\" id=\"cmp-a\" placeholder=\"audit/records/...\">",
    "compare.recB": "Record B <input type=\"text\" id=\"cmp-b\" placeholder=\"audit/records/...\">",
    "compare.btn": "Compute diff",
    "compare.computing": "computing…",
    "compare.done": "done",
    "compare.failed": "diff failed",

    "guide.h": "Guide",
    "guide.pipeline.h": "Pipeline",
    "guide.pipeline.p": "NeuroFS is a pipeline: <strong>scan → pack → (Claude) → replay</strong>. Scan builds the SQLite index at <code>.neurofs/index.db</code>. Pack ranks and compresses. Replay is the offline governance step.",
    "guide.ranker.h": "Ranker signals",
    "guide.ranker.filename": "<code>filename_match</code> — token is in the path.",
    "guide.ranker.path": "<code>path_match</code> — focus prefix hit.",
    "guide.ranker.symbol": "<code>symbol_match</code> — token is in the symbols table (with shallow stemming).",
    "guide.ranker.import": "<code>import_match</code> — token appears in imports.",
    "guide.ranker.focus": "<code>focus</code> / <code>changed</code> — boosts from <code>--focus</code> / <code>--changed</code>.",
    "guide.audit.h": "Audit metrics",
    "guide.audit.grounded": "<strong>grounded_ratio</strong> — fraction of citations that resolve to a file inside the bundle.",
    "guide.audit.drift": "<strong>drift_rate</strong> — unknown / (known + unknown) symbols, split across three buckets:<ul><li><strong>paths</strong> — file-like references not in the bundle.</li><li><strong>apis</strong> — dotted names (<code>jwt.sign</code>) not in the bundle.</li><li><strong>symbols</strong> — plain code-shaped identifiers not in the bundle.</li></ul>",
    "guide.audit.recall": "<strong>answer_recall</strong> — fraction of <code>--facts</code> strings present in the response (case-insensitive substring).",
    "guide.impl.h": "What is implemented",
    "guide.impl.server": "Local HTTP server + embedded UI (<code>internal/ui</code>).",
    "guide.impl.endpoints": "Endpoints wrap scan, pack, replay, records list, diff, stats.",
    "guide.impl.bundle": "Bundle is held in memory between <em>pack</em> and <em>replay</em>. Use snapshot path for cross-session replay.",
    "guide.next.h": "What is recommended next",
    "guide.next.stream": "Streaming scan progress (WebSocket / SSE) — today the UI blocks.",
    "guide.next.diff": "Inline diff viewer with colour — today rendered as plain text.",
    "guide.next.workspace": "Persistent workspace list — today one repo at a time in <code>localStorage</code>.",
    "guide.next.api": "Optional live Claude API integration wrapped behind an explicit toggle.",

    "stats.title": "Pack stats",
    "stats.budget": "budget",
    "stats.used": "used",
    "stats.files": "files",
    "stats.fragments": "fragments",
    "stats.bundleHash": "bundle hash",

    "replay.grounded": "Grounded",
    "replay.drift": "Drift",
    "replay.recall": "Recall",
    "replay.savedTo": "Saved to",
    "replay.unknownPaths": "Unknown paths",
    "replay.unknownApis": "Unknown APIs",
    "replay.unknownSymbols": "Unknown symbols",
    "replay.missingFacts": "Missing facts",
    "replay.foundFacts": "Found facts",

    "common.loading": "loading…",
    "common.searching": "searching…",
    "common.error": "error",
    "common.errorPrefix": "error: ",
    "common.done": "done",
    "common.copied": "copied",
    "common.copyFailed": "copy failed",
    "common.scanning": "scanning…",
    "common.diffing": "diffing…",
    "common.doneIn": "done in",
    "common.ms": "ms",
    "common.checking": "checking…",

    "alert.setRepo": "Set a repo path in the Workspace tab first.",
    "alert.enterPath": "Enter an absolute path.",
    "alert.tilde": "Use the full absolute path — `~` isn't expanded in the browser.",
    "alert.absolute": "Repo path must be absolute (start with /).",
    "alert.enterQuestion": "Enter a question.",
    "alert.pasteResponse": "Paste the model response.",
    "alert.snapshotRequired": "Snapshot path is required in snapshot mode.",
    "alert.noInMemoryBundle": "No in-memory bundle. Either re-pack with a snapshot name, or switch bundle source to snapshot.",
    "alert.needBothPaths": "Need both record paths.",

    "home.workingOn": "Working on",
    "home.filesIndexedSuffix": "files indexed",
    "home.indexNotReady": "index not ready — open Workspace to scan",
    "home.repoSet": "Repo set. Run scan to index.",
    "home.repoRejected": "Repo rejected — see error below.",

    "stat.packed": "packed",

    "bundle.h": "Bundle",
    "bundle.tokens": "tokens",
    "bundle.filesIncluded": "files included",
    "bundle.compression": "compression",
    "bundle.snapshot": "snapshot",
    "bundle.notSaved": "not saved (no snapshot name given)",
    "bundle.noFragments": "no fragments",
    "bundle.fragPath": "path",
    "bundle.fragRep": "representation",
    "bundle.fragTokens": "tokens",
    "bundle.fragScore": "score",

    "pack.copiedClipboard": "copied (template + bundle)",
    "pack.clipboardDenied": "clipboard denied — use download",

    "records.nSuffix": "records",
    "records.selectedSuffix": "selected",
    "records.filterSuffix": "filter",
    "records.noMatchFilter": "no records match filter",
    "records.noneYet": "no records yet — run a replay and enable \"persist\"",

    "journal.noWorkspace": "no workspace",
    "journal.zeroRecords": "0 records",
    "journal.noneYetPersist": "No records yet. Run a replay and tick \"persist\".",
    "journal.filteredOutSuffix": "filtered out",
    "journal.noMatchFilter": "No records match the current filter.",
    "journal.runSingle": "run",
    "journal.runPlural": "runs",
    "journal.untitled": "(untitled)",
    "journal.nRecordsSuffix": "records",
    "journal.expandBtnLabel": "Expand",
    "journal.collapseBtnLabel": "Collapse",
    "journal.cmpALabel": "Use as Compare A",
    "journal.cmpBLabel": "Use as Compare B",
    "journal.copyPathLabel": "Copy path",
    "journal.expandLoading": "loading…",
    "journal.loadFail": "could not load record",
    "journal.bundleFragmentsHeader": "Bundle fragments",
    "journal.tokensSuffix": "tokens",
    "journal.tokSuffix": "tok",
    "journal.noFragmentsPersisted": "no fragments persisted in this record",
    "journal.noContentPersisted": "no content persisted for this fragment — only metadata is available",
    "journal.when": "when",
    "journal.bundle": "bundle",

    "audit.h": "Audit",
    "audit.title": "title",
    "audit.brief": "brief",
    "audit.note": "note",
    "audit.question": "question",
    "audit.mode": "mode",
    "audit.model": "model",
    "audit.bundleHash": "bundle hash",
    "audit.grounded": "grounded",
    "audit.citationsSuffix": "citations",
    "audit.drift": "drift",
    "audit.unknownOf": "unknown of",
    "audit.factRecall": "fact recall",
    "audit.record": "record",
    "audit.unknownPaths": "unknown paths",
    "audit.unknownApis": "unknown apis",
    "audit.unknownSymbols": "unknown symbols",
    "audit.invalidCitations": "Invalid citations",
    "audit.moreSuffix": "more",

    "search.openInJournal": "Open in Journal",
    "search.setRepo": "Set a repo in the Workspace tab.",
    "search.noMatchFor": "no matches for",
    "search.underFilters": "under current filters.",
    "search.matchesSuffix": "matches",
    "search.matchSuffix": "match",
    "search.ofMatchesShown": "matches shown",
    "search.of": "of",
    "search.scoreLabel": "score",

    "compare.diffH": "Diff",
    "compare.sameBundle": "same bundle",
    "compare.sameQuestion": "same question",
    "compare.sameModel": "same model",
    "compare.diffModeWarning": "(different — interpret deltas with care)",
    "compare.groundedDelta": "grounded Δ",
    "compare.driftDelta": "drift Δ",
    "compare.recallDelta": "recall Δ",
    "compare.pathsBucket": "paths",
    "compare.apisBucket": "apis",
    "compare.symbolsBucket": "symbols",

    "mode.whenToUse": "When to use",
    "mode.expectedOutput": "Expected output",
    "mode.nextStep": "Next step",

    "mode.strategy.label": "Strategy",
    "mode.strategy.subtitle": "Decide the approach before writing code.",
    "mode.strategy.when": "You're starting an iteration and want a plan, not an implementation.",
    "mode.strategy.output": "A short technical design, key decisions, probable files, and a minimal test plan.",
    "mode.strategy.next": "Read the plan, agree on scope, then switch to Build for the actual change.",

    "mode.build.label": "Build",
    "mode.build.subtitle": "Implement an iteration that is already defined.",
    "mode.build.when": "You already have a plan (from Strategy or from your own head) and want working code.",
    "mode.build.output": "A diff-shaped proposal: files to change, code, test notes, how to run it.",
    "mode.build.next": "Paste the response in the Response tab and run replay; if grounded/drift look good, apply the diff.",

    "mode.review.label": "Review",
    "mode.review.subtitle": "Evaluate a response, diff, or proposal before integrating.",
    "mode.review.when": "You have something (someone else's patch, a previous Claude answer, a refactor) and need a second read.",
    "mode.review.output": "A structured review: what is correct, what is risky, what looks hallucinated, what to do next.",
    "mode.review.next": "Apply the fixes you agree with, discard the rest, then run a Build iteration if needed.",

    "workspace.repoKv": "repo",
    "workspace.indexedSuffix": "indexed",
    "workspace.bytes": "bytes",
    "workspace.langBreakdown": "no language breakdown",
    "workspace.auditAggregate": "Audit aggregate",
    "workspace.recordsSuffix": "records",
    "workspace.recall": "recall",
  },
  es: {
    "brand.sub": "compilador de contexto — UI local",

    "nav.home": "Inicio",
    "nav.workspace": "Espacio",
    "nav.task": "Nueva tarea",
    "nav.response": "Respuesta",
    "nav.records": "Registros",
    "nav.journal": "Diario",
    "nav.search": "Buscar",
    "nav.compare": "Comparar",
    "nav.guide": "Guía",

    "common.refresh": "Refrescar",
    "common.filter": "Filtro",
    "common.none": "—",
    "common.yes": "sí",
    "common.no": "no",

    "mode.strategy": "Estrategia",
    "mode.build": "Construir",
    "mode.review": "Revisar",
    "mode.unknown": "Desconocido",
    "filter.all": "Todos",
    "filter.unknown": "Desconocido",

    "landing.eyebrow": "App local — funciona con Claude y ChatGPT",
    "landing.title":
      'Deja de <span class="landing-strike">re-explicar</span> tu proyecto.<br><span class="landing-accent-word">Continúa.</span>',
    "landing.subtitle":
      "NeuroFS mantiene tu trabajo enfocado, prepara solo el contexto que importa y te ayuda a continuar donde lo dejaste.",
    "landing.ctaPrimary": "Empezar una tarea",
    "landing.ctaSecondary": "Retomar trabajo anterior",
    "landing.howTo": "Ver cómo funciona",
    "landing.card1.h": "Empieza enfocado",
    "landing.card1.p": "Describe lo que quieres hacer. NeuroFS encuentra los archivos que importan.",
    "landing.card2.h": "Ahorra contexto",
    "landing.card2.p": "Envía menos ruido a Claude o ChatGPT y mantén tus prompts más ligeros.",
    "landing.card3.h": "Continúa después",
    "landing.card3.p": "Retoma trabajo anterior sin tener que explicar todo de nuevo.",
    "landing.edu.before": "Sin NeuroFS",
    "landing.edu.beforeCaption": "Pegas todo. La mayoría es ruido.",
    "landing.edu.after": "Con NeuroFS",
    "landing.edu.afterCaption": "Solo lo que importa. Enfocado y ligero.",
    "landing.footer.ready": "¿Listo?",
    "landing.footer.cta": "Crea tu primera tarea",

    "modal.eyebrow": "Cómo funciona",
    "modal.title": "Tres pasos, treinta segundos",
    "modal.step1.caption": "\"Arregla el botón de login en mobile\"",
    "modal.step1.title": "Describe tu tarea",
    "modal.step1.body":
      "Escríbela como si hablaras con un compañero. Una frase basta — no hace falta listar archivos ni rutas.",
    "modal.step2.title": "NeuroFS elige los archivos adecuados",
    "modal.step2.body":
      "Escanea tu repo, se queda con lo que importa para la tarea y deja fuera el ruido. Todo local — nada sale de tu máquina.",
    "modal.step3.title": "Pégalo en Claude o ChatGPT",
    "modal.step3.body":
      "Copia el bundle enfocado y pégalo. Tu IA responde más rápido, con menos confusión. Vuelve mañana y continúa.",
    "modal.back": "← Atrás",
    "modal.next": "Siguiente →",
    "modal.finish": "Empezar mi primera tarea",

    "workspace.h": "Espacio de trabajo",
    "workspace.lead": "Elige una ruta absoluta a un repositorio. La ruta se guarda en <code>localStorage</code>, nunca se envía a ningún sitio más allá de este servidor local.",
    "workspace.repoPath": "Ruta del repo",
    "workspace.useRepo": "Usar este repo",
    "workspace.runScan": "Escanear",
    "workspace.verbose": "detallado",
    "workspace.scanning": "escaneando…",
    "workspace.scanDone": "escaneo completo",
    "workspace.scanFailed": "falló el escaneo",
    "workspace.saved": "guardado",
    "workspace.noRepo": "No hay repo seleccionado.",
    "workspace.indexedAt": "Indexado en",
    "workspace.files": "archivos",
    "workspace.symbols": "símbolos",
    "workspace.imports": "imports",
    "workspace.indexSize": "tamaño del índice",

    "task.h": "Nueva tarea",
    "task.lead": "Di qué quieres hacer. NeuroFS elige solo los archivos que importan y arma un prompt listo para pegar — para que no tengas que reenviar todo tu proyecto cada vez.",
    "oneshot.h": "Prompt de un solo paso",
    "oneshot.sub": "Escribe la pregunta y obtén un prompt listo para pegar. Escanea solo la primera vez; luego usa caché.",
    "oneshot.questionLabel": "Tu pregunta",
    "oneshot.questionPh": "p. ej. dónde se verifica el jwt",
    "oneshot.btn": "Generar prompt",
    "oneshot.force": "forzar re-ranking (saltar caché)",
    "oneshot.running": "generando…",
    "oneshot.cached": "servido desde caché",
    "oneshot.fresh": "rankeado en fresco",
    "oneshot.cachedNote": "cache hit",
    "oneshot.autoScanned": "escaneo previo",
    "oneshot.copy": "Copiar prompt",
    "oneshot.copied": "copiado",
    "oneshot.clipboardDenied": "portapapeles denegado — usa descargar",
    "oneshot.download": "Descargar .prompt.txt",
    "oneshot.topPicks": "Mejores candidatos",
    "oneshot.previewPrompt": "Previsualizar prompt",
    "alert.enterQuestion": "Escribe una pregunta.",
    "task.mode": "Modo",
    "resume.tag": "Continuando desde",
    "resume.clear": "Limpiar",
    "resume.focusLabel": "Rutas de foco heredadas (edítalas — las rutas borradas no impulsarán al ranker)",
    "resume.focusPh": "una rel_path por línea",
    "pack.headline": "Listo para enviar",
    "pack.savedBig": "Te ahorraste ~{n} tokens",
    "pack.ready": "Tu bundle está listo",
    "pack.benefit": "En vez de reenviar todo tu proyecto, este bundle conserva solo lo que importa.",
    "pack.fileOne": "archivo elegido",
    "pack.fileMany": "archivos elegidos",
    "pack.readyToPaste": "listo para pegar en Claude / ChatGPT",
    "pack.savedAs": "guardado como",
    "pack.technical": "Detalle técnico",
    "journal.continuesFrom": "continúa de:",
    "journal.gotoParentTitle": "Abrir registro padre",
    "journal.resumeFromThis": "Continuar desde este",
    "task.titleLabel": "Título (etiqueta corta para esta ejecución)",
    "task.titlePh": "ej. 010 metadata humana",
    "task.questionLabel": "Pregunta (corta, en qué quieres que Claude se enfoque)",
    "task.questionPh": "ej. cómo maneja el ranker el stemming",
    "task.briefLabel": "Resumen (qué intentas hacer y por qué — va con el registro)",
    "task.briefPh": "opcional: el resumen de un párrafo. Esto es lo que 'tú-de-la-próxima-semana' le agradecerá a 'tú-de-hoy' por escribir.",
    "task.budget": "Presupuesto de tokens",
    "task.focus": "Prefijo de foco (opcional, separado por comas)",
    "task.boostChanged": "priorizar archivos cambiados en git",
    "task.maxFiles": "Máx archivos <input type=\"number\" id=\"q-maxfiles\" value=\"8\" min=\"0\">",
    "task.maxFrags": "Máx fragmentos <input type=\"number\" id=\"q-maxfrags\" value=\"16\" min=\"0\">",
    "task.preferSigs": "preferir firmas",
    "task.slug": "Slug de tarea (opcional, se usa para nombrar la ejecución)",
    "task.snapshot": "Nombre del snapshot del bundle (auto-rellenado desde slug + modo)",
    "task.runPreview": "El nombre será: <code>—</code>",
    "task.template": "Plantilla de prompt para Claude (edítala antes de copiar)",
    "task.pack": "Empaquetar bundle",
    "task.resetTemplate": "Restablecer plantilla al modo por defecto",
    "task.copyPrompt": "Copiar prompt completo (plantilla + bundle)",
    "task.downloadPrompt": "Descargar prompt",
    "task.goResponse": "Siguiente: pegar respuesta →",
    "task.previewPrompt": "Previsualizar prompt (plantilla + bundle)",
    "task.fragmentsIncluded": "Fragmentos incluidos",
    "task.packing": "empaquetando…",
    "task.packDone": "empaquetado",
    "task.packFailed": "falló el empaquetado",
    "task.copied": "copiado",
    "task.downloaded": "descargado",
    "task.runName": "El nombre será:",

    "response.h": "Respuesta",
    "response.lead": "Pega la respuesta del modelo. Replay ejecuta la auditoría offline: validación de citas, clasificador de drift, recall de hechos opcional.",
    "response.modelId": "Id del modelo (etiqueta libre)",
    "response.bundleSource": "Fuente del bundle",
    "response.bundleLast": "Usar el último bundle empaquetado (en memoria)",
    "response.bundleSnapshot": "Usar un JSON de bundle guardado (audit/bundles/...)",
    "response.snapshotPath": "Ruta del snapshot",
    "response.titleLabel": "Título (heredado del pack — editable)",
    "response.titlePh": "etiqueta corta para esta ejecución",
    "response.facts": "Hechos esperados (separados por coma, opcional)",
    "response.briefLabel": "Resumen (heredado del pack — editable)",
    "response.briefPh": "qué intentaba hacer la ejecución",
    "response.textLabel": "Texto de la respuesta",
    "response.textPh": "Pega la respuesta aquí...",
    "response.noteLabel": "Nota (tu conclusión al leer la respuesta y la auditoría)",
    "response.notePh": "¿qué aprendimos? ¿mantener / descartar? ¿siguiente paso?",
    "response.runReplay": "Ejecutar replay",
    "response.persist": "persistir en audit/records/",
    "response.replaying": "ejecutando replay…",
    "response.replayDone": "replay completo",
    "response.replayFailed": "falló el replay",

    "records.h": "Registros",
    "records.lead": "Todos los registros de auditoría persistidos bajo <code>&lt;repo&gt;/audit/records/</code>. Selecciona dos para comparar.",
    "records.col.when": "Cuándo",
    "records.col.mode": "Modo",
    "records.col.context": "Contexto",
    "records.col.model": "Modelo",
    "records.col.grounded": "Fundado",
    "records.col.drift": "Drift",
    "records.col.recall": "Recall",
    "records.col.bundle": "Bundle",
    "records.diffSelected": "Comparar seleccionados →",
    "records.loading": "cargando…",
    "records.loadFailed": "no se pudieron cargar",
    "records.empty": "aún no hay registros",
    "records.selected": "seleccionados",

    "journal.h": "Diario",
    "journal.lead": "Una línea de tiempo legible de tus registros de auditoría — cada ejecución se muestra como una tarjeta con su título, resumen, nota y métricas. Los registros antiguos (sin título/resumen/nota) también aparecen como tarjetas, ancladas a su pregunta.",
    "journal.searchPh": "buscar título / resumen / nota / pregunta",
    "journal.loading": "cargando…",
    "journal.loadFailed": "no se pudieron cargar",
    "journal.empty": "aún no hay registros",
    "journal.noMatches": "sin coincidencias",
    "journal.brief": "Resumen",
    "journal.note": "Nota",
    "journal.question": "Pregunta",
    "journal.expand": "Mostrar fragmentos",
    "journal.collapse": "Ocultar fragmentos",
    "journal.fragmentsCount": "fragmentos",
    "journal.viewFile": "ver archivo",

    "search.h": "Buscar",
    "search.lead": "Encuentra ejecuciones anteriores por título, resumen, nota, pregunta, modo, hash del bundle, rutas de fragmentos o contenido de fragmentos. Subcadena insensible a mayúsculas — rápido y local.",
    "search.ph": "texto a buscar en tu historial de auditoría (Enter para buscar)",
    "search.btn": "Buscar",
    "search.scope": "Alcance",
    "search.metadata": "Metadatos",
    "search.paths": "Rutas",
    "search.content": "Contenido",
    "search.searching": "buscando…",
    "search.noResults": "sin resultados",
    "search.typeToSearch": "escribe algo y pulsa Enter.",
    "search.resultsCount": "resultados",

    "compare.h": "Comparar",
    "compare.lead": "Los deltas son B − A. El drift positivo es peor; grounded y recall positivos son mejores.",
    "compare.recA": "Registro A <input type=\"text\" id=\"cmp-a\" placeholder=\"audit/records/...\">",
    "compare.recB": "Registro B <input type=\"text\" id=\"cmp-b\" placeholder=\"audit/records/...\">",
    "compare.btn": "Calcular diff",
    "compare.computing": "calculando…",
    "compare.done": "listo",
    "compare.failed": "falló el diff",

    "guide.h": "Guía",
    "guide.pipeline.h": "Pipeline",
    "guide.pipeline.p": "NeuroFS es un pipeline: <strong>scan → pack → (Claude) → replay</strong>. Scan construye el índice SQLite en <code>.neurofs/index.db</code>. Pack rankea y comprime. Replay es el paso de gobernanza offline.",
    "guide.ranker.h": "Señales del ranker",
    "guide.ranker.filename": "<code>filename_match</code> — el token está en la ruta.",
    "guide.ranker.path": "<code>path_match</code> — coincidencia con el prefijo de foco.",
    "guide.ranker.symbol": "<code>symbol_match</code> — el token está en la tabla de símbolos (con stemming superficial).",
    "guide.ranker.import": "<code>import_match</code> — el token aparece en imports.",
    "guide.ranker.focus": "<code>focus</code> / <code>changed</code> — boosts de <code>--focus</code> / <code>--changed</code>.",
    "guide.audit.h": "Métricas de auditoría",
    "guide.audit.grounded": "<strong>grounded_ratio</strong> — fracción de citas que resuelven a un archivo dentro del bundle.",
    "guide.audit.drift": "<strong>drift_rate</strong> — símbolos desconocidos / (conocidos + desconocidos), repartidos en tres cubos:<ul><li><strong>paths</strong> — referencias tipo archivo que no están en el bundle.</li><li><strong>apis</strong> — nombres con punto (<code>jwt.sign</code>) que no están en el bundle.</li><li><strong>symbols</strong> — identificadores tipo código que no están en el bundle.</li></ul>",
    "guide.audit.recall": "<strong>answer_recall</strong> — fracción de cadenas <code>--facts</code> presentes en la respuesta (subcadena insensible a mayúsculas).",
    "guide.impl.h": "Lo que está implementado",
    "guide.impl.server": "Servidor HTTP local + UI embebida (<code>internal/ui</code>).",
    "guide.impl.endpoints": "Los endpoints envuelven scan, pack, replay, lista de records, diff y stats.",
    "guide.impl.bundle": "El bundle se mantiene en memoria entre <em>pack</em> y <em>replay</em>. Usa la ruta del snapshot para replay entre sesiones.",
    "guide.next.h": "Qué se recomienda a continuación",
    "guide.next.stream": "Progreso de scan en streaming (WebSocket / SSE) — hoy la UI se bloquea.",
    "guide.next.diff": "Visor de diff inline con color — hoy se renderiza como texto plano.",
    "guide.next.workspace": "Lista persistente de workspaces — hoy un repo a la vez en <code>localStorage</code>.",
    "guide.next.api": "Integración opcional con la API de Claude en vivo detrás de un toggle explícito.",

    "stats.title": "Estadísticas del pack",
    "stats.budget": "presupuesto",
    "stats.used": "usado",
    "stats.files": "archivos",
    "stats.fragments": "fragmentos",
    "stats.bundleHash": "hash del bundle",

    "replay.grounded": "Fundado",
    "replay.drift": "Drift",
    "replay.recall": "Recall",
    "replay.savedTo": "Guardado en",
    "replay.unknownPaths": "Rutas desconocidas",
    "replay.unknownApis": "APIs desconocidas",
    "replay.unknownSymbols": "Símbolos desconocidos",
    "replay.missingFacts": "Hechos faltantes",
    "replay.foundFacts": "Hechos encontrados",

    "common.loading": "cargando…",
    "common.searching": "buscando…",
    "common.error": "error",
    "common.errorPrefix": "error: ",
    "common.done": "listo",
    "common.copied": "copiado",
    "common.copyFailed": "fallo al copiar",
    "common.scanning": "escaneando…",
    "common.diffing": "comparando…",
    "common.doneIn": "listo en",
    "common.ms": "ms",
    "common.checking": "verificando…",

    "alert.setRepo": "Primero configura una ruta de repo en la pestaña Espacio.",
    "alert.enterPath": "Introduce una ruta absoluta.",
    "alert.tilde": "Usa la ruta absoluta completa — el navegador no expande `~`.",
    "alert.absolute": "La ruta del repo debe ser absoluta (empezar con /).",
    "alert.enterQuestion": "Introduce una pregunta.",
    "alert.pasteResponse": "Pega la respuesta del modelo.",
    "alert.snapshotRequired": "En modo snapshot la ruta del snapshot es obligatoria.",
    "alert.noInMemoryBundle": "No hay bundle en memoria. Vuelve a empaquetar con un nombre de snapshot, o cambia la fuente del bundle a snapshot.",
    "alert.needBothPaths": "Se necesitan las dos rutas de registros.",

    "home.workingOn": "Trabajando en",
    "home.filesIndexedSuffix": "archivos indexados",
    "home.indexNotReady": "índice no listo — abre Espacio para escanear",
    "home.repoSet": "Repo configurado. Ejecuta escaneo para indexar.",
    "home.repoRejected": "Repo rechazado — mira el error abajo.",

    "stat.packed": "empaquetado",

    "bundle.h": "Bundle",
    "bundle.tokens": "tokens",
    "bundle.filesIncluded": "archivos incluidos",
    "bundle.compression": "compresión",
    "bundle.snapshot": "snapshot",
    "bundle.notSaved": "no guardado (no se dio nombre de snapshot)",
    "bundle.noFragments": "sin fragmentos",
    "bundle.fragPath": "ruta",
    "bundle.fragRep": "representación",
    "bundle.fragTokens": "tokens",
    "bundle.fragScore": "puntaje",

    "pack.copiedClipboard": "copiado (plantilla + bundle)",
    "pack.clipboardDenied": "portapapeles denegado — usa descargar",

    "records.nSuffix": "registros",
    "records.selectedSuffix": "seleccionados",
    "records.filterSuffix": "filtro",
    "records.noMatchFilter": "ningún registro coincide con el filtro",
    "records.noneYet": "aún no hay registros — ejecuta un replay con \"persist\" activado",

    "journal.noWorkspace": "sin espacio de trabajo",
    "journal.zeroRecords": "0 registros",
    "journal.noneYetPersist": "Aún no hay registros. Ejecuta un replay y marca \"persist\".",
    "journal.filteredOutSuffix": "filtrados",
    "journal.noMatchFilter": "Ningún registro coincide con el filtro actual.",
    "journal.runSingle": "ejecución",
    "journal.runPlural": "ejecuciones",
    "journal.untitled": "(sin título)",
    "journal.nRecordsSuffix": "registros",
    "journal.expandBtnLabel": "Expandir",
    "journal.collapseBtnLabel": "Contraer",
    "journal.cmpALabel": "Usar como Comparar A",
    "journal.cmpBLabel": "Usar como Comparar B",
    "journal.copyPathLabel": "Copiar ruta",
    "journal.expandLoading": "cargando…",
    "journal.loadFail": "no se pudo cargar el registro",
    "journal.bundleFragmentsHeader": "Fragmentos del bundle",
    "journal.tokensSuffix": "tokens",
    "journal.tokSuffix": "tok",
    "journal.noFragmentsPersisted": "no se persistieron fragmentos en este registro",
    "journal.noContentPersisted": "no se persistió contenido para este fragmento — solo hay metadatos",
    "journal.when": "cuándo",
    "journal.bundle": "bundle",

    "audit.h": "Auditoría",
    "audit.title": "título",
    "audit.brief": "resumen",
    "audit.note": "nota",
    "audit.question": "pregunta",
    "audit.mode": "modo",
    "audit.model": "modelo",
    "audit.bundleHash": "hash del bundle",
    "audit.grounded": "fundado",
    "audit.citationsSuffix": "citas",
    "audit.drift": "drift",
    "audit.unknownOf": "desconocidos de",
    "audit.factRecall": "recall de hechos",
    "audit.record": "registro",
    "audit.unknownPaths": "rutas desconocidas",
    "audit.unknownApis": "apis desconocidas",
    "audit.unknownSymbols": "símbolos desconocidos",
    "audit.invalidCitations": "Citas inválidas",
    "audit.moreSuffix": "más",

    "search.openInJournal": "Abrir en Diario",
    "search.setRepo": "Configura un repo en la pestaña Espacio.",
    "search.noMatchFor": "sin coincidencias para",
    "search.underFilters": "con los filtros actuales.",
    "search.matchesSuffix": "coincidencias",
    "search.matchSuffix": "coincidencia",
    "search.ofMatchesShown": "coincidencias mostradas",
    "search.of": "de",
    "search.scoreLabel": "puntaje",

    "compare.diffH": "Diff",
    "compare.sameBundle": "mismo bundle",
    "compare.sameQuestion": "misma pregunta",
    "compare.sameModel": "mismo modelo",
    "compare.diffModeWarning": "(distintos — interpreta los deltas con cuidado)",
    "compare.groundedDelta": "fundado Δ",
    "compare.driftDelta": "drift Δ",
    "compare.recallDelta": "recall Δ",
    "compare.pathsBucket": "rutas",
    "compare.apisBucket": "apis",
    "compare.symbolsBucket": "símbolos",

    "mode.whenToUse": "Cuándo usarlo",
    "mode.expectedOutput": "Salida esperada",
    "mode.nextStep": "Siguiente paso",

    "mode.strategy.label": "Estrategia",
    "mode.strategy.subtitle": "Decide el enfoque antes de escribir código.",
    "mode.strategy.when": "Estás empezando una iteración y quieres un plan, no una implementación.",
    "mode.strategy.output": "Un diseño técnico corto, decisiones clave, archivos probables y un plan de tests mínimo.",
    "mode.strategy.next": "Lee el plan, acuerda el alcance y cambia a Construir para el cambio real.",

    "mode.build.label": "Construir",
    "mode.build.subtitle": "Implementa una iteración ya definida.",
    "mode.build.when": "Ya tienes un plan (de Estrategia o tuyo) y quieres código funcional.",
    "mode.build.output": "Una propuesta con forma de diff: archivos a cambiar, código, notas de test y cómo ejecutarlo.",
    "mode.build.next": "Pega la respuesta en la pestaña Respuesta y ejecuta replay; si fundado/drift pintan bien, aplica el diff.",

    "mode.review.label": "Revisar",
    "mode.review.subtitle": "Evalúa una respuesta, diff o propuesta antes de integrar.",
    "mode.review.when": "Tienes algo (un parche ajeno, una respuesta previa de Claude, un refactor) y necesitas una segunda lectura.",
    "mode.review.output": "Una revisión estructurada: qué es correcto, qué es arriesgado, qué parece alucinado, qué hacer después.",
    "mode.review.next": "Aplica los arreglos con los que estés de acuerdo, descarta el resto y, si hace falta, ejecuta una iteración de Construir.",

    "workspace.repoKv": "repo",
    "workspace.indexedSuffix": "indexados",
    "workspace.bytes": "bytes",
    "workspace.langBreakdown": "sin desglose por lenguaje",
    "workspace.auditAggregate": "Agregado de auditoría",
    "workspace.recordsSuffix": "registros",
    "workspace.recall": "recall",
  },
};

function landingReadLang() {
  const raw = (localStorage.getItem(LANDING_LANG_KEY) || "").toLowerCase();
  return raw.startsWith("es") ? "es" : "en";
}

function t(key) {
  const lang = landingReadLang();
  return (LANDING_DICT[lang] && LANDING_DICT[lang][key]) || LANDING_DICT.en[key] || "";
}

function applyLang(lang) {
  const normalized = lang === "es" ? "es" : "en";
  localStorage.setItem(LANDING_LANG_KEY, normalized);
  document.documentElement.setAttribute("lang", normalized);

  document.querySelectorAll("[data-i18n]").forEach(el => {
    const key = el.getAttribute("data-i18n");
    const value = (LANDING_DICT[normalized] && LANDING_DICT[normalized][key]) || LANDING_DICT.en[key];
    if (typeof value === "string") el.textContent = value;
  });
  document.querySelectorAll("[data-i18n-html]").forEach(el => {
    const key = el.getAttribute("data-i18n-html");
    const value = (LANDING_DICT[normalized] && LANDING_DICT[normalized][key]) || LANDING_DICT.en[key];
    if (typeof value === "string") el.innerHTML = value;
  });
  document.querySelectorAll("[data-i18n-placeholder]").forEach(el => {
    const key = el.getAttribute("data-i18n-placeholder");
    const value = (LANDING_DICT[normalized] && LANDING_DICT[normalized][key]) || LANDING_DICT.en[key];
    if (typeof value === "string") el.setAttribute("placeholder", value);
  });
  document.querySelectorAll(".lang-toggle button[data-lang]").forEach(btn => {
    btn.classList.toggle("active", btn.dataset.lang === normalized);
    btn.setAttribute("aria-pressed", btn.dataset.lang === normalized ? "true" : "false");
  });

  // Re-render any dynamic UI that was already produced by the app code.
  if (typeof rerenderAfterLang === "function") rerenderAfterLang();
}

document.querySelectorAll(".lang-toggle button[data-lang]").forEach(btn => {
  btn.addEventListener("click", () => applyLang(btn.dataset.lang));
});

// ---------- how-it-works modal ----------

const landingHowtoEl = document.getElementById("landing-howto");
const LANDING_STEPS = 3;
let landingStep = 1;
let landingLastFocus = null;

function landingSetStep(n) {
  landingStep = Math.min(LANDING_STEPS, Math.max(1, n));
  if (!landingHowtoEl) return;
  landingHowtoEl.querySelectorAll(".landing-modal-step").forEach(el => {
    el.classList.toggle("landing-modal-step--active", Number(el.dataset.step) === landingStep);
  });
  landingHowtoEl.querySelectorAll(".landing-modal-dot").forEach(dot => {
    dot.classList.toggle("landing-modal-dot--active", Number(dot.dataset.goStep) === landingStep);
  });
  const prev = landingHowtoEl.querySelector("[data-modal-prev]");
  const next = landingHowtoEl.querySelector("[data-modal-next]");
  const finish = landingHowtoEl.querySelector("[data-modal-finish]");
  if (prev)   prev.hidden   = landingStep === 1;
  if (next)   next.hidden   = landingStep === LANDING_STEPS;
  if (finish) finish.hidden = landingStep !== LANDING_STEPS;
}

function landingOpenHowto() {
  if (!landingHowtoEl) return;
  landingLastFocus = document.activeElement;
  landingHowtoEl.hidden = false;
  landingHowtoEl.setAttribute("aria-hidden", "false");
  landingSetStep(1);
  const firstBtn = landingHowtoEl.querySelector("[data-modal-next], [data-modal-finish], .landing-modal-close");
  if (firstBtn) firstBtn.focus();
}

function landingCloseHowto() {
  if (!landingHowtoEl) return;
  landingHowtoEl.hidden = true;
  landingHowtoEl.setAttribute("aria-hidden", "true");
  if (landingLastFocus && typeof landingLastFocus.focus === "function") landingLastFocus.focus();
}

const landingHowtoOpenBtn = document.getElementById("landing-howto-open");
if (landingHowtoOpenBtn) landingHowtoOpenBtn.addEventListener("click", landingOpenHowto);

if (landingHowtoEl) {
  landingHowtoEl.querySelectorAll("[data-howto-close]").forEach(el => {
    el.addEventListener("click", landingCloseHowto);
  });
  const prevBtn = landingHowtoEl.querySelector("[data-modal-prev]");
  const nextBtn = landingHowtoEl.querySelector("[data-modal-next]");
  if (prevBtn) prevBtn.addEventListener("click", () => landingSetStep(landingStep - 1));
  if (nextBtn) nextBtn.addEventListener("click", () => landingSetStep(landingStep + 1));
  landingHowtoEl.querySelectorAll(".landing-modal-dot").forEach(dot => {
    dot.addEventListener("click", () => landingSetStep(Number(dot.dataset.goStep)));
  });
  const finishBtn = landingHowtoEl.querySelector("[data-modal-finish]");
  if (finishBtn) finishBtn.addEventListener("click", landingCloseHowto);
}

document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if (landingHowtoEl && !landingHowtoEl.hidden) landingCloseHowto();
});

// ------------------------------ resume (parent-context reuse) ------------------------------
//
// Flow: click "Resume from this" on a Journal card → fetch the seed →
// prefill New task (title/brief/question/mode/focus textarea) → render
// the breadcrumb banner → switch tabs. The focus paths are editable
// before pack; whatever sits in the textarea at pack time is what the
// ranker boosts AND what gets stamped into the child record.

async function resumeFromRecord(path) {
  if (!requireRepo()) return;
  try {
    const seed = await j(
      "GET",
      `/api/resume-seed?repo=${encodeURIComponent(state.repo)}&path=${encodeURIComponent(path)}`,
    );
    applyResumeSeed(seed);
    switchTab("task");
  } catch (e) {
    alert("Resume failed: " + e.message);
  }
}

function applyResumeSeed(seed) {
  state.resume = {
    parentPath:  seed.parent_path,
    parentTitle: seed.title || seed.question || "(untitled)",
    focusPaths:  (seed.suggested_focus_paths || []).slice(),
  };
  // Prefill Title/Brief/Question only when empty so a user who started
  // typing before clicking Resume doesn't lose their edits. Mode is
  // always applied — the resume UX implies "continue in the same mode
  // unless you change it".
  const tEl = document.getElementById("q-title");
  const b = document.getElementById("q-brief");
  const q = document.getElementById("q-input");
  if (!tEl.value.trim()) tEl.value = seed.title || "";
  if (!b.value.trim()) b.value = seed.brief || "";
  if (!q.value.trim()) q.value = seed.question || "";
  if (seed.mode) applyMode(seed.mode.toLowerCase(), { preserveUserEdits: true });
  renderResumeBanner();
}

function clearResume() {
  state.resume = null;
  renderResumeBanner();
}

function renderResumeBanner() {
  const el = document.getElementById("resume-banner");
  if (!el) return;
  if (!state.resume) {
    el.hidden = true;
    const focus = document.getElementById("resume-focus");
    if (focus) focus.value = "";
    return;
  }
  el.hidden = false;
  document.getElementById("resume-title").textContent = state.resume.parentTitle;
  const pathEl = document.getElementById("resume-parent-path");
  pathEl.textContent = state.resume.parentPath;
  pathEl.title = state.resume.parentPath;
  document.getElementById("resume-focus").value =
    (state.resume.focusPaths || []).join("\n");
}

function readResumeFocusPaths() {
  const raw = (document.getElementById("resume-focus") || {}).value || "";
  const out = [];
  const seen = new Set();
  for (const line of raw.split(/\r?\n/)) {
    const tt = line.trim();
    if (!tt || seen.has(tt)) continue;
    seen.add(tt);
    out.push(tt);
  }
  return out;
}

(() => {
  const btn = document.getElementById("resume-clear");
  if (btn) btn.addEventListener("click", clearResume);
})();

// ------------------------------ init ------------------------------

applyLang(landingReadLang());
applyMode(state.mode);
switchTab("home");

// Bootstrap: when the user has nothing in localStorage yet, prefill the
// repo input with the directory the binary was launched from. Best-
// effort — a missing endpoint (older binary) or a network blip just
// leaves the field blank, which is the legacy behaviour anyway.
(async () => {
  if (state.repo) return;
  try {
    const r = await j("GET", "/api/bootstrap");
    const cwd = (r && r.cwd) ? String(r.cwd).trim() : "";
    if (!cwd || !cwd.startsWith("/")) return;
    const input = document.getElementById("repo-input");
    if (input && !input.value) input.value = cwd;
  } catch {
    // Silent: the user can still paste a path manually.
  }
})();
