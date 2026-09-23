package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"k3snix/internal/agent"
)

func main() {
	var (
		listen  = flag.String("listen", ":9860", "http listen address")
		store   = flag.String("store", "/nix/store", "nix store directory")
		catalog = flag.String("catalog", "", "directory of closure JSON files")
		ttl     = flag.Duration("ttl", 2*time.Second, "store listing TTL")
	)
	flag.Parse()

	s := agent.New(*store)
	s.Store.TTL = *ttl
	if *catalog != "" {
		if err := s.LoadCatalog(*catalog); err != nil {
			log.Fatalf("catalog %s: %v", *catalog, err)
		}
		log.Printf("catalog loaded from %s", *catalog)
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
