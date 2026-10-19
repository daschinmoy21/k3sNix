package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// SigningKey signs narinfo files the way nix does, so a nix client that
// lists the matching public key in trusted-public-keys accepts paths from
// this cache without require-sigs = false.
type SigningKey struct {
	Name string
	key  ed25519.PrivateKey
}

// LoadSigningKey reads a nix secret key file, "<name>:<base64 of the
// 64-byte ed25519 private key>", as written by `nix key generate-secret`.
func LoadSigningKey(file string) (*SigningKey, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return ParseSigningKey(strings.TrimSpace(string(data)))
}

// ParseSigningKey parses a nix secret key, "<name>:<base64>".
func ParseSigningKey(s string) (*SigningKey, error) {
	name, b64, ok := strings.Cut(s, ":")
	if !ok || name == "" {
		return nil, fmt.Errorf("signing key: want <name>:<base64>")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signing key %s: %w", name, err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key %s: %d bytes, want %d", name, len(raw), ed25519.PrivateKeySize)
	}
	return &SigningKey{Name: name, key: ed25519.PrivateKey(raw)}, nil
}

// PublicKey returns the key in nix's trusted-public-keys form.
func (k *SigningKey) PublicKey() string {
	pub := k.key.Public().(ed25519.PublicKey)
	return k.Name + ":" + base64.StdEncoding.EncodeToString(pub)
}

// Sign returns the narinfo Sig value for a path.
func (k *SigningKey) Sign(storePath, narHash string, narSize int64, refs []string) string {
	sig := ed25519.Sign(k.key, []byte(narFingerprint(storePath, narHash, narSize, refs)))
	return k.Name + ":" + base64.StdEncoding.EncodeToString(sig)
}

// narFingerprint is the message nix signs: "1;<store path>;<nar hash>;
// <nar size>;<comma-separated full reference paths, sorted>". narHash
// carries its "sha256:" prefix.
func narFingerprint(storePath, narHash string, narSize int64, refs []string) string {
	sorted := append([]string(nil), refs...)
	sort.Strings(sorted)
	return "1;" + storePath + ";" + narHash + ";" + strconv.FormatInt(narSize, 10) + ";" + strings.Join(sorted, ",")
}
