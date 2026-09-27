// Package nixbase32 encodes hashes in the base32 alphabet Nix uses.
package nixbase32

import "strings"

const alphabet = "0123456789abcdfghijklmnpqrsvwxyz"

// EncodeToString encodes src with Nix's base32 alphabet. Five-bit groups are
// read from the low end of src and the characters are emitted highest group
// first. The output length is ceil(len(src)*8/5); empty input encodes to "".
// IsValid reports whether every byte of s is in Nix's base32 alphabet. The
// empty string is valid.
func IsValid(s string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

func EncodeToString(src []byte) string {
	if len(src) == 0 {
		return ""
	}

	n := (len(src)*8 + 4) / 5
	out := make([]byte, n)
	for i := range out {
		bit := i * 5
		b := bit / 8
		shift := uint(bit % 8)

		v := uint16(src[b]) >> shift
		if b+1 < len(src) {
			v |= uint16(src[b+1]) << (8 - shift)
		}
		out[n-1-i] = alphabet[v&0x1f]
	}
	return string(out)
}
