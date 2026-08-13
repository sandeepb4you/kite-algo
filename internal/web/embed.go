package web

import "embed"

// Templates and static assets are embedded so the binary is self-contained.
// Deploying to a VM is then a single file copy — there is no asset directory to
// keep in sync with the executable, and no chance of serving a stale template
// after an upgrade.
//
//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS
