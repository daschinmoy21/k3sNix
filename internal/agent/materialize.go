package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k3snix/internal/closure"
	"k3snix/internal/nar"
)

// DirMaterializer unpacks NARs into a plain store directory, such as the
// agent's own store volume. Each path is restored under a dot-named temp
// name, which the inventory never lists, and renamed into place, so the
// path appears complete or not at all.
type DirMaterializer struct {
	Dir string
}

func (d DirMaterializer) Materialize(p closure.Path, f *os.File, _ map[string]closure.Path) error {
	final := filepath.Join(d.Dir, filepath.Base(p.Path))
	if _, err := os.Lstat(final); err == nil {
		return nil // installed meanwhile, by another request or nix
	}
	tmp := filepath.Join(d.Dir, ".k3snix-tmp-"+randomSuffix()+"-"+filepath.Base(p.Path))
	if err := nar.Restore(f, tmp); err != nil {
		removeTree(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		removeTree(tmp)
		if _, statErr := os.Lstat(final); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// NixMaterializer installs NARs into the host's real Nix store through the
// nix daemon: the verified NAR and a narinfo describing it are staged as a
// one-path file:// binary cache, and `nix copy` imports it. Nix registers
// the path and checks the NAR hash again; the daemon makes the path
// visible atomically. nix copy walks the closure through the staged
// narinfos, so p's references are staged as narinfo only; they are valid in
// the store already and their NARs are never read. The agent must be a
// trusted nix user, since the staged narinfo carries no signature.
type NixMaterializer struct {
	// StoreDir is the store the catalog paths live in, normally /nix/store.
	StoreDir string

	// To is the nix store URI to import into; empty means the default
	// store, which is the daemon's. Tests use a chroot store.
	To string
}

func (m NixMaterializer) Materialize(p closure.Path, f *os.File, cl map[string]closure.Path) error {
	staging, err := os.MkdirTemp("", "k3snix-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	hash := filepath.Base(p.Path)[:32]
	if err := os.Mkdir(filepath.Join(staging, "nar"), 0o755); err != nil {
		return err
	}
	dst, err := os.Create(filepath.Join(staging, "nar", hash+".nar"))
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, f); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	storeDir := m.StoreDir
	if storeDir == "" {
		storeDir = "/nix/store"
	}
	if err := os.WriteFile(filepath.Join(staging, "nix-cache-info"), []byte("StoreDir: "+storeDir+"\n"), 0o644); err != nil {
		return err
	}
	// Stage p and, transitively, everything it refers to.
	staged := map[string]bool{}
	queue := []closure.Path{p}
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		if staged[q.Path] {
			continue
		}
		staged[q.Path] = true
		if err := writeNarinfo(staging, q); err != nil {
			return err
		}
		for _, r := range q.References {
			if dep, ok := cl[r]; ok && !staged[r] {
				queue = append(queue, dep)
			}
		}
	}
	args := []string{"--extra-experimental-features", "nix-command",
		"copy", "--no-check-sigs", "--from", "file://" + staging}
	if m.To != "" {
		args = append(args, "--to", m.To)
	}
	cmd := exec.Command("nix", append(args, p.Path)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nix copy %s: %v: %s", p.Path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeNarinfo(dir string, p closure.Path) error {
	hash := filepath.Base(p.Path)[:32]
	refs := make([]string, 0, len(p.References))
	for _, r := range p.References {
		refs = append(refs, filepath.Base(r))
	}
	narHash := "sha256:" + p.NarHashNix32()
	info := fmt.Sprintf("StorePath: %s\nURL: nar/%s.nar\nCompression: none\nFileHash: %s\nFileSize: %d\nNarHash: %s\nNarSize: %d\nReferences: %s\n",
		p.Path, hash, narHash, p.NarSize, narHash, p.NarSize, strings.Join(refs, " "))
	return os.WriteFile(filepath.Join(dir, hash+".narinfo"), []byte(info), 0o644)
}

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// removeTree removes a partly restored path whose files are read-only.
func removeTree(dir string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
	os.RemoveAll(dir)
}
