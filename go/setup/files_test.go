package setup

import "github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"

// openFolders stands for the kit's read-only folder ACL in this package's
// tests, whose temporary folders must stay removable; the kit's own tests
// set and check the real ACL.
type openFolders struct{}

func (openFolders) Restrict([]string) error                 { return nil }
func (openFolders) Check(string, *enginefiles.Handle) error { return nil }

func init() { backendFiles = enginefiles.NewVerifier(enginefiles.Options{ACL: openFolders{}}) }
