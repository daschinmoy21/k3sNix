//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package nar

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The FIFO tests need syscall.Mkfifo, which windows, solaris and aix lack.

func TestWriteRejectsNonRegular(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		done <- Write(&buf, fifo)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Write accepted a FIFO")
		}
		if !errors.Is(err, os.ErrInvalid) {
			t.Errorf("Write(fifo) = %v, want os.ErrInvalid", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Write blocked on a FIFO")
	}
}

// openRegular must not block on a FIFO that replaced a file after the Lstat
// in writeNode: the open is non-blocking and the fd is rejected.
func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		f, _, err := openRegular(fifo)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("openRegular(fifo) = %v, want os.ErrInvalid", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("openRegular blocked on a FIFO")
	}
}
