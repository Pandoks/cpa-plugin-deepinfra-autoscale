# cpa-plugin-deepinfra-autoscale

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that serves your **own
Hugging Face weights** on [DeepInfra](https://deepinfra.com) dedicated deployments and autoscales
them, so you pay for GPUs only while a model is actually in use. Models are added, removed, and
managed from the CLIProxyAPI dashboard.

## What it does

- **Serve** a managed model through CLIProxyAPI as an ordinary model id (e.g. `red-snow`),
  forwarding chat to the DeepInfra deployment behind it. OpenAI (`/v1/chat/completions`) and
  Anthropic (`/v1/messages`) clients are both supported, including streaming (SSE).
- **Provision on demand**: the first request to a model creates (or restarts) its DeepInfra
  deployment and returns a clear `503 warming up` during the ~12-minute cold start. Subsequent
  requests are served normally.
- **Scale to zero when idle**: a background sweep stops a deployment (`POST /deploy/{id}/stop`,
  which halts GPU billing while keeping the downloaded weights for a fast restart) after it has
  been idle for the configured window, with a hard max-uptime backstop.
- **Add / remove models on the fly** from the dashboard, from a Hugging Face repo + GPU + context.

Because the plugin is in the request path, **activity is tracked automatically** — no client hooks,
no external state store, no separate worker. Model definitions are persisted so they survive a
CLIProxyAPI restart (which also re-adopts any still-running deployment so it is never left billing).

## Install from the plugin panel (recommended)

Everything is done in the CLIProxyAPI management panel; no file edits.

1. **Config Panel → Advanced & Experimental → Plugins → Third-party Plugin Sources**: add
   `https://raw.githubusercontent.com/Pandoks/cpa-plugin-deepinfra-autoscale/main/registry.json`
   and save.
2. **Plugin Store**: search `deepinfra`, click **Install**, and confirm. It downloads the `.so` from
   the latest GitHub Release and loads it without a restart.
3. **PLUGINS → DeepInfra Autoscale** in the sidebar: enter the management key once (the page
   remembers it), set the DeepInfra API key and idle window under **Settings**, and add models.

## Install manually

Drop the built library at `<plugins.dir>/linux/amd64/deepinfra-autoscale.so` and enable it in
`config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "~/.cli-proxy-api/plugins"
  configs:
    deepinfra-autoscale:
      enabled: true
      priority: 1
      api-key: "<deepinfra api key>"   # or set it in the dashboard under Settings
      idle-minutes: 30                 # scale to zero after this many idle minutes (default 30)
      max-uptime-minutes: 720          # hard backstop (default 720 = 12h)
      state-file: "~/.cli-proxy-api/deepinfra-autoscale-state.json"
```

`make build` produces `dist/deepinfra-autoscale.$(EXT)`.

## Dashboard

- UI (no auth): `GET /v0/resource/plugins/deepinfra-autoscale/dashboard`
- JSON API (management-authenticated, `Authorization: Bearer <management secret-key>`):
  - `GET /v0/management/deepinfra-autoscale/models` — list models, GPU catalog + prices, idle window
  - `POST .../models` — add `{id, hf_repo, revision?, gpu, context, quantization?, tool_parser?, reasoning_parser?, enable_thinking?}`
  - `DELETE .../models?id=<id>` — remove and tear down
  - `POST .../models/wake?id=<id>` / `.../models/sleep?id=<id>` — manual start / scale-to-zero

Adding a model does **not** provision GPUs; it becomes routable immediately and spins up on first
use (or an explicit Wake). The GPU field is the DeepInfra config string, e.g. `2xH200-141GB`.

## Using it from Claude Code

With `ANTHROPIC_BASE_URL` pointing at CLIProxyAPI and `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`,
every model you add shows up in `/model` under its **Name in Claude Code** after the next Claude
Code restart, and disappears when you remove it.

CLIProxyAPI lists non-Claude models to Claude clients as `claude-fable-5-dd-<reversed id>` and
decodes that back when routing, so Claude Code treats every managed model as Fable 5, including a
1M context window. That matches 1M-context models; for a smaller model Claude Code will not compact
before the model's real limit.

The first request to a scaled-to-zero model starts its deployment and returns `503 warming up`
(Claude Code retries) for the ~12-minute cold start.

## Cost safety

- Idle scale-to-zero, a max-uptime backstop, orphan re-adoption after a restart, teardown on
  remove by the recorded deploy id, and a stop-all on clean shutdown all guard against GPUs being
  left billing.
- **Residual risk:** the idle sweep runs inside CLIProxyAPI, so a hard crash that does not restart
  leaves a running deployment billing until CLIProxyAPI returns (a `systemd Restart=always` unit
  re-adopts and bounds it). Set a spending limit in DeepInfra's billing settings as a backstop.

## License

MIT
