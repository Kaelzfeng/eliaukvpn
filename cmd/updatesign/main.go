// updatesign signs a release manifest with an Ed25519 private key so clients
// built with the matching public key can verify it (see internal/update). It is
// a build-time tool; it is not shipped with the app.
//
// Usage:
//
//	updatesign -gen key.priv key.pub           # create a keypair (hex files)
//	updatesign -priv key.priv -in update.json  # sign an existing manifest
//
// The private key file is hex; keep it out of git and off the release box.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"eliaukvpn/internal/update"
)

func main() {
	gen := flag.Bool("gen", false, "generate an Ed25519 keypair: updatesign -gen key.priv key.pub")
	privPath := flag.String("priv", "", "path to the hex private key")
	in := flag.String("in", "update.json", "input manifest to sign")
	out := flag.String("out", "", "output manifest path (default: stdout)")
	flag.Parse()

	if *gen {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fatal(err)
		}
		if flag.NArg() < 2 {
			fatal("usage: updatesign -gen key.priv key.pub")
		}
		if err := os.WriteFile(flag.Arg(0), []byte(hex.EncodeToString(priv)+"\n"), 0o600); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(flag.Arg(1), []byte(hex.EncodeToString(pub)+"\n"), 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s (private) and %s (public)\n", flag.Arg(0), flag.Arg(1))
		return
	}

	privHex, err := os.ReadFile(*privPath)
	if err != nil {
		fatal(err)
	}
	priv, err := hex.DecodeString(string(trim(privHex)))
	if err != nil {
		fatal("private key is not valid hex: " + err.Error())
	}
	if len(priv) != ed25519.PrivateKeySize {
		fatal(fmt.Sprintf("private key has %d bytes, want %d", len(priv), ed25519.PrivateKeySize))
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal(err)
	}
	var m update.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		fatal(err)
	}
	if m.Version == "" || m.URL == "" || m.SHA256 == "" {
		fatal("manifest must contain version, url and sha256")
	}
	// The signature covers exactly version|url|sha256; recompute the same way
	// internal/update.Verify does.
	m.Signature = hex.EncodeToString(ed25519.Sign(priv, []byte(m.Version+"|"+m.URL+"|"+m.SHA256)))

	enc, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fatal(err)
	}
	if *out != "" {
		if err := os.WriteFile(*out, enc, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("signed manifest written to %s\n", *out)
		return
	}
	fmt.Println(string(enc))
}

func trim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func fatal(v any) {
	fmt.Fprintln(os.Stderr, "updatesign:", v)
	os.Exit(1)
}
