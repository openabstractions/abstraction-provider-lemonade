package setup

import "context"

// SetupCLI is the existing private provider protocol over a caller-owned
// command. The caller supplies process and package admission.
type SetupCLI func(context.Context, ...string) ([]byte, error)

type setupCLIWords struct {
	Info, Verify, Configure, Package, Model             string
	Check, Activate, Retire, Add                        string
	JSON, Version, Root, VulkanBackend                  string
	Reviewed, EngineFiles, ReplaceBackend, ReplaceModel string
	Main, Encoder, VAE, ModelID, ModelFile              string
	ModelRoles                                          [3]string
	RefusedExit, HeldExit                               int
}

// SetupCLIWords returns the fixed words shared by the installed CLI parser
// and its Setup caller. This value conveys no process or package authority.
func SetupCLIWords() setupCLIWords {
	return setupCLIWords{
		Info: "setup-info", Verify: "verify-setup", Configure: "configure", Package: "package", Model: "model",
		Check: "check", Activate: "activate", Retire: "retire", Add: "add",
		JSON: "json", Version: "version", Root: "root", VulkanBackend: "vulkan-backend-archive",
		Reviewed: "activate-reviewed-package", EngineFiles: "engine-files-only",
		ReplaceBackend: "replace-backend", ReplaceModel: "replace-private-model",
		Main: "bf16-main", Encoder: "bf16-encoder", VAE: "bf16-vae", ModelID: "id", ModelFile: "file",
		ModelRoles: [3]string{"main", "text_encoder", "vae"}, RefusedExit: 2, HeldExit: 3,
	}
}

func (run SetupCLI) ReadSetupInfo(ctx context.Context) ([]byte, error) {
	words := SetupCLIWords()
	return run(ctx, words.Info, "--"+words.JSON)
}

func (run SetupCLI) VerifySetup(ctx context.Context) ([]byte, error) {
	words := SetupCLIWords()
	return run(ctx, words.Verify, "--"+words.JSON)
}

func (run SetupCLI) InstallReviewedEngineFiles(ctx context.Context, archive string) ([]byte, error) {
	words := SetupCLIWords()
	return run(ctx, words.Configure, "--"+words.Reviewed, "--"+words.EngineFiles, "--"+words.VulkanBackend, archive)
}

func (run SetupCLI) ConfigureReviewedPicture(ctx context.Context, version string, paths BF16Files, archive string, replace bool) ([]byte, error) {
	words := SetupCLIWords()
	args := []string{words.Configure, "--" + words.Reviewed, "--" + words.Package, version, "--" + words.Main,
		paths.Main, "--" + words.Encoder, paths.Encoder, "--" + words.VAE, paths.VAE, "--" + words.VulkanBackend, archive}
	if replace {
		args = append(args, "--"+words.ReplaceBackend, "--"+words.ReplaceModel)
	}
	return run(ctx, args...)
}

func (run SetupCLI) AddPictureModel(ctx context.Context, model string, files map[string]string) ([]byte, error) {
	words := SetupCLIWords()
	args := []string{words.Model, words.Add, "--" + words.ModelID, model}
	for _, role := range words.ModelRoles {
		if path, ok := files[role]; ok {
			args = append(args, "--"+words.ModelFile, role+"="+path)
		}
	}
	return run(ctx, args...)
}

func (run SetupCLI) CheckPackage(ctx context.Context, version, root string) ([]byte, error) {
	words := SetupCLIWords()
	return run(ctx, words.Package, words.Check, "--"+words.Version, version, "--"+words.Root, root, "--"+words.JSON)
}

func (run SetupCLI) ActivatePackage(ctx context.Context, version, archive string) ([]byte, error) {
	words := SetupCLIWords()
	args := []string{words.Package, words.Activate, "--" + words.Version, version}
	if archive != "" {
		args = append(args, "--"+words.VulkanBackend, archive)
	}
	return run(ctx, args...)
}

func (run SetupCLI) RetirePackage(ctx context.Context, version string) ([]byte, error) {
	words := SetupCLIWords()
	return run(ctx, words.Package, words.Retire, "--"+words.Version, version)
}
