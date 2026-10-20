package setup

// SetupInfoReply is the reply of setup-info --json, version 2: the provider's
// private root and identity, the package the selection binds, and every
// package this build can run, in table order.
type SetupInfoReply struct {
	Version          int                `json:"version"`
	ProviderRoot     string             `json:"provider_root"`
	ProviderID       string             `json:"provider_id"`
	SelectionPresent bool               `json:"selection_present"`
	ActivePackage    *ActivePackageInfo `json:"active_package,omitempty"`
	KnownPackages    []KnownPackageInfo `json:"known_packages"`
	// StagedModels are the picture models the selection serves, the starter
	// model first; empty before setup. The provider is the authority for
	// this list; Setup and the runtime read it here.
	StagedModels []string `json:"staged_models"`
	// DownloadDir is where Setup writes model downloads: on the models
	// folder's volume, so configure and model add publish each by rename.
	DownloadDir string `json:"download_dir"`
}

// ActivePackageInfo identifies the selected package and its executable.
type ActivePackageInfo struct {
	Version string `json:"version"`
	Program string `json:"program"`
}

// KnownPackageInfo records the reviewed package and runtime pairing pins.
type KnownPackageInfo struct {
	Version              string `json:"version"`
	ArchiveSHA256        string `json:"archive_sha256"`
	ProgramSHA256        string `json:"program_sha256"`
	BackendArchiveSHA256 string `json:"backend_archive_sha256"`
	ModelRevision        string `json:"model_revision"`
}

// PackageCheckReply reports the checked package pairing.
type PackageCheckReply struct {
	Version              int    `json:"version"`
	OK                   bool   `json:"ok"`
	BackendArchiveSHA256 string `json:"backend_archive_sha256"`
	ModelRevision        string `json:"model_revision"`
}

// PackageActivationReply reports the newly selected version.
type PackageActivationReply struct {
	Version       int    `json:"version"`
	OK            bool   `json:"ok"`
	ActiveVersion string `json:"active_version"`
}

// VerificationReply reports whether the installed files verified.
type VerificationReply struct {
	Version  int  `json:"version"`
	Verified bool `json:"verified"`
}
