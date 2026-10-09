// Package terms embeds the terms texts in docs/terms so Lens shows a workspace the words it accepts (B30.9): each
// capability's terms are <capability>.md, published as version 1 when Lens starts and as each next version by
// `lens terms publish`.
package terms

import "embed"

//go:embed *.md
var FS embed.FS
