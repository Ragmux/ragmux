// Package web embeds the management dashboard.
package web

import "embed"

// FS holds the static dashboard assets: the single-page dashboard and the
// self-hosted web fonts it references (served under /admin/fonts/) and the
// favicon shared with ragmux.com.
//
//go:embed index.html favicon.svg fonts/*.woff2 fonts/OFL-*.txt
var FS embed.FS
