package static

import "embed"

// FS contains the embedded web/static directory.
// Accessed as static.FS by the handler package.
//
//go:embed files
var FS embed.FS
