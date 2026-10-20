package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type imageStagingRefusal struct{ detail string }

func (e *imageStagingRefusal) Error() string { return "lemonade: image model files: " + e.detail }

// ImageStaged checks the reviewed image model's files before engine admission.
// Launch retains its existing byte verification and held-file checks.
func (s *Server) ImageStaged(ctx context.Context, model string) error {
	for _, declared := range ImageModels {
		if declared.ID != model {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, file := range declared.Files {
			path := filepath.Join(s.cfg.ModelsDir, file.CachePath())
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() != file.Bytes {
				return &imageStagingRefusal{detail: fmt.Sprintf("%s has no staged %s file", model, file.Role)}
			}
		}
		return nil
	}
	// Existing operator-defined models keep their established launch checks.
	return nil
}
