//go:build !windows

package setup

import "context"

func retryPublicationRename(_ context.Context, rename func() error) error {
	return rename()
}
