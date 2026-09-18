// Package web embeds the management dashboard.
package web

import "embed"

// FS holds the static dashboard assets.
//
//go:embed index.html
var FS embed.FS
