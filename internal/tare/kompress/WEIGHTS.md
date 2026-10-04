# kompress-small — the weights Tare phase 2a runs

| | |
|---|---|
| Model | [chopratejas/kompress-small](https://huggingface.co/chopratejas/kompress-small) — a 6-layer ModernBERT token classifier distilled from kompress-base, built on [answerdotai/ModernBERT-base](https://huggingface.co/answerdotai/ModernBERT-base) |
| Licence | Apache-2.0 (the model card's `license:`; ModernBERT-base is Apache-2.0 too). Full text: `LICENSE-kompress-small`, shipped beside the weights in the image |
| Revision | `eacbdc589d039a1a39f76a92844979ad4e266bcd` |
| `model.safetensors` | 279,041,436 bytes, sha256 `e9080094129618e082326677adbe0d34871e5ddbec5f30f71c016d74abedcf1e` |
| `tokenizer.json` | 3,583,228 bytes, sha256 `6c8aaa9a542084f2457eab775d4eeb51f92a70c0fd9de28d5edb0ddec3c08d30` |

The weights are not in git (279 MB). The Dockerfile's `kompress` stage and CI's "Tare phase 2a model"
step fetch both files at the revision above and refuse any other bytes. They land at
`/models/kompress-small` in the image (`LENS_TARE_MODEL_DIR`).

The model card publishes `model.onnx`; this package runs the same network in pure Go from
`model.safetensors` because the lens binary is CGO_ENABLED=0 on alpine and onnxruntime is a C++
library. `testdata/golden.json` holds logits onnxruntime produced from that `model.onnx`, and
`golden_test.go` asserts this port reproduces them (max |Δlogit| ≈ 7e-6).
