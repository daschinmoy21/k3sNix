package nixbase32

import (
	"encoding/hex"
	"testing"
)

func TestEncodeToString(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		want string
	}{
		// Vectors taken from `nix hash convert --to nix32`.
		{"sha256 abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "1b8m03r63zqhnjf7l5wnldhh7c134ap5vpj0850ymkq1iyzicy5s"},
		{"sha256 empty", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73"},
		{"sha1 abc", "a9993e364706816aba3e25717850c26c9cd0d89d", "kpcd173cq987hw957sx6m0868wv3x6d9"},
		{"md5 abc", "900150983cd24fb0d6963f7d28e17f72", "3jgzhjhz9zjvbb0kyj7jc500ch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := hex.DecodeString(tt.hex)
			if err != nil {
				t.Fatal(err)
			}
			if got := EncodeToString(src); got != tt.want {
				t.Errorf("EncodeToString(%s) = %q, want %q", tt.hex, got, tt.want)
			}
		})
	}
}

func TestEncodeToStringEmpty(t *testing.T) {
	if got := EncodeToString(nil); got != "" {
		t.Errorf("EncodeToString(nil) = %q, want empty", got)
	}
	if got := EncodeToString([]byte{}); got != "" {
		t.Errorf("EncodeToString([]byte{}) = %q, want empty", got)
	}
}
