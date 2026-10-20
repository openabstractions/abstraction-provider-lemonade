package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStageSpeechModelRenameRetriesWindowsSharing(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		releaseOn int
	}{
		{name: "released", releaseOn: 2},
		{name: "persistent"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			stage, target := filepath.Join(root, "stage"), filepath.Join(root, "model")
			if err := os.Mkdir(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, "weight"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			release := holdPublicationReader(t, stage, windows.FILE_FLAG_BACKUP_SEMANTICS)

			calls := 0
			var denied error
			err := retryPublicationRename(t.Context(), func() error {
				calls++
				err := os.Rename(stage, target)
				if scenario.releaseOn == 0 || calls <= scenario.releaseOn {
					if !sharingDenied(err) {
						t.Fatalf("rename with an open reader: %v", err)
					}
					denied = err
				}
				if calls == scenario.releaseOn {
					release()
				}
				return err
			})
			present, absent := stage, target
			if scenario.releaseOn != 0 {
				if err != nil || calls <= scenario.releaseOn {
					t.Fatalf("rename after reader released: %d tries, %v", calls, err)
				}
				present, absent = target, stage
			} else if err != denied || calls != 5 {
				t.Fatalf("persistent refusal: %d tries, %v, want %v", calls, err, denied)
			}
			if data, err := os.ReadFile(filepath.Join(present, "weight")); err != nil || string(data) != "fixture" {
				t.Fatalf("retained model: %q, %v", data, err)
			}
			if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected alternate model folder: %v", err)
			}
		})
	}
}

func TestRegistryAtomicRenameRetriesWindowsSharing(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		releaseOn int
	}{
		{name: "released", releaseOn: 2},
		{name: "persistent"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			registry := filepath.Join(root, "user_models.json")
			old, next := []byte(`{"old":{}}`), []byte(`{"next":{}}`)
			if err := os.WriteFile(registry, old, 0o600); err != nil {
				t.Fatal(err)
			}
			release := holdPublicationReader(t, registry, windows.FILE_ATTRIBUTE_NORMAL)

			calls := 0
			var denied error
			err := writeAtomicWithRetry(registry, next, func(publish func() error) error {
				return retryPublicationRename(t.Context(), func() error {
					calls++
					err := publish()
					if scenario.releaseOn == 0 || calls <= scenario.releaseOn {
						if !sharingDenied(err) {
							t.Fatalf("registry rename with an open reader: %v", err)
						}
						denied = err
					}
					if calls == scenario.releaseOn {
						release()
					}
					return err
				})
			})
			want := old
			if scenario.releaseOn != 0 {
				if err != nil || calls <= scenario.releaseOn {
					t.Fatalf("registry rename after reader released: %d tries, %v", calls, err)
				}
				want = next
			} else if err != denied || calls != 5 {
				t.Fatalf("persistent registry refusal: %d tries, %v, want %v", calls, err, denied)
			}
			if got, err := os.ReadFile(registry); err != nil || string(got) != string(want) {
				t.Fatalf("registry readback: %q, %v, want %q", got, err, want)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != "user_models.json" {
				t.Fatalf("registry temporary file remained: %v, %v", entries, err)
			}
		})
	}
}

func holdPublicationReader(t *testing.T, path string, flags uint32) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	release := func() {
		if handle == windows.InvalidHandle {
			return
		}
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
		handle = windows.InvalidHandle
	}
	t.Cleanup(release)
	return release
}

func sharingDenied(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
