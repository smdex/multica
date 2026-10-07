//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// nativeOpenRootFile walks each component with openat(2) and O_NOFOLLOW. The
// directory descriptors, rather than a re-resolved path string, are the
// authority for every step. This rejects both leaf and intermediate symlinks.
func nativeOpenRootFile(root, relative string) (*os.File, error) {
	if relative == "." || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("invalid relative native path")
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty relative native path")
	}
	fd, err := nativeOpenDirectoryNoFollow(root)
	if err != nil {
		return nil, err
	}
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("invalid relative native path")
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			// Opening a FIFO read-only can block before f.Stat verifies that the
			// selected object is regular. O_NONBLOCK lets that verification fail
			// promptly and is inert for regular transcript files.
			flags |= unix.O_NONBLOCK
		}
		next, openErr := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Join(root, relative)), nil
}

func nativeOpenDirectoryNoFollow(path string) (int, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return -1, fmt.Errorf("native root must be absolute")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	if path == string(filepath.Separator) {
		return fd, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("invalid native root")
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, openErr
		}
		fd = next
	}
	return fd, nil
}
