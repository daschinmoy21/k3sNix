package closure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Path struct {
	Path       string   `json:"path"`
	NarSize    uint64   `json:"narSize"`
	NarHash    string   `json:"narHash,omitempty"`
	References []string `json:"references,omitempty"`
}

type Closure struct {
	Label string `json:"label,omitempty"`
	Root  string `json:"root,omitempty"`
	Paths []Path `json:"paths"`
}

type Missing struct {
	MissingBytes uint64   `json:"missingBytes"`
	HaveBytes    uint64   `json:"haveBytes"`
	TotalBytes   uint64   `json:"totalBytes"`
	HavePaths    int      `json:"havePaths"`
	TotalPaths   int      `json:"totalPaths"`
	MissingPaths []string `json:"missingPaths"`
}

func Parse(data []byte) (Closure, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Closure{}, fmt.Errorf("parse closure: %w", err)
	}
	if isProjectDoc(raw) {
		return parseProjectDoc(raw)
	}
	return parsePathInfo(raw)
}

func isProjectDoc(raw map[string]json.RawMessage) bool {
	for _, k := range []string{"paths", "label", "id", "imageName", "root"} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}

func parseProjectDoc(raw map[string]json.RawMessage) (Closure, error) {
	label, err := labelFrom(raw, "label", "id", "imageName")
	if err != nil {
		return Closure{}, err
	}
	var root string
	if v, ok := raw["root"]; ok {
		if err := json.Unmarshal(v, &root); err != nil {
			return Closure{}, fmt.Errorf("parse closure root: %w", err)
		}
	}
	var paths []Path
	if v, ok := raw["paths"]; ok {
		if err := json.Unmarshal(v, &paths); err != nil {
			return Closure{}, fmt.Errorf("parse closure %q paths: %w", label, err)
		}
	}
	if len(paths) == 0 {
		return Closure{}, fmt.Errorf("parse closure %q: empty paths", label)
	}
	for _, p := range paths {
		if p.Path == "" {
			return Closure{}, fmt.Errorf("parse closure %q: path entry with empty path", label)
		}
	}
	sortPaths(paths)
	return Closure{Label: label, Root: root, Paths: paths}, nil
}

func labelFrom(raw map[string]json.RawMessage, keys ...string) (string, error) {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", fmt.Errorf("parse closure %s: %w", k, err)
		}
		if s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("parse closure: missing label (expected label, id, or imageName)")
}

func parsePathInfo(raw map[string]json.RawMessage) (Closure, error) {
	c := Closure{}
	for key, v := range raw {
		if key == "version" {
			continue
		}
		if !strings.HasPrefix(key, "/") {
			return Closure{}, fmt.Errorf("parse closure: unexpected key %q", key)
		}
		var p Path
		if err := json.Unmarshal(v, &p); err != nil {
			return Closure{}, fmt.Errorf("parse closure %s: %w", key, err)
		}
		p.Path = key
		c.Paths = append(c.Paths, p)
	}
	if len(c.Paths) == 0 {
		return Closure{}, fmt.Errorf("parse closure: empty paths")
	}
	sortPaths(c.Paths)
	return c, nil
}

func sortPaths(paths []Path) {
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
}

func (c Closure) Fingerprint() string {
	paths := make([]Path, len(c.Paths))
	copy(paths, c.Paths)
	sortPaths(paths)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s %d\n", p.Path, p.NarSize)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func MissingAgainst(c Closure, have map[string]struct{}) Missing {
	m := Missing{TotalPaths: len(c.Paths), MissingPaths: []string{}}
	for _, p := range c.Paths {
		m.TotalBytes += p.NarSize
		if _, ok := have[p.Path]; ok {
			m.HaveBytes += p.NarSize
			m.HavePaths++
			continue
		}
		m.MissingBytes += p.NarSize
		m.MissingPaths = append(m.MissingPaths, p.Path)
	}
	return m
}

func DiffBytes(a, b Closure) uint64 {
	have := make(map[string]struct{}, len(a.Paths))
	for _, p := range a.Paths {
		have[p.Path] = struct{}{}
	}
	return MissingAgainst(b, have).MissingBytes
}
