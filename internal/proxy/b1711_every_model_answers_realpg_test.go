package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
)

// B17.11 — EVERY OPENAI MODEL IN THE CHAT'S PICKER ANSWERS, AT ITS CATALOG PRICE.
//
// The production e2e run of 2026-09-30 asked each picker model "Reply with the single word: ok" with
// the chat's own body ({model, max_tokens: 4096, stream: true, messages}). Eleven OpenAI models failed
// two ways, and the upstream below refuses exactly as OpenAI did there: the GPT-5.x models and
// chat-latest 400 on max_tokens, and GPT-5.3 Codex, GPT-5.4 Pro and GPT-5.5 Pro 404 on
// /v1/chat/completions because OpenAI serves them only on /v1/responses. Every OpenAI model the picker
// offers (priced for output, not deprecated — apps/web/src/areas/chat/chatApi.ts) is asked through the
// real handler and must answer, and be charged on the prepaid ledger at its catalog price.

// b1711ChatOnlyRefused is the production report's list of models /v1/chat/completions 404'd.
var b1711ChatOnlyRefused = map[string]bool{"gpt-5.3-codex": true, "gpt-5.4-pro": true, "gpt-5.5-pro": true}

type b1711Upstream struct {
	mu       sync.Mutex
	path     string
	lastBody map[string]any
}

// serve answers as OpenAI does: 10,000 input and 100 output tokens, streamed or buffered.
func (u *b1711Upstream) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.path, u.lastBody = r.URL.Path, body
		u.mu.Unlock()
		model, _ := body["model"].(string)
		stream, _ := body["stream"].(bool)
		answer := "Hello from " + model
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if b1711ChatOnlyRefused[model] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"This model is not supported in the v1/chat/completions endpoint. Use the v1/responses endpoint instead."}}`)
				return
			}
			if _, sent := body["max_tokens"]; sent && !strings.HasPrefix(model, "gpt-4") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+answer+"\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\ndata: [DONE]\n\n")
		case strings.HasSuffix(r.URL.Path, "/responses"):
			for _, chatOnly := range []string{"messages", "max_tokens", "max_completion_tokens", "stream_options"} {
				if _, sent := body[chatOnly]; sent {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"error":{"message":"Unknown parameter: '%s'."}}`, chatOnly)
					return
				}
			}
			usage := `{"input_tokens":10000,"input_tokens_details":{"cached_tokens":0},"output_tokens":100,"output_tokens_details":{"reasoning_tokens":60},"total_tokens":10100}`
			done := `{"id":"resp_1","object":"response","created_at":1790000000,"status":"completed","model":"` + model + `",` +
				`"output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + answer + `","annotations":[]}]}],` +
				`"usage":` + usage + `}`
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, done)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n"+
					"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello from \"}\n\n"+
					"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\""+model+"\"}\n\n"+
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+done+"}\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func b1711PickerModels(t *testing.T) []catalog.Model {
	t.Helper()
	var out []catalog.Model
	for _, m := range catalog.ByProvider("openai") {
		if m.OutputPer1M > 0 && !m.Deprecated {
			out = append(out, m)
		}
	}
	if len(out) < 15 {
		t.Fatalf("the catalog offers %d OpenAI chat models; the production run asked 17 or more", len(out))
	}
	return out
}

func TestB1711_EveryOpenAIModelInThePickerAnswersTheChatAtItsCatalogPrice(t *testing.T) {
	for _, m := range b1711PickerModels(t) {
		t.Run(m.ID, func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			up := &b1711Upstream{}
			p.openAIURL = up.serve(t).URL + "/v1/chat/completions"

			q := jsonString("b1711 " + m.ID + " " + strings.Repeat("x", 400))
			res := askNewestModel(t, p, false, true, `{"model":"`+m.ID+`","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":`+q+`}]}`)
			got, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK || b1711StreamText(string(got)) != "Hello from "+m.ID {
				t.Fatalf("status=%d text=%q body=%.500q — the model did not answer", res.StatusCode, b1711StreamText(string(got)), got)
			}
			if in, out, ok := b187UsageFrame(string(got)); !ok || in != 10000 || out != 100 {
				t.Errorf("usage frame before [DONE]: found=%v %d/%d, want 10000/100 — %.500q", ok, in, out, got)
			}
			want := b1711CatalogULXC(m)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (10k in + 100 out at the catalog's $%.2f/$%.2f per 1M)",
					rows, debited, want, m.InputPer1M, m.OutputPer1M)
			}
		})
	}
}

// The buffered seam — the other copy of the upstream call — asks a Responses-only model too, and the
// client, the cache and the charge all read an ordinary chat completion.
func TestB1711_AResponsesOnlyModelAnswersABufferedChatAtItsCatalogPrice(t *testing.T) {
	m, ok := catalog.Get("gpt-5.4-pro")
	if !ok {
		t.Fatal("gpt-5.4-pro is not in the catalog")
	}
	p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
	up := &b1711Upstream{}
	p.openAIURL = up.serve(t).URL + "/v1/chat/completions"

	q := jsonString("b1711-buffered " + strings.Repeat("y", 400))
	res := askNewestModel(t, p, false, false, `{"model":"gpt-5.4-pro","max_tokens":4096,"temperature":0.2,"messages":[{"role":"user","content":`+q+`}]}`)
	raw, _ := io.ReadAll(res.Body)
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &chat); err != nil || res.StatusCode != http.StatusOK || len(chat.Choices) != 1 ||
		chat.Choices[0].Message.Content != "Hello from gpt-5.4-pro" {
		t.Fatalf("status=%d body=%.500q — not a chat completion carrying the answer", res.StatusCode, raw)
	}
	if chat.Usage.PromptTokens != 10000 || chat.Usage.CompletionTokens != 100 {
		t.Errorf("usage = %+v, want 10000/100", chat.Usage)
	}
	up.mu.Lock()
	if !strings.HasSuffix(up.path, "/v1/responses") || up.lastBody["max_output_tokens"] != float64(4096) {
		t.Errorf("upstream asked %s with %v; want /v1/responses carrying max_output_tokens 4096", up.path, up.lastBody)
	}
	up.mu.Unlock()
	want := b1711CatalogULXC(m)
	if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
		t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC", rows, debited, want)
	}
}

// b1711CatalogULXC is the charge for 10,000 in + 100 out at m's catalog price. The dollar figure is
// rounded to 1e-12 first, so float noise (100 × 0.4 = 40.00000000000001) cannot tip Ceil up a µLXC.
func b1711CatalogULXC(m catalog.Model) int64 {
	return settleULXC(math.Round((10000*m.InputPer1M+100*m.OutputPer1M)*1e6) / 1e12)
}

// b1711StreamText is the answer an OpenAI-shaped client stream spells out.
func b1711StreamText(stream string) string {
	var sb strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &c) == nil {
			for _, ch := range c.Choices {
				sb.WriteString(ch.Delta.Content)
			}
		}
	}
	return sb.String()
}
