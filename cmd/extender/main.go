package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"k3snix/internal/closure"
	"k3snix/internal/extender"
)

func main() {
	var (
		listen   = flag.String("listen", ":9870", "http listen address")
		catalog  = flag.String("catalog", "", "directory of closure JSON files")
		agentURL = flag.String("agent-url", "http://{ip}:9860", "node agent base URL; {ip} is the node's InternalIP, {node} its name")
		timeout  = flag.Duration("agent-timeout", 500*time.Millisecond, "bound on one agent missing_bytes call; a slower node scores 0")
	)
	flag.Parse()
	if *catalog == "" {
		log.Fatal("-catalog is required")
	}
	cat, err := closure.LoadCatalog(*catalog)
	if err != nil {
		log.Fatalf("catalog %s: %v", *catalog, err)
	}
	s := extender.New(cat)
	s.AgentURL = *agentURL
	s.AgentTimeout = *timeout
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("k3snix-extender listening on %s, catalog %s, agents %s", *listen, *catalog, *agentURL)
	log.Fatal(srv.ListenAndServe())
}
