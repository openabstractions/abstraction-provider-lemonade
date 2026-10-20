package server

import "golang.org/x/sys/windows"

// systemWindowsDir asks the kernel for the Windows folder, so an inherited
// SystemRoot or PATH cannot redirect the child to planted system files.
func systemWindowsDir() (string, error) { return windows.GetSystemWindowsDirectory() }
