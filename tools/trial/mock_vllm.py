#!/usr/bin/env python3
"""Deterministic OpenAI-compatible mock upstream for the routing-pattern earn trial.

Lens' vLLM provider POSTs to  LENS_VLLM_BASE_URL + "/v1/chat/completions".
This server answers that route with a byte-stable JSON body so that the earn
path's request_id = SHA256(SHA256(model)+SHA256(prompt)+SHA256(response)) is
STABLE across replays (scenario c). The body echoes the request's `model` (so
model_used == the model the traffic script sends, whichever side Lens reads it
from) but is otherwise constant and independent of the prompt — different
prompts therefore yield different rids (prompt is in the hash) while identical
(model,prompt) replays yield identical rids.

Stdlib only; threaded; sub-millisecond -> pins latency_bucket=fast (<1000ms).
"""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8000


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):  # keep the container log quiet
        pass

    def do_GET(self):  # health probe convenience
        body = b"ok"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}
        model = req.get("model", "trial-mock")
        resp = {
            "id": "chatcmpl-trial",
            "object": "chat.completion",
            "created": 0,
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": "Trial mock completion. Deterministic body for the routing-pattern earn trial.",
                    },
                    "finish_reason": "stop",
                }
            ],
            "usage": {"prompt_tokens": 12, "completion_tokens": 14, "total_tokens": 26},
        }
        # sort_keys + fixed separators => byte-stable for a given model.
        body = json.dumps(resp, separators=(",", ":"), sort_keys=True).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
