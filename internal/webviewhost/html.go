package webviewhost

import _ "embed"

//go:embed app.html
var appHTML string

// PageHTML returns the embedded frontend document.
func PageHTML() string { return appHTML }
