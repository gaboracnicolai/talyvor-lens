package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
)

// B17.104 — EVERY OPENAI MODEL IN THE PICKER ANSWERS CHAT, WHICH OFFERS IT TOOLS.
//
// The e2e run of 2026-10-09 asked each picker model "Reply with the single word: …" with Chat's own body,
// which now offers OpenAI models function tools (Lens's wallet tools, Track, Docs). Nine models refused:
// GPT-5.3 Codex, GPT-5.4 Pro and GPT-5.5 Pro because Lens sent the tool-carrying body to /v1/responses
// untranslated (400 "Unsupported parameter: 'messages'"), and the six GPT-5.6 and GPT-6 models because
// /v1/chat/completions refuses them function tools while they reason. The upstream below refuses exactly
// those ways, and the Responses API's own: a chat-shaped tool, and a tool left strict (its default) whose
// schema is open.

const b17104Tools = `[{"type":"function","function":{"name":"agent_spend","description":"What an agent spent","parameters":{"type":"object","properties":{"agent":{"type":"string"}}}}}]`

type b17104Upstream struct {
	mu    sync.Mutex
	paths []string
	input []any // the last /v1/responses body's input items
	tools []any // and its tools
	call  bool  // answer a question with no tool result yet with a call to agent_spend
}

func (u *b17104Upstream) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		model, _ := body["model"].(string)
		stream, _ := body["stream"].(bool)
		refuse := func(msg string) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error"}}`, msg)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if b1711ChatOnlyRefused[model] {
				refuse("This model is only supported in v1/responses and not in v1/chat/completions.")
				return
			}
			if _, tools := body["tools"]; tools && (strings.HasPrefix(model, "gpt-5.6") || strings.HasPrefix(model, "gpt-6")) {
				refuse("Function tools with reasoning_effort are not supported for " + model + " in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello from "+model+"\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":100}}\n\ndata: [DONE]\n\n")
		case strings.HasSuffix(r.URL.Path, "/responses"):
			for _, chatOnly := range []string{"messages", "max_tokens", "max_completion_tokens", "stream_options"} {
				if _, sent := body[chatOnly]; sent {
					refuse("Unsupported parameter: '" + chatOnly + "'. In the Responses API, this parameter has moved.")
					return
				}
			}
			tools, _ := body["tools"].([]any)
			for _, tl := range tools {
				tool, _ := tl.(map[string]any)
				if _, nested := tool["function"]; nested || tool["name"] == nil {
					refuse("Missing required parameter: 'tools[0].name'.")
					return
				}
				if strict, set := tool["strict"].(bool); !set || strict {
					refuse("Invalid schema for function 'agent_spend': 'additionalProperties' is required to be supplied and to be false.")
					return
				}
			}
			input, _ := body["input"].([]any)
			u.mu.Lock()
			u.input, u.tools = input, tools
			u.mu.Unlock()
			answered := ""
			for _, it := range input {
				if item, _ := it.(map[string]any); item["type"] == "function_call_output" {
					answered, _ = item["output"].(string)
				}
			}
			out := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello from ` + model + `","annotations":[]}]}`
			if answered != "" {
				out = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The tool said ` + answered + `","annotations":[]}]}`
			}
			calling := u.call && answered == ""
			if calling {
				out = `{"type":"function_call","id":"fc_1","call_id":"call_b17104","name":"agent_spend","arguments":"{\"agent\":\"a1\"}","status":"completed"}`
			}
			done := `{"id":"resp_1","object":"response","created_at":1790000000,"status":"completed","model":"` + model + `",` +
				`"output":[{"type":"reasoning","summary":[]},` + out + `],` +
				`"usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":0},"output_tokens":100,"output_tokens_details":{"reasoning_tokens":60},"total_tokens":10100}}`
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, done)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			events := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n"
			if calling {
				// output_index 1: the reasoning item is 0, as OpenAI numbers them.
				events += "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_b17104\",\"name\":\"agent_spend\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n" +
					"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":1,\"delta\":\"{\\\"agent\\\":\"}\n\n" +
					"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":1,\"delta\":\"\\\"a1\\\"}\"}\n\n"
			} else {
				var text struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				}
				_ = json.Unmarshal([]byte(out), &text)
				events += "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":" + jsonString(text.Content[0].Text) + "}\n\n"
			}
			_, _ = io.WriteString(w, events+"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+done+"}\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestB17104_EveryOpenAIModelInThePickerAnswersChatsToolBodyAtItsCatalogPrice(t *testing.T) {
	for _, m := range b1711PickerModels(t) {
		t.Run(m.ID, func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			up := &b17104Upstream{}
			p.openAIURL = up.serve(t).URL + "/v1/chat/completions"

			q := jsonString("Reply with the single word: b17104 " + m.ID + " " + strings.Repeat("x", 400))
			res := askNewestModel(t, p, false, true, `{"model":"`+m.ID+`","stream":true,"messages":[{"role":"user","content":`+q+`}],"tools":`+b17104Tools+`}`)
			got, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK || b1711StreamText(string(got)) != "Hello from "+m.ID {
				t.Fatalf("status=%d text=%q body=%.500q — the model did not answer", res.StatusCode, b1711StreamText(string(got)), got)
			}
			want := b1711CatalogULXC(m)
			if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC (10k in + 100 out at the catalog's $%.2f/$%.2f per 1M)",
					rows, debited, want, m.InputPer1M, m.OutputPer1M)
			}
		})
	}
}

// A Responses model's tool call reaches Chat as an OpenAI chat stream spells one, and the tool's answer
// goes back to the model on the next request: both requests charged at the catalog price.
func TestB17104_AProModelsToolCallStreamsToChatAndItsAnswerGoesBack(t *testing.T) {
	m, ok := catalog.Get("gpt-5.5-pro")
	if !ok {
		t.Fatal("gpt-5.5-pro is not in the catalog")
	}
	p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
	up := &b17104Upstream{call: true}
	p.openAIURL = up.serve(t).URL + "/v1/chat/completions"
	q := jsonString("What did agent a1 spend? b17104 " + strings.Repeat("z", 400))

	res := askNewestModel(t, p, false, true, `{"model":"gpt-5.5-pro","stream":true,"messages":[{"role":"user","content":`+q+`}],"tools":`+b17104Tools+`}`)
	got, _ := io.ReadAll(res.Body)
	id, name, args, finish := b17104StreamToolCall(string(got))
	if res.StatusCode != http.StatusOK || id != "call_b17104" || name != "agent_spend" || args != `{"agent":"a1"}` || finish != "tool_calls" {
		t.Fatalf("status=%d tool call = %q %q %q finish %q; want call_b17104 agent_spend {\"agent\":\"a1\"} tool_calls — %.800q",
			res.StatusCode, id, name, args, finish, got)
	}

	res = askNewestModel(t, p, false, true, `{"model":"gpt-5.5-pro","stream":true,"messages":[{"role":"user","content":`+q+`},`+
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_b17104","type":"function","function":{"name":"agent_spend","arguments":"{\"agent\":\"a1\"}"}}]},`+
		`{"role":"tool","tool_call_id":"call_b17104","content":"3 LXC"}],"tools":`+b17104Tools+`}`)
	got, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || b1711StreamText(string(got)) != "The tool said 3 LXC" {
		t.Fatalf("status=%d text=%q — the tool's answer did not reach the model: %.500q", res.StatusCode, b1711StreamText(string(got)), got)
	}
	up.mu.Lock()
	sent, _ := json.Marshal(up.input)
	up.mu.Unlock()
	for _, item := range []string{
		`{"arguments":"{\"agent\":\"a1\"}","call_id":"call_b17104","name":"agent_spend","type":"function_call"}`,
		`{"call_id":"call_b17104","output":"3 LXC","type":"function_call_output"}`,
	} {
		if !strings.Contains(string(sent), item) {
			t.Errorf("the second request's input = %s; want it to carry %s", sent, item)
		}
	}
	want := b1711CatalogULXC(m)
	if rows, debited, _ := prepaidDebits(t, pool); rows != 2 || debited != 2*want {
		t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 2 rows of %d µLXC", rows, debited, want)
	}
}

// The buffered seam: a GPT-6 model offered tools is asked on /v1/responses, and its call comes back as a
// chat completion's tool_calls.
func TestB17104_ABufferedGPT6ToolCallComesBackAsAChatCompletion(t *testing.T) {
	m, ok := catalog.Get("gpt-6-sol")
	if !ok {
		t.Fatal("gpt-6-sol is not in the catalog")
	}
	p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
	up := &b17104Upstream{call: true}
	p.openAIURL = up.serve(t).URL + "/v1/chat/completions"

	q := jsonString("What did agent a1 spend? b17104-buffered " + strings.Repeat("w", 400))
	res := askNewestModel(t, p, false, false, `{"model":"gpt-6-sol","messages":[{"role":"user","content":`+q+`}],"tools":`+b17104Tools+`,"tool_choice":{"type":"function","function":{"name":"agent_spend"}}}`)
	raw, _ := io.ReadAll(res.Body)
	var chat struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chat); err != nil || res.StatusCode != http.StatusOK || len(chat.Choices) != 1 ||
		len(chat.Choices[0].Message.ToolCalls) != 1 || chat.Choices[0].FinishReason != "tool_calls" || chat.Choices[0].Message.Content != nil {
		t.Fatalf("status=%d body=%.600q — not a chat completion carrying one tool call", res.StatusCode, raw)
	}
	if c := chat.Choices[0].Message.ToolCalls[0]; c.ID != "call_b17104" || c.Function.Name != "agent_spend" || c.Function.Arguments != `{"agent":"a1"}` {
		t.Errorf("tool call = %+v", c)
	}
	up.mu.Lock()
	paths := strings.Join(up.paths, " ")
	up.mu.Unlock()
	if paths != "/v1/responses" {
		t.Errorf("upstream asked %s; want /v1/responses alone", paths)
	}
	want := b1711CatalogULXC(m)
	if rows, debited, _ := prepaidDebits(t, pool); rows != 1 || debited != want {
		t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 1 row of %d µLXC", rows, debited, want)
	}
}

// b17104StreamToolCall is the one tool call an OpenAI-shaped client stream spells out, and its finish_reason.
func b17104StreamToolCall(stream string) (id, name, args, finish string) {
	for _, line := range strings.Split(stream, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				id += tc.ID
				name += tc.Function.Name
				args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	return id, name, args, finish
}
