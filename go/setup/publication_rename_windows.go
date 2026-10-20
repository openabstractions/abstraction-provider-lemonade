package setup

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// A reader can temporarily prevent the atomic publication rename.
func retryPublicationRename(ctx context.Context, rename func() error) error {
	waits := [...]time.Duration{25 * time.Millisecond, 75 * time.Millisecond, 150 * time.Millisecond, 250 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := rename()
		if attempt == len(waits) ||
			!errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waits[attempt]):
		}
	}
}
