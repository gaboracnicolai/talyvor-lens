//go:build bench

// bench_setters.go exposes URL setters used only by the bench suite.
// Production builds (compiled without -tags=bench) don't include this
// file, so the public API surface stays unchanged off the hot path.
package proxy

func (p *Proxy) SetOpenAIURL(url string)    { p.openAIURL = url }
func (p *Proxy) SetAnthropicURL(url string) { p.anthropicURL = url }
func (p *Proxy) SetGoogleURL(url string)    { p.googleURL = url }
