// Command genident loads or creates an X25519 identity key and prints its
// base64 fingerprint. It is a developer/testing helper: e2e scripts and users
// who want to pre-create a keyfile and share its fingerprint before the GUI
// ever runs use it.
//
//	go run ./cmd/genident path/to/keyfile
package main

import (
	"fmt"
	"log"
	"os"

	"eliaukvpn/internal/crypto"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: genident <keyfile>\n")
		os.Exit(2)
	}
	id, err := crypto.LoadOrCreate(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(id.Fingerprint())
}
