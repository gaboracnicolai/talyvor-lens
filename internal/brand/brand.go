// Package brand carries the Talyvor brand (v4, the 4 Oct 2026 board) for the
// pages Lens renders itself: the colour tokens, the type stack and the mark.
//
// The two SVGs beside this file are byte-for-byte copies of
// brand-v4/svg/talyvor-mark-flat-{dark,light}.svg. Never redraw or recolour
// them; replace the files instead.
//
// Nothing here requests a font: Space Grotesk and IBM Plex Mono are named first
// and fall back to the system stack, so an API host never makes a visitor's
// browser talk to a third party.
package brand

import (
	_ "embed"
	"strings"
)

//go:embed talyvor-mark-flat-dark.svg
var markFlatDark string

//go:embed talyvor-mark-flat-light.svg
var markFlatLight string

// Mark is the flat mark for inline use: the dark-theme file and the light-theme
// file, each shown only in its own theme by TokensCSS. It is decorative beside
// the product name, so it is hidden from assistive technology.
var Mark = `<span class="tv-mark" aria-hidden="true">` +
	`<span class="tv-mark-dark">` + inline(markFlatDark) + `</span>` +
	`<span class="tv-mark-light">` + inline(markFlatLight) + `</span>` +
	`</span>`

// inline drops the namespace declaration, which HTML supplies for an inline
// <svg> anyway, so a page carrying the mark contains no http:// URL at all. The
// drawing is untouched.
func inline(svg string) string {
	return strings.Replace(strings.TrimSpace(svg), ` xmlns="http://www.w3.org/2000/svg"`, "", 1)
}

// TokensCSS is brand-v4/tokens/tokens.css with the theme switch driven by the
// viewer's preference: dark is primary, light under prefers-color-scheme.
const TokensCSS = `:root{
  --tv-canvas:#060A12; --tv-surface:#081220; --tv-raised:#0E1A2A;
  --tv-line:rgba(126,147,171,.18); --tv-line-strong:rgba(126,147,171,.32);
  --tv-ink:#E6EEF7; --tv-ink-muted:#7E93AB; --tv-label:#90ACC0;
  --tv-accent:#3AD6C0; --tv-accent-hover:#55DFCC; --tv-on-accent:#060A12; --tv-accent-tint:#0E2B2E;
  --tv-positive:#45C77F; --tv-caution:#D6A93C; --tv-critical:#F0685C;
  --tv-font-sans:"Space Grotesk",system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  --tv-font-mono:"IBM Plex Mono",ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  --tv-radius-sm:6px; --tv-radius-md:10px; --tv-radius-pill:999px;
  color-scheme:dark;
}
@media (prefers-color-scheme: light){
` + lightTokens + `}
.tv-mark{display:inline-flex; flex:none}
.tv-mark-dark,.tv-mark-light{display:block; width:100%; height:100%}
.tv-mark-light{display:none}
.tv-mark svg{display:block; width:100%; height:100%}
@media (prefers-color-scheme: light){
  .tv-mark-dark{display:none} .tv-mark-light{display:block}
}
.tv-rule{display:block; width:32px; height:2px; background:var(--tv-accent); border:0; margin:0}
`

// PrintCSS puts a page in the light theme on paper whatever the screen's
// preference: browsers drop backgrounds when printing, so the dark theme would
// print Frost text on white. Add it after TokensCSS on a page meant to be
// printed.
const PrintCSS = `@media print{
` + lightTokens + `  .tv-mark-dark{display:none} .tv-mark-light{display:block}
}
`

// lightTokens is the light column of brand-v4/tokens/tokens.css.
const lightTokens = `  :root{
    --tv-canvas:#F4F7FB; --tv-surface:#FFFFFF; --tv-raised:#FFFFFF;
    --tv-line:rgba(6,10,18,.10); --tv-line-strong:rgba(6,10,18,.20);
    --tv-ink:#060A12; --tv-ink-muted:#46586E; --tv-label:#646B79;
    --tv-accent:#0F7A6C; --tv-accent-hover:#0A5F54; --tv-on-accent:#FFFFFF; --tv-accent-tint:#C9E6E0;
    --tv-positive:#1D7A45; --tv-caution:#8A6A12; --tv-critical:#BF3B2E;
    color-scheme:light;
  }
`
