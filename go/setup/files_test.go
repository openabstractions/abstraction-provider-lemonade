package setup

import "github.com/openabstractions/abstraction-facade/go/programfiles"

// openFolders stands for the kit's read-only folder ACL in this package's
// tests, whose temporary folders must stay removable; the kit's own tests
// set and check the real ACL.
type openFolders struct{}

func (openFolders) Restrict([]string) error                  { return nil }
func (openFolders) Check(string, *programfiles.Handle) error { return nil }

func init() { backendFiles = programfiles.NewVerifier(programfiles.Options{ACL: openFolders{}}) }
