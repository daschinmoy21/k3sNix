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

// handleOriginProbe performs one GET of {origin}/nix-cache-info and records
// it in k3snix_origin_requests / k3snix_origin_bytes. Without -origin the
// endpoint answers 400 and never touches the counters.
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("origin probe: %v", err), http.StatusBadGateway)
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
