package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k3snix/internal/closure"
)

// Seed roles. Warm holds every closure path, mid holds the first half
// (rounding down), cold holds none. They are the values the agent logs and
// the e2e smoke asserts on.
const (
	RoleWarm = "warm"
	RoleMid  = "mid"
	RoleCold = "cold"
)

// RoleForNode classifies a node name for seeding:
//
//   - contains "warm" or ends with "server-0" -> warm (every path)
//   - contains "mid" or contains "agent-0"    -> mid (first half)
//   - anything else                           -> cold (no paths)
func RoleForNode(node string) string {
	switch {
	case strings.Contains(node, "warm") || strings.HasSuffix(node, "server-0"):
		return RoleWarm
	case strings.Contains(node, "mid") || strings.Contains(node, "agent-0"):
		return RoleMid
	default:
		return RoleCold
	}
}

// SeedByNode creates the role's share of c's paths under storeDir. Each
// seeded path becomes a directory named by the basename containing one
// regular file "payload" whose bytes are "k3snix-seed:<basename>\n". It
// returns the role and how many paths were seeded.
func SeedByNode(storeDir, node string, c closure.Closure) (role string, created int, err error) {
	role = RoleForNode(node)

	paths := make([]closure.Path, len(c.Paths))
	copy(paths, c.Paths)
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })

	var keep int
	switch role {
	case RoleWarm:
		keep = len(paths)
	case RoleMid:
		keep = len(paths) / 2
	case RoleCold:
		keep = 0
	}

	for _, p := range paths[:keep] {
		base := filepath.Base(p.Path)
		dir := filepath.Join(storeDir, base)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return role, created, fmt.Errorf("seed %s: %w", dir, err)
		}
		payload := "k3snix-seed:" + base + "\n"
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(payload), 0o644); err != nil {
			return role, created, fmt.Errorf("seed %s: %w", dir, err)
		}
		created++
	}
	return role, created, nil
}
