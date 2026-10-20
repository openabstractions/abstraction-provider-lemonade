//go:build !windows

package server

import "errors"

// The reviewed Lemonade package is Windows x64 only.
func systemWindowsDir() (string, error) {
	return "", errors.New("lemonade: the reviewed package runs on Windows only")
}
