package povi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ChallengeClient is the production PathProvider: it signs a challenge with
// Lens's ed25519 key and POSTs it to the node's /challenge endpoint, which
// verifies the signature (so only the Lens key-holder can extract trace leaves)
// and answers from its retained trace. A transport error / non-200 / timeout
// returns an error, which the Challenger treats as a failed (timed-out)
// challenge — a node that can't or won't answer is treated as cheating.
type ChallengeClient struct {
	http     *http.Client
	lensPriv ed25519.PrivateKey
	now      func() time.Time
}

// NewChallengeClient builds a client with the given timeout (the timeout is the
// "node didn't answer" boundary → failed challenge → slash).
func NewChallengeClient(lensPriv ed25519.PrivateKey, timeout time.Duration) *ChallengeClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &ChallengeClient{
		http:     &http.Client{Timeout: timeout},
		lensPriv: lensPriv,
		now:      time.Now,
	}
}

// FetchPaths implements PathProvider.
func (c *ChallengeClient) FetchPaths(ctx context.Context, _, nodeURL, requestID string, positions []int) ([]LeafProof, error) {
	if nodeURL == "" {
		return nil, fmt.Errorf("povi: node has no URL")
	}
	nonce := c.now().UnixNano()
	signed := SignChallenge(c.lensPriv, requestID, positions, nonce)
	body, err := json.Marshal(signed)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trimSlash(nodeURL)+"/challenge", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err // unreachable / timeout → Challenger treats as timeout
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("povi: node challenge status %d: %s", resp.StatusCode, string(raw))
	}
	var answers []LeafProof
	if err := json.Unmarshal(raw, &answers); err != nil {
		return nil, fmt.Errorf("povi: decode challenge answer: %w", err)
	}
	return answers, nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
