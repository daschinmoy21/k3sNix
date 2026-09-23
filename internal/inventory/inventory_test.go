package inventory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"k3snix/internal/closure"
)

func testClosure() closure.Closure {
	return closure.Closure{Paths: []closure.Path{
		{Path: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a", NarSize: 10},
		{Path: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b", NarSize: 20},
		{Path: "/nix/store/cccccccccccccccccccccccccccccccc-c", NarSize: 40},
	}}
}

func seed(t *testing.T, dir string, basenames []string) {
	t.Helper()
	for _, name := range basenames {
		// The on-disk size (5 bytes) differs from the closure NarSize on
		// purpose: Missing must use the closure sizes.
		if err := os.WriteFile(filepath.Join(dir, name), []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMissingWarmPartialCold(t *testing.T) {
	c := testClosure()
	basenames := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b",
		"cccccccccccccccccccccccccccccccc-c",
	}

	for _, tc := range []struct {
		name         string
		present      []string
		missingBytes uint64
		haveBytes    uint64
		havePaths    int
		missingPaths int
	}{
		{"warm", basenames, 0, 70, 3, 0},
		{"partial", basenames[:2], 40, 30, 2, 1},
		{"cold", nil, 70, 0, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, dir, tc.present)
			s := New(dir)
			s.TTL = 0
			m := s.Missing(c)
			if m.MissingBytes != tc.missingBytes || m.HaveBytes != tc.haveBytes ||
				m.TotalBytes != 70 || m.HavePaths != tc.havePaths ||
				m.TotalPaths != 3 || len(m.MissingPaths) != tc.missingPaths {
				t.Fatalf("%+v", m)
			}
		})
	}
}

func TestMissingBasenameAlias(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a"})
	s := New(dir)
	s.TTL = 0
	c := testClosure()
	m := s.Missing(c)
	if m.HaveBytes != 10 || m.MissingBytes != 60 {
		t.Fatalf("%+v", m)
	}
	if m.MissingPaths[0] != "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b" {
		t.Fatalf("missing paths %v", m.MissingPaths)
	}
}

func TestTTLHidesNewFile(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a"})
	s := New(dir)
	s.TTL = time.Hour
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}

	seed(t, dir, []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b"})
	m := s.Missing(testClosure())
	if m.HavePaths != 1 {
		t.Fatalf("new file must stay hidden until Refresh: %+v", m)
	}
	if got := s.LookupHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); got != "" {
		t.Fatalf("LookupHash saw hidden file %q", got)
	}

	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	m = s.Missing(testClosure())
	if m.HavePaths != 2 {
		t.Fatalf("Refresh must expose the new file: %+v", m)
	}
	if got := s.LookupHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); filepath.Base(got) != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b" {
		t.Fatalf("LookupHash got %q", got)
	}
}

func TestTTLZeroRefreshesEveryLookup(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.TTL = 0
	seed(t, dir, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a"})
	m := s.Missing(testClosure())
	if m.HavePaths != 1 {
		t.Fatalf("TTL 0 must see the file without an explicit Refresh: %+v", m)
	}
}

func TestLookupHash(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, []string{"cccccccccccccccccccccccccccccccc-pkg"})
	s := New(dir)
	s.TTL = 0
	if got := s.LookupHash("cccccccccccccccccccccccccccccccc"); filepath.Base(got) != "cccccccccccccccccccccccccccccccc-pkg" {
		t.Fatalf("got %q", got)
	}
	if got := s.LookupHash("dddddddddddddddddddddddddddddddd"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestSkipsDotFiles(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a"})
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	s.TTL = 0
	if got := s.Count(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

func TestDefaultDir(t *testing.T) {
	if got := New("").Dir; got != "/nix/store" {
		t.Fatalf("default dir = %q", got)
	}
	if got := New("").TTL; got != 2*time.Second {
		t.Fatalf("default ttl = %v", got)
	}
}
