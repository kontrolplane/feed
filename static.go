package feed

import "embed"

// StaticFS carries the stylesheets, scripts, fonts and images the server
// hands to the browser.
//
// They used to be served with http.FileServer(http.Dir("static")), a path
// resolved against the process's working directory. That only ever worked
// because the Dockerfile happens to set WORKDIR /app next to a copied
// static/ tree: running the binary from anywhere else — a systemd unit
// without WorkingDirectory, `go run ./cmd/server` from a subdirectory, a
// container started with --workdir — served 404 for every asset and left
// the app rendering unstyled with no JavaScript, with nothing in the log
// to say why.
//
// Embedding makes the binary genuinely self-contained, which is the
// promise the README already makes ("a single binary serves a
// server-rendered three-pane UI"). It also removes the runtime copy of
// static/ from the image.
//
// This lives at the module root rather than under cmd/server because
// go:embed cannot reach outside the directory holding the source file.
//
//go:embed static
var StaticFS embed.FS
