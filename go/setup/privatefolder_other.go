//go:build !windows

package setup

import (
	"fmt"
	"os"
	"syscall"
)

// isRemoteDrive reports false: drive letters are Windows'.
func isRemoteDrive(string) (bool, error) { return false, nil }

// folderIdentity is dir's device and inode, the same for every path that
// reaches the folder.
func folderIdentity(dir string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrSelection, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("%w: %s has no device and inode", ErrSelection, dir)
	}
	return fmt.Sprintf("%x-%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}

// checkParents refuses dir when an existing parent below the root is owned
// by an account other than this one or root.
func checkParents(dir string) error {
	for parent := parentOf(dir); parent != parentOf(parent); parent = parentOf(parent) {
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: the owner of %s: %w", ErrSelection, parent, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Getuid() && stat.Uid != 0 {
			return fmt.Errorf("%w: %s is owned by another account", ErrSelection, parent)
		}
	}
	return nil
}
