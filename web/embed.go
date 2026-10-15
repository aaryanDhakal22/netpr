// Package web embeds templates and static assets into the binary.
package web

import "embed"

//go:embed templates
var Templates embed.FS

//go:embed static
var Static embed.FS
