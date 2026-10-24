// Package assets holds the application artwork.
package assets

import "embed"

//go:embed icon.png
var Icon []byte

//go:embed icons/*.svg
var Icons embed.FS
