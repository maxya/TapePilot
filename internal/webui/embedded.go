//go:build embedui

package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

// Build-native stages generated static files here in its temporary source tree.
// all: includes SvelteKit's underscore-prefixed immutable asset directories.
//
//go:embed all:assets
var embedded embed.FS

func Embedded() bool { return true }
func Wrap(api http.Handler) http.Handler {
	assets, err := fs.Sub(embedded, "assets")
	if err != nil {
		panic(err)
	}
	return staticHandler(assets, api)
}
