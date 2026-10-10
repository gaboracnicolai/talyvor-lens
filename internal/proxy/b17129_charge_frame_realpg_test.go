package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// B17.129 — the figure under a Chat answer is what Lens charged for it. Asked with X-Talyvor-Report-Charge,
// the stream carries a talyvor.charge frame before its terminator (Chat's reader stops at the terminator),
// and the frame's charged_ulxc is the amount of the spend row Lens wrote for the answer. Unasked, the
// stream is exactly the provider's.
func TestChargeFrame_AChatStreamSaysWhatItsSpendRowCharged(t *testing.T) {
	p, pool, _ := chatCacheProxy(t, chatSSE("Pacific", true))
	w := chatAsk(t, p, "wsPoolA", reportChargeHeader, "true", "X-Talyvor-Cache", "bypass")
	body := w.Body.String()

	var row int64
	if err := pool.QueryRow(context.Background(), `SELECT -amount FROM lxc_ledger
		WHERE workspace_id = 'wsPoolA' AND amount < 0 AND description = 'chat: metered usage'`).Scan(&row); err != nil {
		t.Fatalf("want exactly one 'chat: metered usage' spend row: %v", err)
	}
	const opens = "event: " + chargeFrame + "\ndata: "
	at := strings.Index(body, opens)
	stop := strings.Index(body, "event: message_stop")
	if at < 0 || stop < at {
		t.Fatalf("want the charge frame before message_stop; the stream:\n%s", body)
	}
	var frame struct {
		Type    string `json:"type"`
		Charged int64  `json:"charged_ulxc"`
	}
	line := body[at+len(opens):]
	if err := json.Unmarshal([]byte(line[:strings.Index(line, "\n")]), &frame); err != nil || frame.Type != chargeFrame {
		t.Fatalf("charge frame %q: %v", line, err)
	}
	if frame.Charged != row || row <= 0 {
		t.Errorf("the frame says %d µLXC charged, the answer's spend row %d µLXC", frame.Charged, row)
	}

	if unasked := chatAsk(t, p, "wsPoolA", "X-Talyvor-Cache", "bypass").Body.String(); strings.Contains(unasked, chargeFrame) {
		t.Errorf("a stream not asked to report its charge carries the frame:\n%s", unasked)
	}
}
