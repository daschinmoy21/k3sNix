package closure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalog(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, doc := range docs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCatalogLabelConflict(t *testing.T) {
	// One label naming two different closures is ambiguous; loading fails
	// and names the label and the file that introduced the conflict.
	dir := writeCatalog(t, map[string]string{
		"a.json": `{"label": "web", "paths": [
			{"path": "/nix/store/r1", "narSize": 100, "narHash": "sha256:one", "references": []}
		]}`,
		"b.json": `{"label": "web", "paths": [
			{"path": "/nix/store/r2", "narSize": 200, "narHash": "sha256:two", "references": []}
		]}`,
	})
	_, err := LoadCatalog(dir)
	if err == nil {
		t.Fatal("LoadCatalog accepted one label for two closures")
	}
	for _, want := range []string{`"web"`, "b.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestCatalogRepeatedLabelSameClosure(t *testing.T) {
	// The same closure under the same label in two files is a duplicate,
	// not a conflict.
	doc := `{"label": "web", "paths": [
		{"path": "/nix/store/r1", "narSize": 100, "narHash": "sha256:one", "references": []}
	]}`
	cat, err := LoadCatalog(writeCatalog(t, map[string]string{"a.json": doc, "b.json": doc}))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	c, ok := cat.ByLabel("web")
	if !ok {
		t.Fatal("ByLabel(web) failed")
	}
	if len(c.Paths) != 1 || c.Paths[0].Path != "/nix/store/r1" {
		t.Fatalf("ByLabel(web) = %+v", c)
	}
	if len(cat.byFingerprint) != 1 {
		t.Fatalf("stored fingerprints = %d, want 1", len(cat.byFingerprint))
	}
}
