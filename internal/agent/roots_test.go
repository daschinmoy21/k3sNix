package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k3snix/internal/closure"
	"k3snix/internal/nar"
	"k3snix/internal/nixbase32"
)

const (
	uidA = "0f3e2c1a-aaaa-4bbb-8ccc-000000000001"
	uidB = "0f3e2c1a-aaaa-4bbb-8ccc-000000000002"
)

// addTool adds a second catalog closure, tool, that shares libc with app.
func (f fetchFixture) addTool(t *testing.T) string {
	t.Helper()
	const toolName = "dddddddddddddddddddddddddddddddd-tool"
	mustDir(t, filepath.Join(f.origin, toolName))
	mustFile(t, filepath.Join(f.origin, toolName, "tool"), []byte("tool"))
	tool := pathEntry(t, f.origin, toolName, toolName, fLibcName)
	doc, err := json.Marshal(map[string]any{
		"label": "tool", "root": tool.Path,
		"paths": []any{tool, f.paths[fLibcName]},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustFile(t, filepath.Join(f.catDir, "tool.json"), doc)
	return toolName
}

func postGC(t *testing.T, url string) (int, GCResp, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/gc", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out GCResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out, string(raw)
}

func TestPodRootsKeepSharedPaths(t *testing.T) {
	f := newFetchFixture(t)
	toolName := f.addTool(t)
	_, origin := f.serve(t, f.origin, nil)

	dir := t.TempDir()
	roots := t.TempDir()
	pods := t.TempDir()
	n, srv := f.serve(t, dir, nil)
	n.Origin = origin.URL
	n.RootsDir = roots
	n.PodsDir = pods
	for _, uid := range []string{uidA, uidB} {
		mustDir(t, filepath.Join(pods, uid))
	}

	if code, _, raw := postFetch(t, srv.URL, `{"label":"app","pod_uid":"`+uidA+`"}`); code != 200 {
		t.Fatalf("fetch app: %d %s", code, raw)
	}
	if code, _, raw := postFetch(t, srv.URL, `{"label":"tool","pod_uid":"`+uidB+`"}`); code != 200 {
		t.Fatalf("fetch tool: %d %s", code, raw)
	}
	target, err := os.Readlink(filepath.Join(roots, uidA))
	if err != nil || target != filepath.Join(dir, fAppName) {
		t.Fatalf("root for pod A -> %q (%v), want the app path", target, err)
	}

	// Both pods run: nothing is collectable.
	code, gc, raw := postGC(t, srv.URL)
	if code != 200 || len(gc.Removed) != 0 || gc.Roots != 2 {
		t.Fatalf("gc with both pods: %d %+v %s", code, gc, raw)
	}

	// Pod A goes away. Its root goes with it; tool's libc stays.
	os.Remove(filepath.Join(pods, uidA))
	if removed, err := n.Reconcile(); err != nil || removed != 1 {
		t.Fatalf("Reconcile = %d, %v; want 1 root removed", removed, err)
	}
	code, gc, raw = postGC(t, srv.URL)
	if code != 200 {
		t.Fatalf("gc: %d %s", code, raw)
	}
	want := []string{"/nix/store/" + fAppName, "/nix/store/" + fLibName}
	if strings.Join(gc.Removed, ",") != strings.Join(want, ",") {
		t.Fatalf("gc removed %v, want %v", gc.Removed, want)
	}
	for _, name := range []string{toolName, fLibcName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s removed although pod B needs it: %v", name, err)
		}
	}
	for _, name := range []string{fAppName, fLibName} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s survived gc without a root", name)
		}
	}
}

func TestReconcileNeedsPodsDir(t *testing.T) {
	n := newServer(t, t.TempDir())
	n.RootsDir = t.TempDir()
	n.PodsDir = filepath.Join(t.TempDir(), "missing")
	if err := os.Symlink("/nix/store/x", filepath.Join(n.RootsDir, uidA)); err != nil {
		t.Fatal(err)
	}
	// Without the kubelet's directory every pod would look deleted, so
	// nothing is removed.
	if removed, err := n.Reconcile(); err == nil || removed != 0 {
		t.Fatalf("Reconcile = %d, %v; want an error and no removal", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(n.RootsDir, uidA)); err != nil {
		t.Fatal("root removed")
	}
}

func TestFetchRejectsBadPodUID(t *testing.T) {
	f := newFetchFixture(t)
	_, origin := f.serve(t, f.origin, nil)
	n, srv := f.serve(t, t.TempDir(), nil)
	n.Origin = origin.URL
	n.RootsDir = t.TempDir()
	code, _, raw := postFetch(t, srv.URL, `{"label":"app","pod_uid":"../../etc"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", code, raw)
	}
}

// pathEntry describes store/name with its real NAR hash and size.
func pathEntry(t *testing.T, store, name string, refs ...string) closure.Path {
	t.Helper()
	var buf bytes.Buffer
	if err := nar.Write(&buf, filepath.Join(store, name)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	p := closure.Path{
		Path:    "/nix/store/" + name,
		NarSize: uint64(buf.Len()),
		NarHash: "sha256:" + nixbase32.EncodeToString(sum[:]),
	}
	for _, r := range refs {
		p.References = append(p.References, "/nix/store/"+r)
	}
	return p
}
