package nar

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello")
	writeFile(t, path, "hi", 0o644)

	raw := dump(t, path)
	if !bytes.Contains(raw, []byte("nix-archive-1")) {
		t.Fatal("missing magic string")
	}

	n := decode(t, raw)
	if n.typ != "regular" {
		t.Fatalf("type = %q, want regular", n.typ)
	}
	if string(n.contents) != "hi" {
		t.Fatalf("contents = %q, want %q", n.contents, "hi")
	}
}

func TestWriteDirectoryEntriesSorted(t *testing.T) {
	dir := t.TempDir()
	names := []string{"z", "B", "0", "a", "m"}
	for _, name := range names {
		writeFile(t, filepath.Join(dir, name), name, 0o644)
	}

	n := decode(t, dump(t, dir))
	if n.typ != "directory" {
		t.Fatalf("type = %q, want directory", n.typ)
	}

	var got []string
	for _, entry := range n.entries {
		got = append(got, entry.name)
		if string(entry.child.contents) != entry.name {
			t.Errorf("entry %q carries contents %q", entry.name, entry.child.contents)
		}
	}

	want := []string{"0", "B", "a", "m", "z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestWriteSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	writeFile(t, target, "bytes-inside-the-target", 0o644)

	link := filepath.Join(dir, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}

	raw := dump(t, link)
	n := decode(t, raw)
	if n.typ != "symlink" {
		t.Fatalf("type = %q, want symlink", n.typ)
	}
	if n.target != "target" {
		t.Fatalf("target = %q, want %q", n.target, "target")
	}
	if bytes.Contains(raw, []byte("bytes-inside-the-target")) {
		t.Fatal("symlink dump inlined the target's contents")
	}
}

func TestWriteSymlinkToDirectoryIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inner, "child"), "bytes-inside-the-target", 0o644)

	link := filepath.Join(dir, "dirlink")
	if err := os.Symlink("inner", link); err != nil {
		t.Fatal(err)
	}

	raw := dump(t, link)
	n := decode(t, raw)
	if n.typ != "symlink" {
		t.Fatalf("type = %q, want symlink", n.typ)
	}
	if n.target != "inner" {
		t.Fatalf("target = %q, want %q", n.target, "inner")
	}
	if bytes.Contains(raw, []byte("bytes-inside-the-target")) {
		t.Fatal("symlink to a directory was dumped as a directory")
	}
}

func TestWriteExecutable(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	writeFile(t, script, "#!/bin/sh\nexit 0\n", 0o755)
	plain := filepath.Join(dir, "data")
	writeFile(t, plain, "data", 0o644)

	if n := decode(t, dump(t, script)); !n.exec {
		t.Error("executable field missing for a 0755 file")
	}
	if n := decode(t, dump(t, plain)); n.exec {
		t.Error("executable field present for a 0644 file")
	}
}

func TestWriteMatchesNixStoreDump(t *testing.T) {
	nixStore, err := exec.LookPath("nix-store")
	if err != nil {
		t.Skip("nix-store not on PATH")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plain"), "hello nar\n", 0o644)
	writeFile(t, filepath.Join(dir, "empty"), "", 0o644)
	writeFile(t, filepath.Join(dir, "eight"), "12345678", 0o644)
	writeFile(t, filepath.Join(dir, "thirteen"), "1234567890123", 0o644)
	writeFile(t, filepath.Join(dir, "exec"), "#!/bin/sh\necho hi\n", 0o755)
	writeFile(t, filepath.Join(dir, "groupexec"), "x", 0o711)
	if err := os.Symlink("plain", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tree, "B"), "b\n", 0o644)
	writeFile(t, filepath.Join(tree, "a"), "a\n", 0o644)
	writeFile(t, filepath.Join(tree, "exec.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(tree, "sub", "deep"), strings.Repeat("x", 13), 0o644)
	if err := os.Symlink("../plain", filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}

	emptyDir := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		filepath.Join(dir, "plain"),
		filepath.Join(dir, "empty"),
		filepath.Join(dir, "eight"),
		filepath.Join(dir, "thirteen"),
		filepath.Join(dir, "exec"),
		filepath.Join(dir, "groupexec"),
		filepath.Join(dir, "link"),
		tree,
		emptyDir,
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			got := dump(t, path)
			want, err := exec.Command(nixStore, "--dump", path).Output()
			if err != nil {
				t.Fatalf("nix-store --dump %s: %v", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("NAR differs from nix-store --dump (%s)", firstDiff(got, want))
			}
		})
	}
}

func firstDiff(got, want []byte) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("byte %d: got %#02x, want %#02x", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("length: got %d, want %d", len(got), len(want))
}

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func dump(t *testing.T, path string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, path); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	return buf.Bytes()
}

type narNode struct {
	typ      string
	exec     bool
	contents []byte
	target   string
	entries  []narEntry
}

type narEntry struct {
	name  string
	child *narNode
}

type narReader struct {
	b   []byte
	off int
}

func decode(t *testing.T, raw []byte) *narNode {
	t.Helper()
	r := &narReader{b: raw}
	if got := r.str(t); got != magic {
		t.Fatalf("magic = %q, want %q", got, magic)
	}
	n := r.node(t)
	if r.off != len(raw) {
		t.Fatalf("%d unread trailing bytes", len(raw)-r.off)
	}
	return n
}

func (r *narReader) str(t *testing.T) string {
	t.Helper()
	return string(r.blob(t))
}

func (r *narReader) blob(t *testing.T) []byte {
	t.Helper()
	if r.off+8 > len(r.b) {
		t.Fatalf("truncated length field at offset %d", r.off)
	}
	size := int(binary.LittleEndian.Uint64(r.b[r.off:]))
	r.off += 8
	if size < 0 || r.off+size > len(r.b) {
		t.Fatalf("truncated string of %d bytes at offset %d", size, r.off)
	}
	b := r.b[r.off : r.off+size]
	r.off += size + (8-size%8)%8
	return b
}

func (r *narReader) want(t *testing.T, s string) {
	t.Helper()
	if got := r.str(t); got != s {
		t.Fatalf("field = %q, want %q", got, s)
	}
}

func (r *narReader) try(t *testing.T, s string) bool {
	t.Helper()
	save := r.off
	if r.str(t) == s {
		return true
	}
	r.off = save
	return false
}

func (r *narReader) node(t *testing.T) *narNode {
	t.Helper()
	r.want(t, "(")
	r.want(t, "type")

	n := &narNode{typ: r.str(t)}
	switch n.typ {
	case "regular":
		if r.try(t, "executable") {
			r.want(t, "")
			n.exec = true
		}
		r.want(t, "contents")
		n.contents = r.blob(t)
	case "symlink":
		r.want(t, "target")
		n.target = r.str(t)
	case "directory":
		for r.try(t, "entry") {
			r.want(t, "(")
			r.want(t, "name")
			name := r.str(t)
			r.want(t, "node")
			child := r.node(t)
			r.want(t, ")")
			n.entries = append(n.entries, narEntry{name: name, child: child})
		}
	default:
		t.Fatalf("unknown node type %q", n.typ)
	}

	r.want(t, ")")
	return n
}
