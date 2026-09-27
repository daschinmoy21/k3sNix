//go:build unix

package nar

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "target"), "x", 0o644)
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openRegular(link); err == nil {
		f.Close()
		t.Fatal("openRegular followed a symlink")
	}
}

func TestOpenRegularStatsTheFd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	writeFile(t, p, "hello", 0o755)
	f, fi, err := openRegular(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi.Size() != 5 || fi.Mode()&0o100 == 0 {
		t.Fatalf("stat size %d mode %v, want 5 and owner-executable", fi.Size(), fi.Mode())
	}
	// The fd is back in blocking mode and reads normally.
	buf := make([]byte, 5)
	if _, err := f.Read(buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read %q, %v", buf, err)
	}
}
