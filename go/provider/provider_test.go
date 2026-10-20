package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	router "github.com/openabstractions/abstraction-router/go"
)

// The provider's host declares lemonade/seed for the image profile alone,
// the key the runtime's declaration names and server.ImageFields carries;
// the speech profiles declare no extension.
func TestServedHostDeclaresTheSeedExtension(t *testing.T) {
	dir := t.TempDir()
	s, err := server.New(server.Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"), Port: 41234,
		Models: []string{"user.LocalApprovedKlein"}, Transcription: true, Speech: true})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := servedHost("lemonade-image", s)
	extensions := host.SupportedExtensions[router.ProfileImage]
	if len(extensions) != len(server.ImageExtensions) || !extensions[server.SeedExtension] || len(host.SupportedExtensions) != 1 {
		t.Fatalf("declared extensions %v", host.SupportedExtensions)
	}
}
