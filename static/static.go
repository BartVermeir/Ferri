package static

import "embed"

// FS contains the embedded files directory.
// Accessed as static.FS by the handler package.
//
//go:embed files
var FS embed.FS
