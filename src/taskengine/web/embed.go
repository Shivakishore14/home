package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed static/* templates/*
var Content embed.FS

// GetStaticFS returns the embedded static assets filesystem.
func GetStaticFS() (http.FileSystem, error) {
	sub, err := fs.Sub(Content, "static")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}

// GetTemplate parses the embedded dashboard template.
func GetTemplate() (*template.Template, error) {
	return template.ParseFS(Content, "templates/index.html")
}
