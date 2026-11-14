// Package assets holds files built into the function.
package assets

import _ "embed"

// Icon is the Slack app icon: a 1024x1024 PNG, inside Slack's 512 to 2000
// pixel square range. icon.svg is its source.
//
//go:embed icon.png
var Icon []byte
