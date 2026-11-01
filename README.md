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

1. Add this repo's registry to `plugins.store-sources` in `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     store-sources:
       - https://raw.githubusercontent.com/Pandoks/cpa-plugin-deepinfra-autoscale/main/registry.json
   ```

2. In the management panel, open **Plugin Store**, search `deepinfra`, click **Install**, and confirm.
   It downloads the `.so` from the latest GitHub Release and loads it without a restart.
3. Open **Plugins → DeepInfra Autoscale → Edit config**, paste your DeepInfra API key into `api-key`,
   and save. Changes apply live.
4. Open **DeepInfra Autoscale** under **PLUGINS** in the sidebar, enter the management key once (the
   page remembers it), and add models.

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

With `ANTHROPIC_BASE_URL` pointing at CLIProxyAPI and `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`,
every model you add shows up in `/model` as "From gateway" after the next Claude Code restart.

CLIProxyAPI's `/v1/models` carries no context window, so to give a model its full window add a
labeled row to `modelPicker` in your user settings; the `[1m]` suffix tells Claude Code the model
has a 1M window and the plugin strips it when routing:

```json
"modelPicker": {
  "options": [
    { "model": "red-snow[1m]", "label": "RED-SNOW 5.3 Flash (1M)", "description": "DeepInfra · scales to zero when idle" }
  ]
}
```

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
