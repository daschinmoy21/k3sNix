// Package compare measures Nix store-path delivery against OCI layer
// delivery on the same workloads. Both sides go through one function,
// closure.MissingAgainst: a delivery design only decides what the units are
// (store paths with their NAR sizes, or image layers with their tar sizes)
// and a node's have-set is the union of the units of what it holds.
package compare

import (
	"fmt"
	"sort"
	"strings"

	"k3snix/internal/closure"
	"k3snix/internal/oci"
)

// Delivery designs.
const (
	Nix       = "nix"        // store paths, NAR bytes
	OCIFine   = "oci-fine"   // one layer per store path
	OCICoarse = "oci-coarse" // one layer for the whole closure
)

// Deliveries lists the delivery designs in report order.
var Deliveries = []string{Nix, OCIFine, OCICoarse}

var granularity = map[string]string{OCIFine: "fine", OCICoarse: "coarse"}

// Inputs are the workloads in each delivery design.
type Inputs struct {
	Manifests map[string]closure.Closure      // label -> closure manifest
	NarGzip   map[string]uint64               // store path -> gzipped NAR size
	Images    map[string]map[string]oci.Image // "fine"/"coarse" -> label -> image
	// RuntimeImageSizes supplies CRI image sizes for ImageLocality.
	// Without it Run omits image-scoring arms; archive sizes cannot replace it.
	// When supplied it must cover every image; runtimes define sizes differently.
	RuntimeImageSizes map[string]map[string]uint64
	// ImageLocalityNodes includes filtered nodes, e.g. a tainted control plane.
	// Zero uses the three model workers.
	ImageLocalityNodes int
}

// Labels returns the workload labels, sorted.
func (in Inputs) Labels() []string {
	ls := make([]string, 0, len(in.Manifests))
	for l := range in.Manifests {
		ls = append(ls, l)
	}
	sort.Strings(ls)
	return ls
}

// Units returns label in delivery d as a closure of its content units:
// store paths, or the image layers that hold store paths. With gzipped,
// each unit is sized by its gzip stream instead of its raw bytes.
func (in Inputs) Units(d, label string, gzipped bool) (closure.Closure, error) {
	return in.units(d, label, gzipped, false)
}

// WholeImage is Units with every layer of an image, including the path-less
// one: what a runtime stores and pulls, and what a layer scorer sees. For
// nix it equals Units.
func (in Inputs) WholeImage(d, label string, gzipped bool) (closure.Closure, error) {
	return in.units(d, label, gzipped, true)
}

func (in Inputs) units(d, label string, gzipped, whole bool) (closure.Closure, error) {
	if d == Nix {
		m, ok := in.Manifests[label]
		if !ok {
			return closure.Closure{}, fmt.Errorf("no manifest for %s", label)
		}
		c := closure.Closure{Label: label, Root: m.Root, Paths: make([]closure.Path, len(m.Paths))}
		copy(c.Paths, m.Paths)
		if gzipped {
			for i := range c.Paths {
				n, ok := in.NarGzip[c.Paths[i].Path]
				if !ok {
					return closure.Closure{}, fmt.Errorf("no gzipped NAR size for %s", c.Paths[i].Path)
				}
				c.Paths[i].NarSize = n
			}
		}
		return c, nil
	}
	g, ok := granularity[d]
	if !ok {
		return closure.Closure{}, fmt.Errorf("unknown delivery %q", d)
	}
	img, ok := in.Images[g][label]
	if !ok {
		return closure.Closure{}, fmt.Errorf("no %s image for %s", g, label)
	}
	c := img.Units(gzipped)
	if whole {
		return c, nil
	}
	// Drop the path-less layer (buildLayeredImage's customisation layer, an
	// empty directory here). Check proves it is the same in every image, so
	// it carries no workload content and would only make unrelated images
	// look shared; Run reports it separately as per-image overhead, and
	// placement uses WholeImage, since schedulers and runtimes see it.
	kept := c.Paths[:0]
	for i, l := range img.Layers {
		if len(l.Paths) > 0 {
			kept = append(kept, c.Paths[i])
		}
	}
	c.Paths = kept
	return c, nil
}

// Overhead returns the path-less layer every image carries, if any.
func (in Inputs) Overhead() (oci.Layer, bool) {
	for _, byLabel := range in.Images {
		for _, img := range byLabel {
			for _, l := range img.Layers {
				if len(l.Paths) == 0 {
					return l, true
				}
			}
		}
	}
	return oci.Layer{}, false
}

// Have is the have-set of a node holding labels in delivery d: every unit
// of every label it holds. Unit IDs are content addresses (store paths,
// layer diffIDs), so a unit shared by two workloads is held once.
func (in Inputs) Have(d string, held []string) (map[string]struct{}, error) {
	return in.have(d, held, false)
}

func (in Inputs) have(d string, held []string, whole bool) (map[string]struct{}, error) {
	have := map[string]struct{}{}
	for _, l := range held {
		c, err := in.units(d, l, false, whole)
		if err != nil {
			return nil, err
		}
		for _, p := range c.Paths {
			have[p.Path] = struct{}{}
		}
	}
	return have, nil
}

// Missing is what a node holding held lacks of target's content units in
// delivery d.
func (in Inputs) Missing(d string, held []string, target string, gzipped bool) (closure.Missing, error) {
	return in.missing(d, held, target, gzipped, false)
}

// MissingWhole is Missing over whole images (see WholeImage).
func (in Inputs) MissingWhole(d string, held []string, target string, gzipped bool) (closure.Missing, error) {
	return in.missing(d, held, target, gzipped, true)
}

func (in Inputs) missing(d string, held []string, target string, gzipped, whole bool) (closure.Missing, error) {
	have, err := in.have(d, held, whole)
	if err != nil {
		return closure.Missing{}, err
	}
	c, err := in.units(d, target, gzipped, whole)
	if err != nil {
		return closure.Missing{}, err
	}
	return closure.MissingAgainst(c, have), nil
}

// Check verifies that the comparison is like for like: every image of a
// label carries exactly the store paths of that label's manifest, each in
// one layer, plus at most one layer that holds no store path and is the
// same in every image (buildLayeredImage's customisation layer). Fine
// images must hold one store path per layer and coarse images one layer of
// store paths.
func (in Inputs) Check() error {
	if in.ImageLocalityNodes != 0 && in.ImageLocalityNodes < len(Nodes) {
		return fmt.Errorf("ImageLocality node count must include all %d workers", len(Nodes))
	}
	if in.RuntimeImageSizes != nil {
		for _, g := range []string{"fine", "coarse"} {
			for _, label := range in.Labels() {
				if in.RuntimeImageSizes[g][label] == 0 {
					return fmt.Errorf("no runtime image size for %s/%s", g, label)
				}
			}
		}
	}
	var extra string
	fineDiffIDs := map[string]string{}
	for _, g := range []string{"fine", "coarse"} {
		for _, label := range in.Labels() {
			img, ok := in.Images[g][label]
			if !ok {
				return fmt.Errorf("no %s image for %s", g, label)
			}
			want := map[string]bool{}
			for _, p := range in.Manifests[label].Paths {
				want[p.Path] = true
				if _, ok := in.NarGzip[p.Path]; !ok {
					return fmt.Errorf("%s: no gzipped NAR size for %s", label, p.Path)
				}
			}
			got := map[string]bool{}
			storeLayers := 0
			for _, l := range img.Layers {
				if len(l.Paths) == 0 {
					if extra == "" {
						extra = l.DiffID
					}
					if l.DiffID != extra {
						return fmt.Errorf("%s/%s: second path-less layer %s (first %s)", g, label, l.DiffID, extra)
					}
					continue
				}
				storeLayers++
				if g == "fine" && len(l.Paths) != 1 {
					return fmt.Errorf("fine/%s: layer %s holds %d store paths", label, l.DiffID, len(l.Paths))
				}
				for _, p := range l.Paths {
					if g == "fine" {
						if previous, ok := fineDiffIDs[p]; ok && previous != l.DiffID {
							return fmt.Errorf("fine/%s: store path %s has inconsistent diffIDs: %s and %s", label, p, previous, l.DiffID)
						}
						fineDiffIDs[p] = l.DiffID
					}
					if got[p] {
						return fmt.Errorf("%s/%s: %s in two layers", g, label, p)
					}
					got[p] = true
				}
			}
			if g == "coarse" && storeLayers != 1 {
				return fmt.Errorf("coarse/%s: %d store-path layers, want 1", label, storeLayers)
			}
			var diff []string
			for p := range want {
				if !got[p] {
					diff = append(diff, "image lacks "+p)
				}
			}
			for p := range got {
				if !want[p] {
					diff = append(diff, "image adds "+p)
				}
			}
			if len(diff) > 0 {
				sort.Strings(diff)
				return fmt.Errorf("%s/%s differs from its manifest: %s", g, label, strings.Join(diff, "; "))
			}
		}
	}
	return nil
}
