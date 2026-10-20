package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DownloadDir is the provider's staging area for model downloads under
// root: Setup writes each verified download there, on the volume of the
// models folder, and a setup step publishes it into the models folder by
// rename. The server reads only the models folder, so a partial download
// here is never served.
func DownloadDir(root string) string {
	return filepath.Join(root, "engine-downloads")
}

// renameFile moves a file within a volume; tests replace it.
var renameFile = os.Rename

// placements are the downloads one setup step has moved into the provider's
// folders. A step that fails calls restore, which returns each download
// that has not been published to where it was, so a retry finds it again.
type placements []struct{ source, dest string }

// place moves pin's download to dest by rename and verifies it there. OA
// never makes a copy of a model for an engine (VISION 2026-10-01): a
// download on another volume is refused and stays where it was. A download
// that does not verify goes back to its source and is refused.
func (p *placements) place(ctx context.Context, pin weightPin, dest string, progress func(int64) error) error {
	if !filepath.IsAbs(pin.source) || pin.source == "" {
		return fmt.Errorf("%w: missing approved weight", ErrSelection)
	}
	info, err := os.Lstat(pin.source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: approved weight source", ErrSelection)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%w: %s is already staged", ErrSelection, filepath.Base(dest))
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := renameFile(pin.source, dest); err != nil {
		return fmt.Errorf("%w: the download %s is published by rename and must be on the volume of the provider's models folder (%v); download it again in Setup, which writes into the provider's download folder", ErrSelection, filepath.Base(pin.source), err)
	}
	*p = append(*p, struct{ source, dest string }{pin.source, dest})
	if err := checkDigestCtx(ctx, dest, pin.digest, progress); err != nil {
		(*p)[len(*p)-1:].restore()
		return err
	}
	return nil
}

// restore moves every placed download back to its source, latest first.
// A download already published under its final name is not at dest any
// more and stays published.
func (p placements) restore() {
	for i := len(p) - 1; i >= 0; i-- {
		if _, err := os.Lstat(p[i].dest); err == nil {
			//unchecked: restore runs on a step that already failed; a download that cannot move back is downloaded again
			_ = os.Rename(p[i].dest, p[i].source)
		}
	}
}
