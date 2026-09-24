package closure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const testdataPathInfo = "../../testdata/closure/path-info.json"

func loadTestData(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(testdataPathInfo)
	if err != nil {
		t.Fatalf("read %s: %v", testdataPathInfo, err)
	}
	return data
}

func TestParsePathInfo(t *testing.T) {
	c, err := Parse(loadTestData(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Paths) == 0 {
		t.Fatal("expected at least one path")
	}
	var sawSize, sawHash bool
	for i, p := range c.Paths {
		if p.Path == "" {
			t.Fatalf("path %d empty", i)
		}
		if i > 0 && c.Paths[i-1].Path >= p.Path {
			t.Errorf("paths not sorted at index %d", i)
		}
		if p.NarSize > 0 {
			sawSize = true
		}
		if p.NarHash != "" {
			sawHash = true
		}
	}
	if !sawSize {
		t.Error("expected a nonzero narSize")
	}
	if !sawHash {
		t.Error("expected a narHash")
	}
}

func TestParseProjectDoc(t *testing.T) {
	doc := `{
		"label": "churn-v1",
		"root": "/nix/store/b-root",
		"paths": [
			{"path": "/nix/store/b-root", "narSize": 2, "narHash": "sha256:bbb", "references": []},
			{"path": "/nix/store/a-dep", "narSize": 1, "narHash": "sha256:aaa", "references": ["/nix/store/b-root"]}
		]
	}`
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Label != "churn-v1" {
		t.Errorf("label = %q, want churn-v1", c.Label)
	}
	if c.Root != "/nix/store/b-root" {
		t.Errorf("root = %q, want /nix/store/b-root", c.Root)
	}
	if len(c.Paths) != 2 || c.Paths[0].Path != "/nix/store/a-dep" || c.Paths[1].Path != "/nix/store/b-root" {
		t.Errorf("paths not parsed/sorted: %+v", c.Paths)
	}
}

func TestParseLegacyDoc(t *testing.T) {
	doc := `{
		"id": "app",
		"imageName": "app:v1",
		"root": "/nix/store/r",
		"paths": [{"path": "/nix/store/r", "narSize": 10, "narHash": "sha256-QUJDRA==", "references": []}]
	}`
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Label != "app" {
		t.Errorf("label = %q, want app (from legacy id)", c.Label)
	}
	if c.Root != "/nix/store/r" {
		t.Errorf("root = %q, want /nix/store/r", c.Root)
	}
	if c.Paths[0].NarHash != "sha256-QUJDRA==" {
		t.Errorf("narHash = %q, want unchanged SRI form", c.Paths[0].NarHash)
	}
}

func TestParseRoundTrip(t *testing.T) {
	first, err := Parse(loadTestData(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	first.Label = "round-trip"
	project, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	second, err := Parse(project)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if len(first.Paths) != len(second.Paths) {
		t.Fatalf("path count changed: %d -> %d", len(first.Paths), len(second.Paths))
	}
	byPath := make(map[string]Path, len(second.Paths))
	for _, p := range second.Paths {
		byPath[p.Path] = p
	}
	for _, p := range first.Paths {
		q, ok := byPath[p.Path]
		if !ok {
			t.Fatalf("path %q lost in round trip", p.Path)
		}
		if q.NarSize != p.NarSize {
			t.Errorf("path %q narSize %d -> %d", p.Path, p.NarSize, q.NarSize)
		}
		if q.NarHash != p.NarHash {
			t.Errorf("path %q narHash %q -> %q", p.Path, p.NarHash, q.NarHash)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"malformed", `{"label": "x", "paths": [`},
		{"empty object", `{}`},
		{"empty paths", `{"label": "x", "paths": []}`},
		{"missing label", `{"paths": [{"path": "/nix/store/x", "narSize": 1}]}`},
		{"missing path field", `{"paths": [{"narSize": 1}]}`},
		{"unexpected key", `{"foo": 1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.doc)); err == nil {
				t.Errorf("Parse(%s) succeeded, want error", tc.doc)
			}
		})
	}
}

func TestFingerprintVector(t *testing.T) {
	c := Closure{Paths: []Path{
		{Path: "/nix/store/bbb", NarSize: 2},
		{Path: "/nix/store/aaa", NarSize: 1},
	}}
	const want = "706d6ef7218d2f5707afe07d321b426b"
	if got := c.Fingerprint(); got != want {
		t.Errorf("Fingerprint = %s, want %s", got, want)
	}
	reversed := Closure{Paths: []Path{
		{Path: "/nix/store/aaa", NarSize: 1},
		{Path: "/nix/store/bbb", NarSize: 2},
	}}
	if got := reversed.Fingerprint(); got != want {
		t.Errorf("Fingerprint order-dependent: %s, want %s", got, want)
	}
	bigger := Closure{Paths: []Path{
		{Path: "/nix/store/aaa", NarSize: 1},
		{Path: "/nix/store/bbb", NarSize: 3},
	}}
	if got := bigger.Fingerprint(); got == want {
		t.Error("Fingerprint did not change with a different size")
	}
}

func TestMissingAgainst(t *testing.T) {
	c := Closure{Paths: []Path{
		{Path: "/nix/store/a", NarSize: 10},
		{Path: "/nix/store/b", NarSize: 20},
		{Path: "/nix/store/c", NarSize: 30},
	}}
	have := map[string]struct{}{
		"/nix/store/a": {},
		"/nix/store/c": {},
	}
	m := MissingAgainst(c, have)
	if m.MissingBytes != 20 {
		t.Errorf("MissingBytes = %d, want 20", m.MissingBytes)
	}
	if m.HaveBytes != 40 {
		t.Errorf("HaveBytes = %d, want 40", m.HaveBytes)
	}
	if m.TotalBytes != 60 {
		t.Errorf("TotalBytes = %d, want 60", m.TotalBytes)
	}
	if m.HavePaths != 2 {
		t.Errorf("HavePaths = %d, want 2", m.HavePaths)
	}
	if m.TotalPaths != 3 {
		t.Errorf("TotalPaths = %d, want 3", m.TotalPaths)
	}
	if len(m.MissingPaths) != 1 || m.MissingPaths[0] != "/nix/store/b" {
		t.Errorf("MissingPaths = %v, want [/nix/store/b]", m.MissingPaths)
	}
}

func TestDiffBytes(t *testing.T) {
	a := Closure{Paths: []Path{
		{Path: "/nix/store/x", NarSize: 1},
		{Path: "/nix/store/y", NarSize: 2},
	}}
	b := Closure{Paths: []Path{
		{Path: "/nix/store/y", NarSize: 2},
		{Path: "/nix/store/z", NarSize: 5},
	}}
	if got := DiffBytes(a, b); got != 5 {
		t.Errorf("DiffBytes = %d, want 5", got)
	}
	if got := DiffBytes(b, a); got != 1 {
		t.Errorf("DiffBytes(b, a) = %d, want 1", got)
	}
}

func TestCatalog(t *testing.T) {
	dir := t.TempDir()
	docs := map[string]string{
		"churn-v1.json": `{"label": "churn-v1", "root": "/nix/store/r1", "paths": [
			{"path": "/nix/store/r1", "narSize": 100, "narHash": "sha256:one", "references": []}
		]}`,
		"share-a.json": `{"label": "share-a", "root": "/nix/store/r2", "paths": [
			{"path": "/nix/store/r2", "narSize": 200, "narHash": "sha256:two", "references": []},
			{"path": "/nix/store/d", "narSize": 50, "narHash": "sha256:d", "references": ["/nix/store/r2"]}
		]}`,
		"z-churn-v1-alias.json": `{"label": "churn-v1-alias", "root": "/nix/store/r1", "paths": [
			{"path": "/nix/store/r1", "narSize": 100, "narHash": "sha256:one", "references": []}
		]}`,
	}
	for name, doc := range docs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	nonJSON := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(nonJSON, []byte("skip me"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	cat, err := LoadCatalog(dir)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	want := Closure{Paths: []Path{{Path: "/nix/store/r1", NarSize: 100, NarHash: "sha256:one"}}}
	got, ok := cat.Lookup(want.Fingerprint())
	if !ok {
		t.Fatal("Lookup by fingerprint failed for churn-v1")
	}
	if got.Label != "churn-v1" {
		t.Errorf("Lookup label = %q, want churn-v1", got.Label)
	}
	alias, ok := cat.ByLabel("churn-v1-alias")
	if !ok {
		t.Fatal("ByLabel failed for closure with a shared fingerprint")
	}
	if alias.Label != "churn-v1-alias" {
		t.Errorf("alias label = %q, want churn-v1-alias", alias.Label)
	}
	if alias.Fingerprint() != want.Fingerprint() {
		t.Errorf("alias fingerprint = %s, want %s", alias.Fingerprint(), want.Fingerprint())
	}
	if len(cat.byFingerprint) != 2 {
		t.Errorf("stored fingerprints = %d, want 2 despite three labeled docs", len(cat.byFingerprint))
	}
	if _, ok := cat.Lookup("00000000000000000000000000000000"); ok {
		t.Error("Lookup with unknown fingerprint succeeded")
	}

	got, ok = cat.ByLabel("share-a")
	if !ok {
		t.Fatal("ByLabel failed for share-a")
	}
	if len(got.Paths) != 2 {
		t.Errorf("share-a paths = %d, want 2", len(got.Paths))
	}
	if _, ok := cat.ByLabel("nope"); ok {
		t.Error("ByLabel with unknown label succeeded")
	}
}

func TestCatalogLoadsTestdata(t *testing.T) {
	cat, err := LoadCatalog(filepath.Dir(testdataPathInfo))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	dump, err := Parse(loadTestData(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, ok := cat.Lookup(dump.Fingerprint()); !ok {
		t.Fatal("testdata dump not indexed by fingerprint")
	} else if len(got.Paths) != len(dump.Paths) {
		t.Errorf("catalog paths = %d, want %d", len(got.Paths), len(dump.Paths))
	}
}
