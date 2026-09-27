package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// originClientTimeout bounds the probe's single GET so a wedged origin
// cannot hang the agent.
const originClientTimeout = 10 * time.Second

// originMaxBody bounds the probe's response so a misbehaving origin cannot
// OOM the agent or inflate k3snix_origin_bytes.
const originMaxBody = 1 << 20

// handleOriginProbe performs one GET of {origin}/nix-cache-info and records
// it in k3snix_origin_requests / k3snix_origin_bytes. Without -origin the
// endpoint answers 400 and never touches the counters. An upstream status
// other than 200 answers 502 and never touches the counters either: only a
// genuinely healthy origin moves them.
func (s *Server) handleOriginProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.Origin == "" {
		http.Error(w, "origin not configured", http.StatusBadRequest)
		return
	}

	client := &http.Client{Timeout: originClientTimeout}
	resp, err := client.Get(strings.TrimRight(s.Origin, "/") + "/nix-cache-info")
	if err != nil {
		http.Error(w, fmt.Sprintf("origin probe: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain at most originMaxBody so a small error body can reuse the
		// connection; anything longer is cut off by Close.
		io.Copy(io.Discard, io.LimitReader(resp.Body, originMaxBody))
		http.Error(w, fmt.Sprintf("origin probe: unexpected status %d", resp.StatusCode),
			http.StatusBadGateway)
		return
	}
	// Read one byte past the cap: a plain LimitReader would silently
	// truncate, and the probe would count and relay a half-body as valid.
	body, err := io.ReadAll(io.LimitReader(resp.Body, originMaxBody+1))
	if err != nil {
		http.Error(w, fmt.Sprintf("origin probe: %v", err), http.StatusBadGateway)
		return
	}
	if int64(len(body)) > originMaxBody {
		http.Error(w, "origin probe: response too large", http.StatusBadGateway)
		return
	}

	s.mu.Lock()
	s.originRequests++
	s.originBytes += int64(len(body))
	s.mu.Unlock()

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}
