package cli

import (
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/storage"
)

// loadProjectInfo reads the project.Info previously persisted by `scan` from
// the metadata table. Returns nil when absent or invalid — callers treat nil
// as "no project metadata available" and fall back to defaults.
func loadProjectInfo(db *storage.DB) *project.Info {
	raw, ok, err := db.GetMeta(indexer.ProjectMetaKey)
	if err != nil || !ok {
		return nil
	}
	return project.Decode(raw)
}
