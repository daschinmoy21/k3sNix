package closure

import (
	"encoding/base64"
	"encoding/hex"
	"strings"

	"k3snix/internal/nixbase32"
)

// NarHashNix32 returns p's NAR hash as nix base32 SHA-256, the form a
// narinfo NarHash carries after "sha256:". It accepts "sha256:<nix32>"
// (exportReferencesGraph, nix-serve), "sha256:<hex>" and the SRI form
// "sha256-<base64>" (nix path-info --json). Anything else, including an
// absent hash, returns "", which callers treat as "not known".
func (p Path) NarHashNix32() string {
	return NormalizeSHA256(p.NarHash)
}

// NormalizeSHA256 converts a SHA-256 hash in any form NarHashNix32 accepts
// to nix base32, or returns "" when s is not one of them.
func NormalizeSHA256(s string) string {
	var sum []byte
	switch {
	case strings.HasPrefix(s, "sha256:"):
		v := strings.TrimPrefix(s, "sha256:")
		switch len(v) {
		case 52:
			if nixbase32.IsValid(v) {
				return v
			}
			return ""
		case 64:
			b, err := hex.DecodeString(v)
			if err != nil {
				return ""
			}
			sum = b
		default:
			return ""
		}
	case strings.HasPrefix(s, "sha256-"):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "sha256-"))
		if err != nil {
			return ""
		}
		sum = b
	default:
		return ""
	}
	if len(sum) != 32 {
		return ""
	}
	return nixbase32.EncodeToString(sum)
}
