package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
)

// sdk_base_url_test.go — B18.15: the two SDKs agree on the base URL.
//
// sdk/typescript always used {lensUrl}/v1/proxy/openai/v1; sdk/python used {lensUrl}/v1/proxy/openai,
// so the OpenAI library under each one POSTed a different path for the same call. Both SDKs now use
// /v1. The released Python SDK still sends the old path, so the server must keep answering it.
//
// The SDK subtests run the REAL SDKs (python + the openai package, node + the compiled TypeScript
// SDK) against the real HandleOpenAI mounted on the production route pattern. They need both
// toolchains, so they run where LENS_SDK_E2E=1 — the `sdk` CI job — and skip elsewhere; with the
// variable set, a missing toolchain is a failure, not a skip.

const (
	sdkNewPath = "/v1/proxy/openai/v1/chat/completions"
	sdkOldPath = "/v1/proxy/openai/chat/completions"
)

// sdkLens serves HandleOpenAI on the route cmd/lens/main.go registers (`/v1/proxy/openai/*`), in
// front of an upstream that answers every chat completion with "pong". It records each inbound
// path, so a test can prove WHICH path a client used, not just that it got an answer.
func sdkLens(t *testing.T) (url string, inbound func() []string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-sdk","object":"chat.completion","created":1,"model":"gpt-4o-mini",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(upstream.Close)

	p := New(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "test-key", "", "", nil)
	p.openAIURL = upstream.URL + "/v1/chat/completions"

	var mu sync.Mutex
	var paths []string
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			paths = append(paths, req.URL.Path)
			mu.Unlock()
			next.ServeHTTP(w, req)
		})
	})
	r.Post("/v1/proxy/openai/*", p.HandleOpenAI)
	lens := httptest.NewServer(r)
	t.Cleanup(lens.Close)

	return lens.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestSDKBaseURL_BothPathsAnswerAndBothSDKsUseTheNewOne(t *testing.T) {
	url, inbound := sdkLens(t)

	t.Run("server answers the old and the new path", func(t *testing.T) {
		for _, path := range []string{sdkNewPath, sdkOldPath} {
			resp, err := http.Post(url+path, "application/json",
				strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}`))
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			var out struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 || out.Choices[0].Message.Content != "pong" {
				t.Errorf("%s: want the upstream's completion, got %d: %s", path, resp.StatusCode, body)
			}
		}
	})

	// Each script makes one call with the SDK's own client (the new path) and one with the openai
	// library pointed at the old base URL and carrying the SDK's headers — what a released Python
	// SDK sends. It prints both answers as a JSON array.
	sdks := []struct {
		name, dir string
		run       func(t *testing.T, dir string) *exec.Cmd
	}{
		{"python", "../../sdk/python", func(t *testing.T, dir string) *exec.Cmd {
			return exec.Command(requireTool(t, "python3"), "-c", `
import json, os
from openai import OpenAI
from talyvor_lens import LensClient
c = LensClient(lens_url=os.environ["LENS_URL"], api_key="tlv_sdk_test")
msgs = [{"role": "user", "content": "ping"}]
new = c.openai.chat.completions.create(model="gpt-4o-mini", messages=msgs)
old = OpenAI(base_url=os.environ["LENS_URL"] + "/v1/proxy/openai", api_key="tlv_sdk_test",
             default_headers=c.get_headers()).chat.completions.create(model="gpt-4o-mini", messages=msgs)
print(json.dumps([new.choices[0].message.content, old.choices[0].message.content]))
`)
		}},
		{"typescript", "../../sdk/typescript", func(t *testing.T, dir string) *exec.Cmd {
			// Compile the SDK out of tree so the test never writes dist/ into the checkout.
			out := t.TempDir()
			tsc := exec.Command(requireTool(t, "npx"), "tsc", "--outDir", out)
			tsc.Dir = dir
			if b, err := tsc.CombinedOutput(); err != nil {
				t.Fatalf("compiling the TypeScript SDK: %v\n%s", err, b)
			}
			cmd := exec.Command(requireTool(t, "node"), "-e", `
const { LensClient } = require(process.env.SDK_DIST);
const { OpenAI } = require("openai");
(async () => {
  const c = new LensClient({ lensUrl: process.env.LENS_URL, apiKey: "tlv_sdk_test" });
  const body = { model: "gpt-4o-mini", messages: [{ role: "user", content: "ping" }] };
  const fresh = await c.openai().chat.completions.create(body);
  const old = await new OpenAI({ baseURL: process.env.LENS_URL + "/v1/proxy/openai", apiKey: "tlv_sdk_test",
    defaultHeaders: c.getHeaders() }).chat.completions.create(body);
  console.log(JSON.stringify([fresh.choices[0].message.content, old.choices[0].message.content]));
})().catch((e) => { console.error(e); process.exit(1); });
`)
			abs, _ := filepath.Abs(filepath.Join(dir, "node_modules"))
			cmd.Env = append(cmd.Env, "SDK_DIST="+out, "NODE_PATH="+abs)
			return cmd
		}},
	}
	for _, sdk := range sdks {
		t.Run(sdk.name+" SDK on the new and the old path", func(t *testing.T) {
			if os.Getenv("LENS_SDK_E2E") == "" {
				t.Skip("needs the SDK toolchains; set LENS_SDK_E2E=1 (the sdk CI job does)")
			}
			before := len(inbound())
			cmd := sdk.run(t, sdk.dir)
			cmd.Dir = sdk.dir
			cmd.Env = append(append(os.Environ(), cmd.Env...), "LENS_URL="+url)
			stdout, err := cmd.Output()
			if err != nil {
				var stderr []byte
				if ee, ok := err.(*exec.ExitError); ok {
					stderr = ee.Stderr
				}
				t.Fatalf("%s SDK script failed: %v\n%s", sdk.name, err, stderr)
			}
			var answers []string
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(stdout))), &answers); err != nil {
				t.Fatalf("%s SDK printed %q, want a JSON array of two answers", sdk.name, stdout)
			}
			if len(answers) != 2 || answers[0] != "pong" || answers[1] != "pong" {
				t.Errorf("%s SDK answers = %q, want the upstream's completion on both paths", sdk.name, answers)
			}
			got := inbound()[before:]
			if want := []string{sdkNewPath, sdkOldPath}; strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("%s SDK reached Lens on %q, want %q — the SDK's own client must use the /v1 path", sdk.name, got, want)
			}
		})
	}
}

func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("LENS_SDK_E2E is set but %s is not on PATH", name)
	}
	return p
}
