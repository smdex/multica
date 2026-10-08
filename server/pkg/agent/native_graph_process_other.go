//go:build !linux

package agent

import (
	"fmt"
	"os/exec"
)

// Native graph process execution requires Linux user+PID namespaces. This
// build refuses the opted-in operation with never-launched evidence so the
// provider executable is never executed on unsupported platforms.

func configureNativeGraphProcess(*exec.Cmd) error {
	return fmt.Errorf("native graph process execution requires Linux user+PID namespaces")
}

func nativeGraphProcessStartError(err error) error {
	return fmt.Errorf("%w: %w", ErrNativeGraphNeverLaunched, err)
}
