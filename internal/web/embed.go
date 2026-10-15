package web

import (
	"embed"
	"io/fs"
	"strings"
)

// The whole interface is embedded in the binary: no Node toolchain, no
// build step, no files to copy onto the server. Editing the CSS and
// rebuilding the binary is the entire development loop.
//
//go:embed static
var staticFS embed.FS

//go:embed templates
var templateFS embed.FS

// PageInfo is injected into the HTML shell.
type PageInfo struct {
	BasePath string
	Version  string
}

// renderIndex returns the single page with its base path and version
// substituted, so that the same binary works at any URL prefix.
func renderIndex(basePath string) ([]byte, error) {
	raw, err := fs.ReadFile(templateFS, "templates/index.html")
	if err != nil {
		return nil, err
	}
	page := string(raw)
	page = strings.ReplaceAll(page, "{{BASE_PATH}}", basePath)
	page = strings.ReplaceAll(page, "{{ASSET_VERSION}}", assetVersion)
	return []byte(page), nil
}

// assetVersion busts browser caches when the embedded files change. It is
// set by the build through -ldflags, and falls back to the release number.
var assetVersion = Version

// Version is the panel version, overridden at build time with
//
//	go build -ldflags "-X github.com/nelvixboy/eye-of-lotus/internal/web.Version=v1.0.0"
var Version = "dev"
