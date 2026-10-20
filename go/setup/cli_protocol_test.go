package setup

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestSetupCLIHandsOffExactArgumentsAndReply(t *testing.T) {
	ctx := t.Context()
	paths := BF16Files{Main: "main", Encoder: "encoder", VAE: "vae"}
	var argv [][]string
	cli := SetupCLI(func(got context.Context, args ...string) ([]byte, error) {
		if got != ctx {
			t.Fatal("the caller context changed")
		}
		argv = append(argv, slices.Clone(args))
		return []byte("exact reply\n"), nil
	})
	commands := []struct {
		invoke func() ([]byte, error)
		want   []string
	}{
		{func() ([]byte, error) { return cli.ReadSetupInfo(ctx) }, []string{"setup-info", "--json"}},
		{func() ([]byte, error) { return cli.VerifySetup(ctx) }, []string{"verify-setup", "--json"}},
		{func() ([]byte, error) { return cli.InstallReviewedEngineFiles(ctx, "backend") },
			[]string{"configure", "--activate-reviewed-package", "--engine-files-only", "--vulkan-backend-archive", "backend"}},
		{func() ([]byte, error) { return cli.ConfigureReviewedPicture(ctx, "v1", paths, "backend", true) },
			[]string{"configure", "--activate-reviewed-package", "--package", "v1", "--bf16-main", "main", "--bf16-encoder", "encoder", "--bf16-vae", "vae", "--vulkan-backend-archive", "backend", "--replace-backend", "--replace-private-model"}},
		{func() ([]byte, error) {
			return cli.AddPictureModel(ctx, "picture", map[string]string{"vae": "v", "main": "m"})
		},
			[]string{"model", "add", "--id", "picture", "--file", "main=m", "--file", "vae=v"}},
		{func() ([]byte, error) { return cli.CheckPackage(ctx, "v1", "root") }, []string{"package", "check", "--version", "v1", "--root", "root", "--json"}},
		{func() ([]byte, error) { return cli.ActivatePackage(ctx, "v1", "backend") }, []string{"package", "activate", "--version", "v1", "--vulkan-backend-archive", "backend"}},
		{func() ([]byte, error) { return cli.ActivatePackage(ctx, "v1", "") }, []string{"package", "activate", "--version", "v1"}},
		{func() ([]byte, error) { return cli.RetirePackage(ctx, "v1") }, []string{"package", "retire", "--version", "v1"}},
	}
	for i, command := range commands {
		reply, err := command.invoke()
		if err != nil || string(reply) != "exact reply\n" || len(argv) != i+1 || !slices.Equal(argv[i], command.want) {
			t.Fatalf("command %d handoff %v, reply %q (%v)", i, argv, reply, err)
		}
	}
}

func TestCLIReplyEncoding(t *testing.T) {
	tests := []struct {
		name  string
		reply any
		want  string
	}{
		{"setup-info empty selection", SetupInfoReply{Version: 2, KnownPackages: []KnownPackageInfo{}, StagedModels: []string{}}, `{"version":2,"provider_root":"","provider_id":"","selection_present":false,"known_packages":[],"staged_models":[],"download_dir":""}`},
		{"setup-info active selection", SetupInfoReply{
			Version: 2, ProviderRoot: "root", ProviderID: "provider", SelectionPresent: true,
			ActivePackage: &ActivePackageInfo{Version: "v1", Program: "engine"},
			KnownPackages: []KnownPackageInfo{{Version: "v1", ArchiveSHA256: "archive", ProgramSHA256: "program", BackendArchiveSHA256: "backend", ModelRevision: "model"}},
			StagedModels:  []string{"first", "second"}, DownloadDir: "downloads",
		}, `{"version":2,"provider_root":"root","provider_id":"provider","selection_present":true,"active_package":{"version":"v1","program":"engine"},"known_packages":[{"version":"v1","archive_sha256":"archive","program_sha256":"program","backend_archive_sha256":"backend","model_revision":"model"}],"staged_models":["first","second"],"download_dir":"downloads"}`},
		{"verify", VerificationReply{Version: 1, Verified: true}, `{"version":1,"verified":true}`},
		{"package check", PackageCheckReply{Version: 1, OK: true, BackendArchiveSHA256: "backend", ModelRevision: "model"}, `{"version":1,"ok":true,"backend_archive_sha256":"backend","model_revision":"model"}`},
		{"package activate", PackageActivationReply{Version: 1, OK: true, ActiveVersion: "v1"}, `{"version":1,"ok":true,"active_version":"v1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.reply)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tt.want {
				t.Fatalf("encoded %s, want %s", encoded, tt.want)
			}
		})
	}
}
