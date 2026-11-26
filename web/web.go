// Package web holds the booking system's single-page web UI, embedded in
// the server binary so there is nothing extra to deploy.
package web

import (
	"embed"
	"net/http"
)

//go:embed index.html
var files embed.FS

// Handler serves the web UI's files, with index.html at "/".
func Handler() http.Handler {
	return http.FileServerFS(files)
}
