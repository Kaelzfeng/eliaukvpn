package update

// UpdatePubKey is the hex-encoded Ed25519 public key that release manifests
// are signed against. Empty disables signature verification (SHA-256 of the
// exe still applies). Override at build time with:
//
//	go build -ldflags "-X eliaukvpn/internal/update.UpdatePubKey=<hex>"
//
// The matching private key is generated with `updatesign -gen` and must never
// be committed or shipped; only the public half belongs in the binary.
var UpdatePubKey = "1a3202a6ac81bf7b034469e5c88c3bc7ee6aa9d304a26407a3f38653ee4ada8b"
