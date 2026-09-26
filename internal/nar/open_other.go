//go:build !unix

package nar

import "os"

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
