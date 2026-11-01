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

Add this repo's registry to `plugins.store-sources`, then install "DeepInfra Autoscale" from the
CLIProxyAPI management panel (it downloads the `.so` from the GitHub Release):

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/Pandoks/cpa-plugin-deepinfra-autoscale/main/registry.json
  configs:
    deepinfra-autoscale:
      enabled: true
      priority: 1
      api-key: "<deepinfra api key>"
```

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
      api-key: "<deepinfra api key>"   # also settable from the dashboard config fields
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

With gateway model discovery on (`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`), models you add in
the dashboard appear in Claude Code's **model picker** — just select one, no `--model` flag or hooks
needed. When you add a model the dashboard triggers a CLIProxyAPI reload so it shows in `/v1/models`;
restart Claude Code once for it to re-discover the list. (Routing works immediately regardless of
the listing.)

CLIProxyAPI's `/v1/models` is minimal (id + owner only), so the picker doesn't convey the context
window. For the full 1M window, select the model and set the window explicitly — either the `[1m]`
variant or the env var:

```sh
# main chat 1M context, subagents 256k, both on the same deployment
CLAUDE_CODE_SUBAGENT_MODEL=red-snow \
CLAUDE_CODE_MAX_CONTEXT_TOKENS=262144 \
  claude --model 'red-snow[1m]'
```

The `[1m]` suffix tells Claude Code the model has a 1M window; the plugin normalizes it when routing.

## Cost safety

- Idle scale-to-zero, a max-uptime backstop, orphan re-adoption after a restart, teardown on
  remove by the recorded deploy id, and a stop-all on clean shutdown all guard against GPUs being
  left billing.
- **Residual risk:** the idle sweep runs inside CLIProxyAPI, so a hard crash that does not restart
  leaves a running deployment billing until CLIProxyAPI returns (a `systemd Restart=always` unit
  re-adopts and bounds it). Set a spending limit in DeepInfra's billing settings as a backstop.

## License

MIT
