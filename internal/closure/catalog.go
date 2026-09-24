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
}

func LoadCatalog(dir string) (*Catalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load catalog %s: %w", dir, err)
	}
	
	cat := &Catalog{
		byFingerprint: map[string]Closure{},
		byLabel:       map[string]string{},
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
		if c.Label == "" {
			continue
		}

		if prev, ok := cat.byLabel[c.Label]; ok && prev != fingerprint {
			return nil, fmt.Errorf(
				"load catalog %s: label %q already maps to closure %s",path, c.Label, prev
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
