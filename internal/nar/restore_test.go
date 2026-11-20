package nar

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTree writes a store-path-like tree: nested dirs, a plain and an
// executable file, an empty file, a relative and an absolute symlink.
func buildTree(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{"bin", "lib/sub"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "bin/app"), "#!/bin/sh\necho hi\n", 0o755)
	writeFile(t, filepath.Join(root, "lib/sub/data"), strings.Repeat("x", 13), 0o644)
	writeFile(t, filepath.Join(root, "empty"), "", 0o644)
	if err := os.Symlink("../lib/sub/data", filepath.Join(root, "bin/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-dep/lib", filepath.Join(root, "dep")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	buildTree(t, src)
	want := dump(t, src)

	dst := filepath.Join(t.TempDir(), "dst")
	if err := Restore(bytes.NewReader(want), dst); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, dst); !bytes.Equal(got, want) {
		t.Fatal("NAR of the restored tree differs from the original NAR")
	}
	fi, err := os.Stat(filepath.Join(dst, "bin/app"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o555 {
		t.Errorf("executable restored as %v, want 0555", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(dst, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Errorf("file restored as %v, want 0444", fi.Mode().Perm())
	}
}

func TestRestoreSingleFileAndSymlink(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	writeFile(t, file, "hello", 0o644)
	link := filepath.Join(dir, "link")
	if err := os.Symlink("file", link); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{file, link} {
		want := dump(t, src)
		dst := filepath.Join(t.TempDir(), "out")
		if err := Restore(bytes.NewReader(want), dst); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := dump(t, dst); !bytes.Equal(got, want) {
			t.Fatalf("%s: round trip differs", src)
		}
	}
}

// str encodes one NAR string.
func str(s string) []byte {
	var b []byte
	b = binary.LittleEndian.AppendUint64(b, uint64(len(s)))
	b = append(b, s...)
	return append(b, make([]byte, (8-len(s)%8)%8)...)
}

func narOf(tokens ...string) []byte {
	var b []byte
	for _, tok := range tokens {
		b = append(b, str(tok)...)
	}
	return b
}

func dirWith(names ...string) []byte {
	toks := []string{"nix-archive-1", "(", "type", "directory"}
	for _, n := range names {
		toks = append(toks, "entry", "(", "name", n, "node", "(", "type", "symlink", "target", "x", ")", ")")
	}
	return narOf(append(toks, ")")...)
}

func TestRestoreRejectsBadArchives(t *testing.T) {
	good := dump(t, func() string {
		p := filepath.Join(t.TempDir(), "f")
		writeFile(t, p, "abc", 0o644)
		return p
	}())
	for name, data := range map[string][]byte{
		"dotdot":       dirWith(".."),
		"slash":        dirWith("a/b"),
		"empty name":   dirWith(""),
		"out of order": dirWith("b", "a"),
		"duplicate":    dirWith("a", "a"),
		"bad magic":    narOf("nix-archive-2", "(", "type", "symlink", "target", "x", ")"),
		"bad type":     narOf("nix-archive-1", "(", "type", "fifo", ")"),
		"truncated":    good[:len(good)-9],
		"trailing":     append(append([]byte{}, good...), str("x")...),
	} {
		dst := filepath.Join(t.TempDir(), "out")
		if err := Restore(bytes.NewReader(data), dst); err == nil {
			t.Errorf("%s: Restore accepted the archive", name)
		}
	}
}

func TestRestoreRefusesExistingDestination(t *testing.T) {
	dst := t.TempDir()
	if err := Restore(bytes.NewReader(dirWith("a")), dst); err == nil {
		t.Fatal("Restore wrote into an existing directory")
	}
}
