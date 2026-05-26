package cli

import (
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

// loadProjectInfo is a thin wrapper around taskflow.LoadProjectInfo so the
// cli/ package can call it as a package-private name without each command
// having to take a taskflow import.
func loadProjectInfo(db *storage.DB) *project.Info {
	return taskflow.LoadProjectInfo(db)
}
