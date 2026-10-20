package setup

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// remoteDrive reports whether an absolute drive path lies on a network
// drive; tests replace it.
var remoteDrive = isRemoteDrive

// EnsurePrivateFolder admits dir as a folder lemonaded --state or --dir
// names: an absolute local path, never a UNC share (\\host\share), a device
// path (\\?\, \\.\, \??\, pipes included) or a mapped network drive, and
// every existing parent below the volume root owned by this account, SYSTEM
// or Administrators (root elsewhere). An
// absent folder is created private to this account; an existing one, and
// the created one, must be private: owned by this account, with no access
// for any other account but SYSTEM, and no link or junction
// (abstraction-identity's go/privatedir).
func EnsurePrivateFolder(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q is not an absolute folder", ErrSelection, dir)
	}
	slashed := filepath.ToSlash(dir)
	if strings.HasPrefix(slashed, "//") || strings.HasPrefix(slashed, "/??/") {
		return fmt.Errorf("%w: %s is a network share or device path; name a local folder", ErrSelection, dir)
	}
	if remote, err := remoteDrive(dir); err != nil || remote {
		if err == nil {
			err = fmt.Errorf("%s is on a network drive; name a local folder", dir)
		}
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	if err := checkParents(dir); err != nil {
		return err
	}
	if err := privatedir.Ensure(filepath.Clean(dir)); err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	return nil
}

// parentOf is filepath.Dir of the cleaned path.
func parentOf(path string) string { return filepath.Dir(filepath.Clean(path)) }
