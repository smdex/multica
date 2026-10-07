//go:build !linux

package execenv

import (
	"context"
	"errors"
)

// InitializeNativeSource is not supported on this platform. It never launches
// any process.
func InitializeNativeSource(ctx context.Context, domain *NativeSourceDomain, executable string) error {
	return errors.New("execenv: InitializeNativeSource is unsupported on this platform")
}
