//go:build !unix

package nar

import "os"

func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, &os.PathError{Op: "Write", Path: path, Err: os.ErrInvalid}
	}
	return f, fi, nil
}
