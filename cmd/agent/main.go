package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"k3snix/internal/agent"
)

// probeTimeout bounds the one-shot client so a wedged peer fails the e2e
// smoke instead of hanging the kubectl exec forever.
const probeTimeout = 30 * time.Second

func main() {
	var (
		listen     = flag.String("listen", ":9860", "http listen address")
		store      = flag.String("store", "/nix/store", "nix store directory")
		catalog    = flag.String("catalog", "", "directory of closure JSON files")
		ttl        = flag.Duration("ttl", 2*time.Second, "store listing TTL")
		dumpTTL    = flag.Duration("dump-ttl", 5*time.Minute, "how long an idle NAR dump stays cached")
		dumpBytes  = flag.Int64("dump-cache-bytes", 2<<30, "cap on the total size of cached NAR dumps")
		origin     = flag.String("origin", "", "upstream origin cache base URL for /v1/origin_probe")
		seedByNode = flag.Bool("seed-by-node", false, "seed the store from the label closure according to NODE_NAME")
		seedLabel  = flag.String("seed-label", "e2e", "closure label used when -seed-by-node is set")
		probeURL   = flag.String("probe-url", "", "one-shot client: GET this URL (POST with -probe-body), print the response body, exit")
		probeBody  = flag.String("probe-body", "", "request body for -probe-url; empty body sends GET")
	)
	flag.Parse()

	if *probeURL != "" {
		probe(*probeURL, *probeBody)
		return
	}

	s := agent.New(*store)
	s.Store.TTL = *ttl
	s.DumpTTL = *dumpTTL
	s.MaxDumpBytes = *dumpBytes
	s.Origin = *origin
	if *catalog != "" {
		if err := s.LoadCatalog(*catalog); err != nil {
			log.Fatalf("catalog %s: %v", *catalog, err)
		}
		log.Printf("catalog loaded from %s", *catalog)
	}
	if *seedByNode {
		seed(s, *seedLabel)
	}
	if err := s.Store.Refresh(); err != nil {
		log.Printf("store listing: %v (will retry on demand)", err)
	} else {
		log.Printf("store %s: %d paths", s.Store.Dir, s.Store.Count())
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("k3snix-agent listening on %s (pid %d)", *listen, os.Getpid())
	log.Fatal(srv.ListenAndServe())
}

// seed fills the store for this node's role and logs one line with the
// role, the number of created paths, and the closure fingerprint.
func seed(s *agent.Server, label string) {
	node := os.Getenv("NODE_NAME")
	if node == "" {
		log.Fatal("seed-by-node requires NODE_NAME")
	}
	if s.Catalog == nil {
		log.Fatal("seed-by-node requires a catalog (-catalog)")
	}
	c, ok := s.Catalog.ByLabel(label)
	if !ok {
		log.Fatalf("seed: catalog has no closure labeled %q", label)
	}
	role, created, err := agent.SeedByNode(s.Store.Dir, node, c)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Printf("seed node=%s role=%s paths=%d fingerprint=%s", node, role, created, c.Fingerprint())
}

// probe sends a single HTTP request and streams the response body to
// stdout. It exists so the e2e smoke can use kubectl exec as an HTTP
// client: the agent image ships no shell or other HTTP tools.
func probe(url, body string) {
	var (
		req *http.Request
		err error
	)
	if body == "" {
		req, err = http.NewRequest(http.MethodGet, url, nil)
	} else {
		req, err = http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		log.Fatalf("probe %s: %v", url, err)
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("probe %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("probe %s: %v", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		os.Stderr.Write(out)
		log.Fatalf("probe %s: status %d", url, resp.StatusCode)
	}
	os.Stdout.Write(out)
}
