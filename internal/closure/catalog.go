package closure

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Catalog struct {
	byFingerprint map[string]Closure
	byLabel       map[string]Closure
}

func LoadCatalog(dir string) (*Catalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load catalog %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	cat := &Catalog{
		byFingerprint: map[string]Closure{},
		byLabel:       map[string]Closure{},
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("load catalog %s: %w", name, err)
		}
		c, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("load catalog %s: %w", name, err)
		}
		cat.byFingerprint[c.Fingerprint()] = c
		if c.Label != "" {
			cat.byLabel[c.Label] = c
		}
	}
	return cat, nil
}

func (c *Catalog) Lookup(id string) (Closure, bool) {
	cl, ok := c.byFingerprint[id]
	return cl, ok
}

func (c *Catalog) ByLabel(label string) (Closure, bool) {
	cl, ok := c.byLabel[label]
	return cl, ok
}
