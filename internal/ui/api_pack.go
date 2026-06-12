package ui

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

// --------------------- /api/pack ---------------------

type packReq struct {
	Repo             string `json:"repo"`
	Query            string `json:"query"`
	Budget           int    `json:"budget"`
	Focus            string `json:"focus"`
	Changed          bool   `json:"changed"`
	MaxFiles         int    `json:"max_files"`
	MaxFragments     int    `json:"max_fragments"`
	PreferSignatures bool   `json:"prefer_signatures"`
	StripComments    bool   `json:"strip_comments"`
	StripBlankLines  bool   `json:"strip_blank_lines"`
	SnapshotName     string `json:"snapshot_name"` // optional — when empty, a default path under .neurofs/ui/ is used
	// InheritedFocus carries the focus paths the user kept from a parent
	// record. Merged with Focus server-side so the ranker sees one list and
	// legacy clients (no parent) remain unchanged.
	InheritedFocus []string `json:"inherited_focus"`
}

type packResp struct {
	Prompt     string             `json:"prompt"`
	Stats      models.BundleStats `json:"stats"`
	BundlePath string             `json:"bundle_path,omitempty"`
	Fragments  []fragmentView     `json:"fragments"`
	Query      string             `json:"query"`
}

type fragmentView struct {
	RelPath        string  `json:"rel_path"`
	Representation string  `json:"representation"`
	Tokens         int     `json:"tokens"`
	Score          float64 `json:"score"`
}

func handlePack(w http.ResponseWriter, r *http.Request) {
	var req packReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}
	if req.Budget <= 0 {
		req.Budget = config.DefaultBudget
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "open index (did you run scan?): "+err.Error())
		return
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load files: "+err.Error())
		return
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "index is empty — run scan first")
		return
	}

	embClient := embeddings.NewClient(cfg.HybridMode)
	queryEmb, _ := embClient.GetEmbedding(context.Background(), req.Query)
	fileEmbs, _ := db.AllEmbeddings()

	rels, _ := db.AllRelations()
	rankOpts := ranking.Options{
		Project:        taskflow.LoadProjectInfo(db),
		Focus:          mergeFocus(req.Focus, req.InheritedFocus),
		QueryEmbedding: queryEmb,
		Embeddings:     fileEmbs,
		Relations:      rels,
	}
	if req.Changed {
		rankOpts.ChangedFiles = gitChangedFiles(cfg.RepoRoot)
	}
	ranked := ranking.RankWithOptions(files, req.Query, rankOpts)

	bundle, err := packager.Pack(ranked, req.Query, packager.Options{
		Budget:           req.Budget,
		MaxFiles:         req.MaxFiles,
		MaxFragments:     req.MaxFragments,
		PreferSignatures: req.PreferSignatures,
		StripComments:    req.StripComments,
		StripBlankLines:  req.StripBlankLines,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pack: "+err.Error())
		return
	}

	// Render the Claude-shaped prompt so the UI can let the user copy it
	// verbatim. Using the same output.WriteClaude path means what the UI
	// shows is exactly what the CLI would emit.
	var promptBuf bytes.Buffer
	if err := output.WriteClaude(&promptBuf, bundle, taskflow.BuildRepoSummary(cfg.RepoRoot, files, taskflow.LoadProjectInfo(db))); err != nil {
		writeErr(w, http.StatusInternalServerError, "render prompt: "+err.Error())
		return
	}

	// Always persist the bundle to a snapshot so replay can audit it later,
	// even across page reloads. If the user supplied a name, we also copy
	// the snapshot there for long-term record-keeping.
	bundle = taskflow.EnrichBundle(bundle, cfg.RepoRoot)
	defaultPath := filepath.Join(cfg.DBDir(), "ui", "last.bundle.json")
	if err := taskflow.WriteBundleJSON(defaultPath, bundle); err != nil {
		writeErr(w, http.StatusInternalServerError, "save default bundle: "+err.Error())
		return
	}
	snapshotPath := defaultPath
	if name := strings.TrimSpace(req.SnapshotName); name != "" {
		target, err := fsutil.ConfineToRepo(cfg.RepoRoot, name)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "snapshot_name: "+err.Error())
			return
		}
		if err := taskflow.WriteBundleJSON(target, bundle); err != nil {
			writeErr(w, http.StatusInternalServerError, "save snapshot: "+err.Error())
			return
		}
		snapshotPath = target
	}

	frags := make([]fragmentView, 0, len(bundle.Fragments))
	for _, f := range bundle.Fragments {
		frags = append(frags, fragmentView{
			RelPath:        f.RelPath,
			Representation: string(f.Representation),
			Tokens:         f.Tokens,
			Score:          f.Score,
		})
	}

	writeJSON(w, http.StatusOK, packResp{
		Prompt:     promptBuf.String(),
		Stats:      bundle.Stats,
		BundlePath: snapshotPath,
		Fragments:  frags,
		Query:      req.Query,
	})
}

// --------------------- /api/task ---------------------

type taskReq struct {
	Repo          string `json:"repo"`
	Query         string `json:"query"`
	Budget        int    `json:"budget"`
	Force         bool   `json:"force"`
	DisableChunks bool   `json:"disable_chunks"`
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	var req taskReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	// mustRepo enforces the same absolute-path / valid-config rules as
	// every other handler, so the bootstrap-suggested cwd cannot be
	// silently massaged into something exotic.
	if _, ok := mustRepo(w, req.Repo); !ok {
		return
	}

	res, err := taskflow.Run(taskflow.Opts{
		RepoRoot:      req.Repo,
		Query:         req.Query,
		Budget:        req.Budget,
		Force:         req.Force,
		DisableChunks: req.DisableChunks,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --------------------- pack helper ---------------------

func gitChangedFiles(repoRoot string) []string {
	return fsutil.GitChangedFiles(repoRoot)
}

// mergeFocus joins the user-typed focus CSV with the inherited list carried
// over from a parent record. Dedupe is case-sensitive (paths are case-
// sensitive on most SSD checkouts) and order is preserved: user entries
// first, inherited after. The ranker's parseFocusPrefixes handles whitespace
// and empty segments, so we just emit a clean comma-joined string.
func mergeFocus(user string, inherited []string) string {
	out := make([]string, 0, len(inherited)+4)
	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, part := range strings.Split(user, ",") {
		add(part)
	}
	for _, p := range inherited {
		add(p)
	}
	return strings.Join(out, ",")
}
