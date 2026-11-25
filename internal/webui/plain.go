//go:build !embedui

package webui

import "net/http"

func Embedded() bool                     { return false }
func Wrap(api http.Handler) http.Handler { return api }
