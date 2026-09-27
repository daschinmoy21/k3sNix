package agent

import (
	"os"
	"path/filepath"
	"testing"

	"k3snix/internal/closure"
)

func fixtureClosure() closure.Closure {
	return closure.Closure{Label: "e2e", Paths: []closure.Path{
		{Path: "/nix/store/dddddddddddddddddddddddddddddddd-d", NarSize: 80},
		{Path: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a", NarSize: 10},
		{Path: "/nix/store/cccccccccccccccccccccccccccccccc-c", NarSize: 40},
		{Path: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b", NarSize: 20},
	}}
}

func TestSeedByNodeRoles(t *testing.T) {
	for _, tc := range []struct {
		node string
		role string
		want int
	}{
		{"k3d-k3snix-server-0", RoleWarm, 4},
		{"k3d-k3snix-agent-0", RoleMid, 2},
		{"k3d-k3snix-agent-1", RoleCold, 0},
	} {
		store := t.TempDir()
		role, created, err := SeedByNode(store, tc.node, fixtureClosure())
		if err != nil {
			t.Fatalf("%s: %v", tc.node, err)
		}
		if role != tc.role {
			t.Fatalf("%s: role %q, want %q", tc.node, role, tc.role)
		}
		if created != tc.want {
			t.Fatalf("%s: created %d, want %d", tc.node, created, tc.want)
		}
		ents, err := os.ReadDir(store)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(ents); got != tc.want {
			t.Fatalf("%s: %d seeded dirs, want %d", tc.node, got, tc.want)
		}
	}
}

func TestSeedByNodeMidHalf(t *testing.T) {
	// Mid seeds the first half of the sorted closure paths, rounding down.
	store := t.TempDir()
	if _, created, err := SeedByNode(store, "k3d-k3snix-agent-0", fixtureClosure()); err != nil {
		t.Fatal(err)
	} else if created != 2 {
		t.Fatalf("created %d, want 2", created)
	}
	for _, base := range []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b",
	} {
		if _, err := os.Stat(filepath.Join(store, base, "payload")); err != nil {
			t.Fatalf("payload missing for %s: %v", base, err)
		}
	}
	for _, base := range []string{
		"cccccccccccccccccccccccccccccccc-c",
		"dddddddddddddddddddddddddddddddd-d",
	} {
		if _, err := os.Stat(filepath.Join(store, base)); !os.IsNotExist(err) {
			t.Fatalf("%s seeded for mid node", base)
		}
	}
}

func TestSeedByNodePayloadContent(t *testing.T) {
	store := t.TempDir()
	if _, _, err := SeedByNode(store, "k3d-k3snix-server-0", fixtureClosure()); err != nil {
		t.Fatal(err)
	}
	base := "cccccccccccccccccccccccccccccccc-c"
	data, err := os.ReadFile(filepath.Join(store, base, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "k3snix-seed:" + base + "\n"; string(data) != want {
		t.Fatalf("payload %q, want %q", data, want)
	}
}
