package closure

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Catalog struct {
	byFingerprint map[string]Closure
	byLabel       map[string]string
	byRoot        map[string]string // root store path -> fingerprint
}

func LoadCatalog(dir string) (*Catalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load catalog %s: %w", dir, err)
	}
	cat := &Catalog{
		byFingerprint: map[string]Closure{},
		byLabel:       map[string]string{},
		byRoot:        map[string]string{},
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("load catalog %s: %w", e.Name(), err)
		}
		c, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("load catalog %s: %w", e.Name(), err)
		}
		fingerprint := c.Fingerprint()
		if _, ok := cat.byFingerprint[fingerprint]; !ok {
			cat.byFingerprint[fingerprint] = c
		}
		if c.Root != "" {
			if prev, ok := cat.byRoot[c.Root]; ok && prev != fingerprint {
				return nil, fmt.Errorf(
					"load catalog %s: root %s already maps to closure %s", e.Name(), c.Root, prev,
				)
			}
			cat.byRoot[c.Root] = fingerprint
		}
		if c.Label == "" {
			continue
		}

		if prev, ok := cat.byLabel[c.Label]; ok && prev != fingerprint {
			return nil, fmt.Errorf(
				"load catalog %s: label %q already maps to closure %s", e.Name(), c.Label, prev,
			)
		}
		cat.byLabel[c.Label] = fingerprint
	}
	return cat, nil
}

func (c *Catalog) Lookup(id string) (Closure, bool) {
	cl, ok := c.byFingerprint[id]
	return cl, ok
}

func (c *Catalog) ByLabel(label string) (Closure, bool) {
	fingerprint, ok := c.byLabel[label]
	if !ok {
		return Closure{}, false
	}
	cl, ok := c.byFingerprint[fingerprint]
	if !ok {
		return Closure{}, false
	}
	cl.Label = label
	return cl, true
}

// ByRoot returns the closure whose manifest records root as its root store
// path. A pod names its software by root path; the closure, and so its ID,
// always comes from the catalog.
func (c *Catalog) ByRoot(root string) (Closure, bool) {
	fingerprint, ok := c.byRoot[root]
	if !ok {
		return Closure{}, false
	}
	cl, ok := c.byFingerprint[fingerprint]
	return cl, ok
}

// Closures returns every distinct closure in the catalog, one per
// fingerprint, in no particular order.
func (c *Catalog) Closures() []Closure {
	out := make([]Closure, 0, len(c.byFingerprint))
	for _, cl := range c.byFingerprint {
		out = append(out, cl)
	}
	return out
}
