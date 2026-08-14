package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
)

// Templates and static assets are embedded so the binary is self-contained.
// Deploying to a VM is then a single file copy — there is no asset directory to
// keep in sync with the executable, and no chance of serving a stale template
// after an upgrade.
//
//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// buildID is a short hash of the embedded static assets, used to version their
// URLs (/static/app.css?v=abc123).
//
// Without it, browsers hold onto cached CSS and JavaScript for as long as the
// Cache-Control lifetime allows — which during development means a changed
// stylesheet appears not to have taken effect at all, and the page behaves like
// a build from an hour ago. Hashing the content means the URL changes exactly
// when the bytes change: a stale asset becomes impossible rather than merely
// unlikely, and the files can then be cached aggressively and safely.
var buildID = hashStatic()

func hashStatic() string {
	h := sha256.New()
	// Walk in a fixed order so the same assets always produce the same hash.
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, readErr := staticFS.ReadFile(path)
		if readErr != nil {
			return nil
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}
