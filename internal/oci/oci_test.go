package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layerTar builds a layer holding the given files.
func layerTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if strings.HasSuffix(name, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o444, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func diffID(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// archive writes a docker-archive with the given layers; diffIDs overrides
// the config's diff_ids when non-nil.
func archive(t *testing.T, gz bool, layers [][]byte, diffIDs []string) string {
	t.Helper()
	if diffIDs == nil {
		for _, l := range layers {
			diffIDs = append(diffIDs, diffID(l))
		}
	}
	var cfg imageConfig
	cfg.RootFS.DiffIDs = diffIDs
	cfgJSON, _ := json.Marshal(cfg)
	man := []archiveManifest{{Config: "config.json", RepoTags: []string{"k3snix-oci/x:fine"}}}
	files := map[string][]byte{"config.json": cfgJSON}
	for i, l := range layers {
		name := string(rune('a'+i)) + "/layer.tar"
		man[0].Layers = append(man[0].Layers, name)
		files[name] = l
	}
	manJSON, _ := json.Marshal(man)
	files["manifest.json"] = manJSON

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Layers first and manifest last, as stream_layered_image.py writes them.
	order := append(append([]string{}, man[0].Layers...), "config.json", "manifest.json")
	for _, name := range order {
		b := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o444, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if gz {
		var z bytes.Buffer
		zw := gzip.NewWriter(&z)
		zw.Write(out)
		zw.Close()
		out = z.Bytes()
	}
	f := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(f, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReadArchive(t *testing.T) {
	l1 := layerTar(t, map[string]string{
		"/nix/store/aaaa-glibc/":            "",
		"/nix/store/aaaa-glibc/lib/libc.so": "libc",
	})
	l2 := layerTar(t, map[string]string{
		"nix/store/bbbb-hello/bin/hello": "hello",
		"nix/store/cccc-extra/x":         "x",
	})
	l3 := layerTar(t, map[string]string{"./": ""})
	for _, gz := range []bool{false, true} {
		img, err := ReadArchive(archive(t, gz, [][]byte{l1, l2, l3}, nil))
		if err != nil {
			t.Fatalf("gz=%v: %v", gz, err)
		}
		if img.RepoTag != "k3snix-oci/x:fine" || len(img.Layers) != 3 {
			t.Fatalf("gz=%v: got %+v", gz, img)
		}
		if got := img.Layers[0]; got.DiffID != diffID(l1) || got.Size != uint64(len(l1)) ||
			strings.Join(got.Paths, ",") != "/nix/store/aaaa-glibc" {
			t.Errorf("layer 0: %+v", got)
		}
		if got := img.Layers[1].Paths; strings.Join(got, ",") != "/nix/store/bbbb-hello,/nix/store/cccc-extra" {
			t.Errorf("layer 1 paths: %v", got)
		}
		if got := img.Layers[2].Paths; len(got) != 0 {
			t.Errorf("layer 2 paths: %v", got)
		}
		for i, l := range img.Layers {
			if l.GzipSize == 0 {
				t.Errorf("layer %d: no gzip size", i)
			}
		}
		u := img.Units(false)
		if len(u.Paths) != 3 || u.Paths[1].Path != diffID(l2) || u.Paths[1].NarSize != uint64(len(l2)) {
			t.Errorf("units: %+v", u)
		}
		if g := img.Units(true); g.Paths[1].NarSize != img.Layers[1].GzipSize {
			t.Errorf("gzip units: %+v", g)
		}
	}
}

func TestReadArchiveRejectsDiffIDMismatch(t *testing.T) {
	l := layerTar(t, map[string]string{"nix/store/aaaa-x/f": "f"})
	_, err := ReadArchive(archive(t, false, [][]byte{l}, []string{"sha256:" + strings.Repeat("0", 64)}))
	if err == nil || !strings.Contains(err.Error(), "diff_id") {
		t.Fatalf("want diff_id mismatch, got %v", err)
	}
}

func TestReadArchiveRejectsDiffIDCount(t *testing.T) {
	l := layerTar(t, map[string]string{"nix/store/aaaa-x/f": "f"})
	_, err := ReadArchive(archive(t, false, [][]byte{l}, []string{diffID(l), diffID(l)}))
	if err == nil {
		t.Fatal("want an error for 1 layer with 2 diff_ids")
	}
}

func TestStorePath(t *testing.T) {
	for in, want := range map[string]string{
		"/nix/store/abc-x/bin/y": "/nix/store/abc-x",
		"nix/store/abc-x":        "/nix/store/abc-x",
		"./nix/store/abc-x/":     "/nix/store/abc-x",
		"nix/store/":             "",
		"nix/":                   "",
		"etc/passwd":             "",
		"./":                     "",
	} {
		if got := storePath(in); got != want {
			t.Errorf("storePath(%q) = %q, want %q", in, got, want)
		}
	}
}
