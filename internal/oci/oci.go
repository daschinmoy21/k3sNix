// Package oci reads the layers of a docker-archive image (as written by
// dockerTools.buildLayeredImage) so the OCI baselines can be measured with
// the same missing-bytes computation as Nix closures: an image becomes a
// closure.Closure whose units are its layers, identified by diffID.
package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"k3snix/internal/closure"
)

// Layer is one image layer. Size is the uncompressed tar, the bytes a
// runtime unpacks; GzipSize is the same tar through Go's gzip at the default
// level, the compressor used for NARs in the comparison as well.
type Layer struct {
	DiffID   string   `json:"diffID"`
	Size     uint64   `json:"size"`
	GzipSize uint64   `json:"gzipSize"`
	Paths    []string `json:"paths"` // store paths the layer holds, sorted
}

// Image is the layer list of one image, in manifest order.
type Image struct {
	Label       string  `json:"label"`
	Granularity string  `json:"granularity"`
	RepoTag     string  `json:"repoTag"`
	Layers      []Layer `json:"layers"`
}

type archiveManifest struct {
	Config   string
	RepoTags []string
	Layers   []string
}

type imageConfig struct {
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// ReadArchive reads a docker-archive tarball, plain or gzipped. Every layer
// is hashed and must match the diffID its config declares.
func ReadArchive(file string) (Image, error) {
	var man []archiveManifest
	configs := map[string][]byte{}
	err := walk(file, func(name string, r io.Reader) error {
		switch {
		case name == "manifest.json":
			return json.NewDecoder(r).Decode(&man)
		case strings.HasSuffix(name, ".json"):
			b, err := io.ReadAll(r)
			configs[name] = b
			return err
		}
		return nil
	})
	if err != nil {
		return Image{}, fmt.Errorf("read %s: %w", file, err)
	}
	if len(man) != 1 {
		return Image{}, fmt.Errorf("read %s: want one image in manifest.json, got %d", file, len(man))
	}
	var cfg imageConfig
	if err := json.Unmarshal(configs[man[0].Config], &cfg); err != nil {
		return Image{}, fmt.Errorf("read %s: config %s: %w", file, man[0].Config, err)
	}
	if len(cfg.RootFS.DiffIDs) != len(man[0].Layers) {
		return Image{}, fmt.Errorf("read %s: %d layers but %d diff_ids", file, len(man[0].Layers), len(cfg.RootFS.DiffIDs))
	}
	index := map[string]int{}
	for i, l := range man[0].Layers {
		index[l] = i
	}

	img := Image{Layers: make([]Layer, len(man[0].Layers))}
	if len(man[0].RepoTags) > 0 {
		img.RepoTag = man[0].RepoTags[0]
	}
	seen := make([]bool, len(img.Layers))
	err = walk(file, func(name string, r io.Reader) error {
		i, ok := index[name]
		if !ok {
			return nil
		}
		l, err := readLayer(r)
		if err != nil {
			return fmt.Errorf("layer %s: %w", name, err)
		}
		if l.DiffID != cfg.RootFS.DiffIDs[i] {
			return fmt.Errorf("layer %s: sha256 %s, config diff_id %s", name, l.DiffID, cfg.RootFS.DiffIDs[i])
		}
		img.Layers[i] = l
		seen[i] = true
		return nil
	})
	if err != nil {
		return Image{}, fmt.Errorf("read %s: %w", file, err)
	}
	for i, ok := range seen {
		if !ok {
			return Image{}, fmt.Errorf("read %s: layer %s not in archive", file, man[0].Layers[i])
		}
	}
	return img, nil
}

// walk calls fn for each regular file in the archive.
func walk(file string, fn func(name string, r io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(path.Clean(h.Name), tr); err != nil {
			return err
		}
	}
}

// readLayer hashes, sizes and gzips one layer tar and lists the store paths
// in it.
func readLayer(r io.Reader) (Layer, error) {
	h := sha256.New()
	size := &countWriter{}
	gz := &countWriter{}
	zw := gzip.NewWriter(gz)
	pr, pw := io.Pipe()
	paths := map[string]struct{}{}
	done := make(chan error, 1)
	go func() {
		tr := tar.NewReader(pr)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				pr.CloseWithError(err)
				done <- err
				return
			}
			if p := storePath(hdr.Name); p != "" {
				paths[p] = struct{}{}
			}
		}
		_, _ = io.Copy(io.Discard, pr) // tar end padding
		done <- nil
	}()
	_, err := io.Copy(io.MultiWriter(h, size, zw, pw), r)
	pw.CloseWithError(err)
	if perr := <-done; err == nil {
		err = perr
	}
	if err != nil {
		return Layer{}, err
	}
	if err := zw.Close(); err != nil {
		return Layer{}, err
	}
	l := Layer{
		DiffID:   "sha256:" + hex.EncodeToString(h.Sum(nil)),
		Size:     size.n,
		GzipSize: gz.n,
		Paths:    make([]string, 0, len(paths)),
	}
	for p := range paths {
		l.Paths = append(l.Paths, p)
	}
	sort.Strings(l.Paths)
	return l, nil
}

// storePath maps a tar entry name inside a layer to the store path it
// belongs to: "/nix/store/<name>/bin/x" -> "/nix/store/<name>".
func storePath(name string) string {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	parts := strings.SplitN(name, "/", 4)
	if len(parts) < 3 || parts[0] != "nix" || parts[1] != "store" || parts[2] == "" {
		return ""
	}
	return "/nix/store/" + parts[2]
}

type countWriter struct{ n uint64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += uint64(len(p))
	return len(p), nil
}

// Units returns the image as a closure of layers: each layer is one unit
// named by its diffID and sized by its tar (or, with gzipped, its gzip
// size). closure.MissingAgainst then gives the layer bytes a node lacks.
func (img Image) Units(gzipped bool) closure.Closure {
	c := closure.Closure{Label: img.Label}
	for _, l := range img.Layers {
		size := l.Size
		if gzipped {
			size = l.GzipSize
		}
		c.Paths = append(c.Paths, closure.Path{Path: l.DiffID, NarSize: size})
	}
	return c
}
