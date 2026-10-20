package setup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// backendRuntime is the private release-input projection shared with the
// ModelHost derivation. The caller stages this fixed-name asset beside its
// backend archive; no installed SDK or machine runtime is searched.
type backendRuntime struct {
	Name, SHA256, Prefix string
	Size                 int64
	Files                []server.ManifestFile
}

func (r backendRuntime) read(ctx context.Context, backendArchive string) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(backendArchive), r.Name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("pinned app-local runtime input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != r.Size {
		return nil, errors.New("pinned app-local runtime input has a different kind or size")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, r.Size+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != r.Size || hex.EncodeToString(sum[:]) != r.SHA256 {
		return nil, errors.New("app-local runtime ZIP differs from its release input pin")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	if len(archive.File) != len(r.Files) {
		return nil, errors.New("app-local runtime ZIP member roster differs")
	}
	result := make(map[string][]byte, len(r.Files))
	for index, want := range r.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := archive.File[index]
		if entry.Name != r.Prefix+want.Name || !entry.Mode().IsRegular() || entry.UncompressedSize64 != uint64(want.Size) {
			return nil, errors.New("app-local runtime ZIP member differs")
		}
		stream, err := entry.Open()
		if err != nil {
			return nil, err
		}
		member, readErr := io.ReadAll(io.LimitReader(stream, want.Size+1))
		if err := errors.Join(readErr, stream.Close()); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(member)
		if int64(len(member)) != want.Size || hex.EncodeToString(digest[:]) != want.SHA256 {
			return nil, fmt.Errorf("app-local runtime member %s differs from its pin", want.Name)
		}
		result[want.Name] = member
	}
	return result, nil
}
