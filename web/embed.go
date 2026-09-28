package web

import "embed"

// Templates contains all HTML templates embedded at build time.
//
//go:embed templates
var Templates embed.FS
