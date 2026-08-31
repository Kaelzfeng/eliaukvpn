// Command server runs the Eliauk VPN coordination server.
//
// The coordination server only does signalling: it registers clients, hands
// out virtual IPs and exchanges peer/endpoint info. Game traffic travels
// peer-to-peer between clients (milestones M2+).
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"

	"eliaukvpn/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address, e.g. :8080")
	flag.Parse()

	reg := server.NewRegistry()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.HandleWS(reg, w, r)
	})

	// Debug endpoint: dump the registry as JSON.
	http.HandleFunc("/peers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.Peers(""))
	})

	log.Printf("eliauk coordination server listening on %s", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}
