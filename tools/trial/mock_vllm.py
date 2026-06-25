#!/usr/bin/env python3
"""Deterministic OpenAI-compatible mock upstream for the trial harness.

Two routes:

  POST /v1/chat/completions  — byte-stable JSON body so the earn path's
      request_id = SHA256(SHA256(model)+SHA256(prompt)+SHA256(response)) is
      STABLE across replays (pattern-earn scenario c). The body echoes the
      request's `model` but is otherwise constant and independent of the prompt.

  POST /v1/embeddings  — 1536-dim vectors (matches prompt_embeddings vector(1536))
      with an ENGINEERED, DETERMINISTIC collision so the #142 semantic-isolation
      proof can run offline (added for PR 2a; reached via LENS_EMBEDDING_BASE_URL):

        * Any input containing the marker token "TLVCOLLIDE" -> the FIXED unit
          vector e0 = [1, 0, 0, ...].  cosine(any two marker prompts) = 1.0,
          which is >= the 0.92 SemanticThreshold -> a forced semantic match.
          So in the isolation scenarios the EMBEDDING always collides; the only
          thing that can stop a cross-tenant hit is the workspace_id filter
          (#142) — which is exactly what we want to test.

        * Any non-marker input -> a deterministic (sha256-seeded) pseudo-random
          unit vector whose axis-0 component is pinned to 0.  Therefore:
            - cosine(non-marker, marker) = v[0] = 0  EXACTLY (disjoint support on
              axis 0) -> 0 < 0.92, so a non-marker prompt can NEVER accidentally
              match a marker row. This is a hard guarantee, not statistical.
            - cosine(non-marker_i, non-marker_j) for distinct prompts is the dot
              of two pseudo-random unit vectors in 1535 dims -> ~0; exceeding
              0.92 has probability < 1e-200 (concentration of measure). So the
              non-marker prompts the pattern trial uses stay mutually distinct
              (no spurious semantic hits — see the PR's neutrality note).

Stdlib only; threaded; sub-millisecond -> pins latency_bucket=fast (<1000ms).
"""
import hashlib
import json
import math
import struct
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8000
DIM = 1536
MARKER = "TLVCOLLIDE"
# Models advertised by GET /v1/models (OpenAI shape) — the PoVI node's vllm provider
# Health/ListModels reads this to come up in the closed test.
MODELS = ["trial-mock"]


def _embed(text):
    """Deterministic 1536-dim vector — see module docstring for the guarantees."""
    if MARKER in text:
        v = [0.0] * DIM
        v[0] = 1.0  # the fixed marker axis; cosine 1.0 between any two marker prompts
        return v
    # Non-marker: axis 0 pinned to 0 (=> exact cosine 0 vs the marker), axes
    # 1..DIM-1 sha256-seeded and unit-normalised.
    seed = hashlib.sha256(text.encode()).digest()
    vals, counter = [], 0
    while len(vals) < DIM - 1:
        block = hashlib.sha256(seed + counter.to_bytes(4, "big")).digest()
        for i in range(0, len(block), 4):
            u = struct.unpack(">I", block[i : i + 4])[0]
            vals.append((u / 2**31) - 1.0)  # [-1, 1)
            if len(vals) >= DIM - 1:
                break
        counter += 1
    norm = math.sqrt(sum(x * x for x in vals)) or 1.0
    return [0.0] + [x / norm for x in vals]


def _embeddings_response(req):
    inp = req.get("input", "")
    texts = inp if isinstance(inp, list) else [inp]
    return {
        "object": "list",
        "data": [
            {"object": "embedding", "index": i, "embedding": _embed(t)}
            for i, t in enumerate(texts)
        ],
        "model": req.get("model", "trial-embed"),
        "usage": {"prompt_tokens": 1, "total_tokens": 1},
    }


def _chat_response(req):
    model = req.get("model", "trial-mock")
    return {
        "id": "chatcmpl-trial",
        "object": "chat.completion",
        "created": 0,
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": "Trial mock completion. Deterministic body for the trial harness.",
                },
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 12, "completion_tokens": 14, "total_tokens": 26},
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):  # keep the container log quiet
        pass

    def do_GET(self):
        # /v1/models must return OpenAI-shape JSON: the PoVI node's vllm provider Health probes it
        # and ListModels/heartbeat JSON-decode {"data":[{"id":...}]}. A plain "ok" passed Health
        # but broke ListModels — the closed-test node never came up.
        if self.path.rstrip("/").endswith("/v1/models"):
            body = json.dumps(
                {"object": "list", "data": [{"id": m, "object": "model"} for m in MODELS]},
                separators=(",", ":"), sort_keys=True,
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        # Other GETs keep the cheap text/plain 200 (health convenience).
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
        if self.path.endswith("/embeddings"):
            resp = _embeddings_response(req)
        else:
            resp = _chat_response(req)
        # sort_keys + fixed separators => byte-stable for a given input.
        body = json.dumps(resp, separators=(",", ":"), sort_keys=True).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
