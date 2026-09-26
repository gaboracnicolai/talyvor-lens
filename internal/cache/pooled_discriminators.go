package cache

import (
	"strings"

	"github.com/talyvor/lens/internal/discriminator"
)

// pooledDiscriminators is the discriminators column a pooled write stores, computed on the BARE
// prompt — the text GetPooled canonicalises — never on the marker-prefixed key it is handed (B9.5).
// On the key, the sentence-start rule no longer sees the first word at the start, so a capitalised
// one became a proper noun on write only: "Can I enable SSO for Okta?" stored
// caps:sso|propn:can|propn:okta against a read of caps:sso|propn:okta, and was never matched.
func pooledDiscriminators(prompt string) string {
	return string(discriminator.Canon(strings.TrimPrefix(prompt, PoolKeyMarker)))
}
