//go:build !linux && !darwin

package agent

import (
	"fmt"
	"os"
)

// Native history fails closed where this build lacks the Linux openat
// O_NOFOLLOW primitive used to prove source containment and identity.
func nativeOpenRootFile(_, _ string) (*os.File, error) {
	return nil, fmt.Errorf("native history requires a no-follow rooted open")
}
