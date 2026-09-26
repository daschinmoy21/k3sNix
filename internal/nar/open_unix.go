//go:build unix

package nar

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only without following a symlink that may have
// been swapped in after the caller's Lstat, matching how nix opens store paths.
func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
