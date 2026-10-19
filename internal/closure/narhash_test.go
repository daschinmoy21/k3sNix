package closure

import "testing"

func TestNarHashNix32(t *testing.T) {
	// One hash in every accepted form; vectors from `nix hash convert`.
	const nix32 = "1ixxaz0n8nfxd2w29qq8frn6pjvdnjj3pl139gbi7jksrj9jd7jg"
	tests := []struct {
		name, in, want string
	}{
		{"nix32", "sha256:" + nix32, nix32},
		{"hex", "sha256:4f9e2693cc7aca13d74b23d03ba4b46dcb6b6c7608e324b868dd5964c157bdc7", nix32},
		{"sri", "sha256-T54mk8x6yhPXSyPQO6S0bctrbHYI4yS4aN1ZZMFXvcc=", nix32},
		{"empty", "", ""},
		{"no algo", nix32, ""},
		{"sha512", "sha512-T54mk8x6yhPXSyPQO6S0bctrbHYI4yS4aN1ZZMFXvcc=", ""},
		{"bad nix32 alphabet", "sha256:eixxaz0n8nfxd2w29qq8frn6pjvdnjj3pl139gbi7jksrj9jd7jg", ""},
		{"bad hex", "sha256:zz9e2693cc7aca13d74b23d03ba4b46dcb6b6c7608e324b868dd5964c157bdc7", ""},
		{"short sri", "sha256-AAAA", ""},
		{"bad base64", "sha256-!!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Path{NarHash: tt.in}).NarHashNix32(); got != tt.want {
				t.Errorf("NarHashNix32(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNarHashNix32Testdata(t *testing.T) {
	c, err := Parse(loadTestData(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Paths[0].NarHashNix32(), "1ixxaz0n8nfxd2w29qq8frn6pjvdnjj3pl139gbi7jksrj9jd7jg"; got != want {
		t.Fatalf("testdata narHash = %q, want %q", got, want)
	}
}
