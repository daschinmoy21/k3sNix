//go:build unix

package nar

import (
	"os"
	"syscall"
)

// openRegular opens path read-only for dumping and returns the fd's own
// stat. O_NOFOLLOW refuses a symlink swapped in after the caller's Lstat,
// matching how nix opens store paths, and O_NONBLOCK keeps a FIFO swapped in
// the same way from blocking the open until a writer appears. Anything but a
// regular file is rejected before the fd is switched back to blocking mode.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		syscall.Close(fd)
		return nil, nil, &os.PathError{Op: "Write", Path: path, Err: os.ErrInvalid}
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}
