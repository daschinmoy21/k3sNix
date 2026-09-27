package nar

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Nix marks a file executable from the owner execute bit alone; group and
// other bits do not count.
var execModes = []struct {
	mode os.FileMode
	exec bool
}{
	{0o644, false},
	{0o650, false}, // group execute only
	{0o605, false}, // other execute only
	{0o611, false},
	{0o700, true},
	{0o744, true},
	{0o755, true},
}

func TestExecutableIsOwnerBitOnly(t *testing.T) {
	for _, tc := range execModes {
		t.Run(fmt.Sprintf("%04o", tc.mode), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			writeFile(t, p, "x", tc.mode)
			if got := decode(t, dump(t, p)).exec; got != tc.exec {
				t.Fatalf("executable = %v, want %v", got, tc.exec)
			}
		})
	}
}

func TestExecutableMatchesNixStoreDump(t *testing.T) {
	nixStore, err := exec.LookPath("nix-store")
	if err != nil {
		t.Skip("nix-store not in PATH")
	}
	for _, tc := range execModes {
		t.Run(fmt.Sprintf("%04o", tc.mode), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			writeFile(t, p, "x", tc.mode)
			want, err := exec.Command(nixStore, "--dump", p).Output()
			if err != nil {
				t.Fatalf("nix-store --dump: %v", err)
			}
			if got := dump(t, p); !bytes.Equal(got, want) {
				t.Fatalf("NAR differs from nix-store --dump at %s", firstDiff(got, want))
			}
		})
	}
}
