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
	// relayPublic is advertised to clients so they know where to send relay
	// traffic. For local testing 127.0.0.1 works; on a real VPS set it to the
	// server's public IP.
	relayPublic := flag.String("relay-public", "127.0.0.1:8081", "relay endpoint advertised to clients")
	relayListen := flag.String("relay-listen", "0.0.0.0:8081", "relay listen address")
	flag.Parse()

	reg := server.NewRegistry()

	relay, err := server.NewRelay(reg, *relayListen)
	if err != nil {
		log.Fatalf("start relay: %v", err)
	}
	defer relay.Close()
	log.Printf("udp relay listening on %s (advertised as %s)", relay.Addr(), *relayPublic)

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.HandleWS(reg, *relayPublic, w, r)
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
