// app.js — vanilla client for the NeuroFS local UI.
// No framework. Tabs are show/hide; state is kept in module-level vars plus
// localStorage for the repo path. Every network call goes through j().

const state = {
  repo: localStorage.getItem("neurofs.repo") || "",
  lastBundlePath: null, // snapshot path from the last pack, if any
  lastPrompt: "",
  lastPackStats: null,
  selectedRecords: [], // paths checked in the records tab
};

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
}

document.querySelectorAll("nav#tabs button").forEach(b => {
  b.addEventListener("click", () => switchTab(b.dataset.tab));
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
    state.lastPrompt = r.prompt;
    state.lastBundlePath = r.bundle_path || null;
    state.lastPackStats = r.stats;
    document.getElementById("pack-prompt").textContent = r.prompt;
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

document.getElementById("copy-prompt").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(state.lastPrompt);
    document.getElementById("pack-status").textContent = "copied to clipboard";
  } catch {
    document.getElementById("pack-status").textContent = "clipboard denied — use download";
  }
});

document.getElementById("download-prompt").addEventListener("click", () => {
  const blob = new Blob([state.lastPrompt], { type: "text/plain" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = "neurofs.prompt.txt";
  a.click();
});

document.getElementById("go-response").addEventListener("click", () => switchTab("response"));

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
      facts: document.getElementById("r-facts").value.trim(),
      save: document.getElementById("r-save").checked,
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

  document.getElementById("replay-report").innerHTML = `
    <h3>Audit</h3>
    <dl class="kv">
      <dt>question</dt><dd>${esc(rec.question || "—")}</dd>
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
  const tbody = document.querySelector("#records-table tbody");
  status.textContent = "loading…"; tbody.innerHTML = "";
  try {
    const r = await j("GET", `/api/records?repo=${encodeURIComponent(state.repo)}`);
    if (!r.records || !r.records.length) {
      tbody.innerHTML = `<tr><td colspan="8" class="muted" style="padding:1rem;text-align:center">no records yet — run a replay and enable "persist"</td></tr>`;
      status.textContent = "";
      return;
    }
    state.selectedRecords = [];
    tbody.innerHTML = r.records.map(rec => `
      <tr>
        <td><input type="checkbox" data-path="${esc(rec.path)}" class="rec-check"></td>
        <td>${esc(rec.timestamp)}</td>
        <td>${esc((rec.question || "").slice(0, 50))}</td>
        <td>${esc(rec.model)}</td>
        <td>${fmtPct(rec.grounded_ratio)}</td>
        <td>${fmtPct(rec.drift_rate)}</td>
        <td>${rec.expects_facts ? fmtPct(rec.answer_recall) : "—"}</td>
        <td><code>${esc((rec.bundle_hash || "").slice(0, 10))}</code></td>
      </tr>`).join("");
    document.querySelectorAll(".rec-check").forEach(c =>
      c.addEventListener("change", onRecSelect));
    status.textContent = `${r.records.length} records`;
  } catch (e) {
    status.textContent = "error: " + e.message;
  }
}

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

  document.getElementById("compare-report").innerHTML = `
    <h3>Diff</h3>
    <dl class="kv">
      <dt>same bundle</dt><dd>${d.same_bundle ? "yes" : "<span class=\"delta-neg\">no</span>"}</dd>
      <dt>same question</dt><dd>${d.same_question ? "yes" : "<span class=\"delta-neg\">no</span>"}</dd>
      <dt>same model</dt><dd>${d.same_model ? "yes" : "<span class=\"delta-neg\">no</span>"}</dd>
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

switchTab("home");
