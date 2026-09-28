# Laya sidecar for smart routing

[Laya](https://huggingface.co/convaiinnovations/laya) is an open-source (Apache 2.0)
"System 1" decision model. It generates no text: it reads a request and answers a
typed question, here "which tier does this need?", in roughly 30–400 ms. It runs
next to BabelGate, so prompts never leave the machine for this step.

BabelGate itself stays pure Go. It talks to Laya's `laya-serve` HTTP server
(`POST /v1/systemone`) and falls back to its built-in heuristic whenever Laya is
unreachable, slow, or not confident enough.

## Start Laya

Docker (CPU):

```sh
docker compose -f deploy/laya/compose.yaml up -d --build
curl http://localhost:8000/health
```

Native (uses Apple MPS or CUDA when available; needs Python ≥ 3.10):

```sh
deploy/laya/run.sh
```

The first start downloads the checkpoint from Hugging Face (about 1.7 GB for
`english`, about 1.3 GB for `multilingual`). Choose the checkpoint with
`LAYA_MODELS`; `multilingual` is the better choice for non-English prompts.

## Point BabelGate at it

```yaml
smart:
  classifier:
    mode: laya
    url: http://localhost:8000
    # api_key: ${LAYA_API_KEY}   # only if the server sets LAYA_API_KEY
    # model: multilingual        # pin a checkpoint; empty lets Laya choose
    timeout_ms: 1000
    min_confidence: 0.6          # below this, the heuristic decides
```

Every smart decision logs its source in the request trace, for example
`smart complex: laya english 0.87 in 45ms`, or `heuristic score ...` when the
fallback decided. Only the latest user instruction (first 1500 and last 500
characters) plus whether tools and thinking are present is sent to Laya.
Tool-result follow-ups reuse the turn's tier without asking Laya again.
