//go:build linux

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// configureNativeGraphProcess launches the provider as the init of a fresh
// user+PID namespace, preserving the caller's UID/GID and process-group setup.
// When the namespace init is reaped, the kernel has killed and reaped remaining
// processes in that namespace, including setsid descendants. This is scoped
// process-stop evidence, not filesystem/network isolation or proof about work
// delegated to an external service. A synchronous Start error proves only that
// the provider executable did not execute, not that no internal fork occurred.
func configureNativeGraphProcess(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		// Unreachable through newRuntimeCmd, which always sets it.
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	uid, gid := os.Getuid(), os.Getgid()
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}

// nativeGraphProcessStartError classifies a synchronous start failure of an
// opted-in native execution. ErrNativeGraphNeverLaunched is only for failures
// at the launch boundary; arbitrary later errors keep their own identity.
func nativeGraphProcessStartError(err error) error {
	return fmt.Errorf("%w: %w", ErrNativeGraphNeverLaunched, err)
}
