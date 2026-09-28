package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B18.7 — EVERY PROVIDER'S MODELS STREAM THROUGH THEIR OWN UPSTREAM, AT THEIR OWN PRICE.
//
// The streaming seam was "OpenAI, else Anthropic": a streamed request for any other provider's model
// was sent to Anthropic's URL with Anthropic's key. Here six upstreams run at once, each speaking its
// provider's real streaming format — OpenAI, Mistral and Groq SSE chunks, Anthropic events, Gemini's
// streamGenerateContent SSE, Bedrock's binary event stream — and each case sends ONE streamed chat
// question through the real handler, with the chat's credential, to one provider. It must reach that
// provider and no other, with that provider's credential, come back as a readable stream, and be
// charged on the prepaid ledger at that model's published price (10,000 in + 100 out).

type b187Upstream struct {
	mu    sync.Mutex
	calls int
	path  string
	query string
	auth  string // the provider credential the request carried
}

func (u *b187Upstream) record(r *http.Request, cred string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.path, u.query, u.auth = r.URL.Path, r.URL.RawQuery, cred
}

// eventStreamFrame encodes one AWS event-stream message with string headers.
func eventStreamFrame(headers map[string]string, payload []byte) []byte {
	var h bytes.Buffer
	for _, k := range []string{":event-type", ":content-type", ":message-type"} {
		v, ok := headers[k]
		if !ok {
			continue
		}
		h.WriteByte(byte(len(k)))
		h.WriteString(k)
		h.WriteByte(7)
		_ = binary.Write(&h, binary.BigEndian, uint16(len(v)))
		h.WriteString(v)
	}
	total := uint32(12 + h.Len() + len(payload) + 4)
	var m bytes.Buffer
	_ = binary.Write(&m, binary.BigEndian, total)
	_ = binary.Write(&m, binary.BigEndian, uint32(h.Len()))
	_ = binary.Write(&m, binary.BigEndian, uint32(0)) // prelude CRC — not checked by the reader
	m.Write(h.Bytes())
	m.Write(payload)
	_ = binary.Write(&m, binary.BigEndian, uint32(0))
	return m.Bytes()
}

func bedrockChunk(event string) []byte {
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	return eventStreamFrame(map[string]string{":event-type": "chunk", ":content-type": "application/json", ":message-type": "event"}, payload)
}

func TestB187_EveryProviderStreamsThroughItsOwnUpstreamAtItsOwnPrice(t *testing.T) {
	cases := []struct {
		provider, model   string
		inPer1M, outPer1M float64 // internal/catalog/seed.go
	}{
		{"anthropic", "claude-haiku-4-5", 1.00, 5.00},
		{"openai", "gpt-6-luna", 0.10, 0.50},
		{"google", "gemini-2.5-flash", 0.30, 2.50},
		{"mistral", "mistral-small-latest", 0.10, 0.30},
		{"groq", "llama-3.3-70b-versatile", 0.59, 0.79},
		{"bedrock", "claude-sonnet-4-6", 3.00, 15.00},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			answer := "Hello from " + tc.model
			ups := map[string]*b187Upstream{}
			serveOpenAIShape := func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+answer+"\"}}]}\n\n"+
					"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\ndata: [DONE]\n\n")
			}
			for _, name := range []string{"anthropic", "openai", "google", "mistral", "groq", "bedrock"} {
				u := &b187Upstream{}
				ups[name] = u
				name := name
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch name {
					case "anthropic":
						u.record(r, r.Header.Get("x-api-key"))
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, chatSSEWithUsage(answer, 10000, 100))
					case "google":
						u.record(r, r.URL.Query().Get("key"))
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"`+answer+`"}],"role":"model"}}]}`+"\n\n"+
							`data: {"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10000,"candidatesTokenCount":100}}`+"\n\n")
					case "bedrock":
						u.record(r, strings.SplitN(r.Header.Get("Authorization"), " ", 2)[0])
						w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
						for _, ev := range []string{
							`{"type":"message_start","message":{"usage":{"input_tokens":10000,"output_tokens":1}}}`,
							`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + answer + `"}}`,
							`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}`,
							`{"type":"message_stop"}`,
						} {
							_, _ = w.Write(bedrockChunk(ev))
						}
					default: // openai, mistral, groq: OpenAI-compatible
						u.record(r, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
						serveOpenAIShape(w)
					}
				}))
				t.Cleanup(srv.Close)
				switch name {
				case "anthropic":
					p.anthropicURL = srv.URL
				case "openai":
					p.openAIURL = srv.URL
				case "google":
					p.googleURL = srv.URL
				case "mistral":
					p.mistralURL = srv.URL
				case "groq":
					p.groqURL = srv.URL
				case "bedrock":
					p.bedrockURL = srv.URL
				}
			}
			p.googleKey, p.mistralKey, p.groqKey = "google-key", "mistral-key", "groq-key"
			p.bedrockConfig = BedrockConfig{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", Region: "us-east-1"}

			q := jsonString("b187 " + tc.model + " " + strings.Repeat("x", 400))
			body := `{"model":"` + tc.model + `","stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			if tc.provider == "anthropic" {
				body = `{"model":"` + tc.model + `","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":` + q + `}]}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/"+tc.provider+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "ws-log")
			req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
			w := newFlushRecorder()
			switch tc.provider {
			case "anthropic":
				p.HandleAnthropic(w, req)
			case "openai":
				p.HandleOpenAI(w, req)
			case "google":
				p.HandleGoogle(w, req)
			case "bedrock":
				p.HandleBedrock(w, req)
			default:
				p.HandleExtraProvider(tc.provider)(w, req)
			}
			got := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(got, answer) {
				t.Fatalf("status=%d body=%.400q — the model did not answer as a stream", w.Code, got)
			}
			if tc.provider != "anthropic" && !strings.Contains(got, "data: [DONE]") {
				t.Errorf("the client stream does not end the OpenAI way: %.400q", got)
			}
			// B18.59: every OpenAI-shaped stream — Google's and Bedrock's translated ones too — ends with a
			// usage frame carrying the tokens it is billed on, before [DONE], so the chat can price it.
			if tc.provider != "anthropic" {
				if in, out, ok := b187UsageFrame(got); !ok || in != 10000 || out != 100 {
					t.Errorf("usage frame before [DONE]: found=%v prompt_tokens=%d completion_tokens=%d, want 10000/100 — %.600q",
						ok, in, out, got)
				}
			}

			// It reached its own provider, with its own credential — and no other provider at all.
			for name, u := range ups {
				want := 0
				if name == tc.provider {
					want = 1
				}
				if u.calls != want {
					t.Errorf("%s upstream received %d request(s), want %d", name, u.calls, want)
				}
			}
			own := ups[tc.provider]
			wantCred := map[string]string{"anthropic": "anthropic-key", "openai": "openai-key", "google": "google-key",
				"mistral": "mistral-key", "groq": "groq-key", "bedrock": "AWS4-HMAC-SHA256"}[tc.provider]
			if own.auth != wantCred {
				t.Errorf("the %s request carried credential %q, want %q", tc.provider, own.auth, wantCred)
			}
			switch tc.provider {
			case "google":
				if !strings.HasSuffix(own.path, "/"+tc.model+":streamGenerateContent") || !strings.Contains(own.query, "alt=sse") {
					t.Errorf("google was asked %s?%s, want the model's streamGenerateContent with alt=sse", own.path, own.query)
				}
			case "bedrock":
				if !strings.HasSuffix(own.path, "/invoke-with-response-stream") {
					t.Errorf("bedrock was asked %s, want invoke-with-response-stream", own.path)
				}
			}

			want := settleULXC(10000*tc.inPer1M/1e6 + 100*tc.outPer1M/1e6)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (10k in + 100 out at $%.2f/$%.2f per 1M)",
					rows, debited, want, tc.inPer1M, tc.outPer1M)
			}
		})
	}
}

// b187UsageFrame finds the usage-only chunk of an OpenAI-shaped client stream, before its [DONE].
func b187UsageFrame(stream string) (prompt, completion int, ok bool) {
	for _, line := range strings.Split(stream, "\n") {
		data, isData := strings.CutPrefix(line, "data: ")
		if !isData {
			continue
		}
		if data == "[DONE]" {
			return prompt, completion, ok
		}
		var c struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &c) == nil && c.Usage != nil {
			prompt, completion, ok = c.Usage.PromptTokens, c.Usage.CompletionTokens, true
		}
	}
	return 0, 0, false
}
