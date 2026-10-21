package agent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k3snix/internal/closure"
	"k3snix/internal/nixbase32"
)

// originClientTimeout bounds the probe's small GETs (nix-cache-info and
// narinfo) so a wedged origin cannot hang the agent.
const originClientTimeout = 10 * time.Second

// originNARTimeout bounds a probe's NAR download.
const originNARTimeout = 5 * time.Minute

// originMaxBody bounds the probe's small responses so a misbehaving origin
// cannot OOM the agent or inflate k3snix_origin_bytes.
const originMaxBody = 1 << 20

// defaultMaxOriginNARBytes applies when Server.MaxOriginNARBytes is unset.
const defaultMaxOriginNARBytes = 1 << 30

// maxProbeRequestBytes caps a /v1/origin_probe request body.
const maxProbeRequestBytes = 4 << 10

// OriginProbeReq selects what /v1/origin_probe fetches. An empty Hash
// probes {origin}/nix-cache-info; a 32-character store hash fetches that
// path's narinfo and NAR and verifies the NAR against the narinfo.
type OriginProbeReq struct {
	Hash string `json:"hash"`
}

// OriginNARProbe reports a verified NAR probe.
type OriginNARProbe struct {
	StorePath string `json:"store_path"`
	NarHash   string `json:"nar_hash"`
	NarSize   int64  `json:"nar_size"`
	Bytes     int64  `json:"bytes"` // narinfo plus NAR bytes read from the origin
	Verified  bool   `json:"verified"`
}

// handleOriginProbe measures the origin and records the traffic in
// k3snix_origin_requests / k3snix_origin_bytes. Without -origin the
// endpoint answers 400 and never touches the counters. A failed probe
// answers 502 and never touches the counters either: only a healthy origin
// serving verified bytes moves them. The NAR probe discards what it reads;
// nothing lands in the store.
func (s *Server) handleOriginProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.Origin == "" {
		http.Error(w, "origin not configured", http.StatusBadRequest)
		return
	}
	var req OriginProbeReq
	r.Body = http.MaxBytesReader(w, r.Body, maxProbeRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Hash == "" {
		s.probeCacheInfo(w)
		return
	}
	if !validStoreHash(req.Hash) {
		http.Error(w, "hash must be a 32-character nix store hash", http.StatusBadRequest)
		return
	}
	res, err := s.probeNAR(req.Hash)
	if err != nil {
		http.Error(w, fmt.Sprintf("origin probe: %v", err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (s *Server) probeCacheInfo(w http.ResponseWriter) {
	body, ctype, err := s.originGetSmall("/nix-cache-info")
	if err != nil {
		http.Error(w, fmt.Sprintf("origin probe: %v", err), http.StatusBadGateway)
		return
	}
	s.countOrigin(1, int64(len(body)))
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// probeNAR fetches {hash}.narinfo and the NAR it points at, and checks the
// NAR's size and SHA-256 against the narinfo. Both requests count only once
// the NAR verifies; a mismatch counts in k3snix_origin_verify_failures.
func (s *Server) probeNAR(hash string) (OriginNARProbe, error) {
	info, _, err := s.originGetSmall("/" + hash + ".narinfo")
	if err != nil {
		return OriginNARProbe{}, err
	}
	fields := parseNarinfoFields(info)
	if c := fields["Compression"]; c != "" && c != "none" {
		return OriginNARProbe{}, fmt.Errorf("narinfo compression %q is not supported", c)
	}
	want := closure.NormalizeSHA256(fields["NarHash"])
	if want == "" {
		return OriginNARProbe{}, fmt.Errorf("narinfo NarHash %q is not a sha256", fields["NarHash"])
	}
	size, err := strconv.ParseInt(fields["NarSize"], 10, 64)
	if err != nil || size <= 0 {
		return OriginNARProbe{}, fmt.Errorf("narinfo NarSize %q", fields["NarSize"])
	}
	limit := s.MaxOriginNARBytes
	if limit <= 0 {
		limit = defaultMaxOriginNARBytes
	}
	if size > limit {
		return OriginNARProbe{}, fmt.Errorf("NarSize %d exceeds the %d-byte probe cap", size, limit)
	}
	url := fields["URL"]
	if url == "" || strings.Contains(url, "://") || strings.HasPrefix(url, "/") {
		return OriginNARProbe{}, fmt.Errorf("narinfo URL %q is not a relative path", url)
	}

	client := &http.Client{Timeout: originNARTimeout}
	resp, err := client.Get(strings.TrimRight(s.Origin, "/") + "/" + url)
	if err != nil {
		return OriginNARProbe{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, originMaxBody))
		return OriginNARProbe{}, fmt.Errorf("NAR: unexpected status %d", resp.StatusCode)
	}
	h := sha256.New()
	// One byte past NarSize so an oversized body fails instead of hashing
	// a prefix that happens to match.
	n, err := io.Copy(h, io.LimitReader(resp.Body, size+1))
	if err != nil {
		return OriginNARProbe{}, err
	}
	got := nixbase32.EncodeToString(h.Sum(nil))
	if n != size || got != want {
		s.mu.Lock()
		s.originVerifyFailures++
		s.mu.Unlock()
		return OriginNARProbe{}, fmt.Errorf("NAR is sha256:%s (%d bytes), narinfo says sha256:%s (%d bytes)",
			got, n, want, size)
	}
	total := int64(len(info)) + n
	s.countOrigin(2, total)
	return OriginNARProbe{
		StorePath: fields["StorePath"],
		NarHash:   "sha256:" + want,
		NarSize:   size,
		Bytes:     total,
		Verified:  true,
	}, nil
}

// originGetSmall GETs a small origin document, bounded by originMaxBody.
func (s *Server) originGetSmall(path string) ([]byte, string, error) {
	client := &http.Client{Timeout: originClientTimeout}
	resp, err := client.Get(strings.TrimRight(s.Origin, "/") + path)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain at most originMaxBody so a small error body can reuse the
		// connection; anything longer is cut off by Close.
		io.Copy(io.Discard, io.LimitReader(resp.Body, originMaxBody))
		return nil, "", fmt.Errorf("%s: unexpected status %d", path, resp.StatusCode)
	}
	// Read one byte past the cap: a plain LimitReader would silently
	// truncate, and the probe would count and relay a half-body as valid.
	body, err := io.ReadAll(io.LimitReader(resp.Body, originMaxBody+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > originMaxBody {
		return nil, "", fmt.Errorf("%s: response too large", path)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func (s *Server) countOrigin(requests, n int64) {
	s.mu.Lock()
	s.originRequests += requests
	s.originBytes += n
	s.mu.Unlock()
}

// parseNarinfoFields splits a narinfo into its "Key: value" fields.
func parseNarinfoFields(info []byte) map[string]string {
	fields := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(info))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ": "); ok {
			fields[k] = v
		}
	}
	return fields
}
