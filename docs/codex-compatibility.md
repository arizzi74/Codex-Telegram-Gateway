# Codex App Server compatibility

`internal/codexadapter` was verified against Codex CLI `0.154.0` on
2026-09-13. The version-specific JSON Schema bundle was generated with:

```text
codex app-server generate-json-schema --out ./schemas
```

The protocol reference is [Codex App Server](https://developers.openai.com/codex/app-server).

The worker's normal runtime starts `codex app-server --listen stdio://` and
uses one local JSONL reader and writer. Each connection performs `initialize`
followed by `initialized` before every other request.

The terminal CLI cannot attach to a child whose only listener is `stdio://`:
there is one private byte stream and no attachable endpoint. Codex `0.154.0`
supports local CLI attachment through a Unix control socket instead. A managed
daemon can expose that socket to `codex --remote unix://PATH`; a worker that
needs the same daemon can keep its adapter boundary by supervising
`codex app-server proxy --sock PATH` as its JSONL stdio peer. This is an
explicit alternate runtime mode, not a second writer on the direct stdio child.
