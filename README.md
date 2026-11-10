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
  been idle for the configured window. A model in use stays up; a deployment stuck starting for
  over 2 hours is deleted.
- **Fall back across GPUs**: each model has a list of GPU configs. Provisioning checks DeepInfra's
  live availability and creates on the cheapest free one; if that create is rejected, or the
  deployment fails to start (e.g. `no-gpu-available`), it moves to the next, skipping a GPU that
  failed for 10 minutes. When none is left, requests get `503 no GPU available` and the dashboard
  shows why.
- **Add, edit and remove models on the fly** from the dashboard: search a Hugging Face repo and it
  fills in the context, parsers and GPUs that fit, from the repo's metadata.

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
3. **PLUGINS → DeepInfra Autoscale** in the sidebar: paste your DeepInfra API key under
   the gear (Settings) and add models. The page uses your panel login if you ticked **Remember password**;
   otherwise it asks for the management key once.

The API key is stored in `deepinfra-autoscale.key` (mode 0600) next to the state file, not in
`config.yaml`, because CLIProxyAPI shows plugin config fields in plain text.

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
      idle-minutes: 30                 # scale to zero after this many idle minutes (default 30)
      state-file: "~/.cli-proxy-api/deepinfra-autoscale-state.json"
```

`make build` produces `dist/deepinfra-autoscale.$(EXT)`.

## Dashboard

- UI (no auth): `GET /v0/resource/plugins/deepinfra-autoscale/dashboard`
- JSON API (management-authenticated, `Authorization: Bearer <management secret-key>`):
  - `GET /v0/management/deepinfra-autoscale/models` — list models, GPU catalog + prices, idle window
  - `POST .../models` — add `{id, hf_repo, revision?, gpus, context, quantization?, tool_parser?, reasoning_parser?, enable_thinking?, generation_config?, speculative_config?, loop_guard?}`
  - `PUT .../models` — edit a model; same body as add, `id` picks the model
  - `DELETE .../models?id=<id>` — remove and tear down
  - `POST .../models/wake?id=<id>` / `.../models/sleep?id=<id>` — manual start / scale-to-zero
  - `PUT /v0/management/deepinfra-autoscale/api-key` — set the DeepInfra API key `{api_key}`

`generation_config` sets the deployment's default sampling through vLLM's `--override-generation-config`,
e.g. `{"presence_penalty":1.5}`. Claude Code sends no sampling fields, so this is how you tune it for
Claude Code. It applies when a deployment is created, so editing it deletes the model's deployment.

`speculative_config` is passed to vLLM's `--speculative-config`, also at deployment creation. A draft
model runs on the same GPUs and vLLM downloads it from Hugging Face itself, e.g.
`{"method":"dflash","model":"Blackfrost-AI/RED-SNOW-5.3-FLASH-DFlash2-STAGE2","num_speculative_tokens":8}`.

`loop_guard` watches a model's streamed responses for degeneration: the same 128 characters recurring
6 times, or, in thinking, 1500 bytes with no sentence break or newline (the endless word lists a
presence penalty produces). With `on`, a loop in the answer ends the response there; a loop in thinking
is cut, and the request is sent again with the output so far and a note asking the model to finish, at
most twice and never after a tool call. `observe` only logs loops; `off` is the default.

Adding a model does **not** provision GPUs; it becomes routable immediately and spins up on first
use (or an explicit Wake). `gpus` are DeepInfra config strings, e.g. `["1xB300-270GB","2xH200-141GB"]`;
order doesn't matter, since the cheapest free one is tried first. A stopped deployment is restarted only
when its GPU is still the cheapest free one; otherwise it is replaced on the cheaper GPU.

Editing the name in Claude Code, the loop guard or the GPU list keeps the deployment (a new GPU list
applies at the next wake, unless the deployment's GPU was removed). Editing anything else deletes the
deployment, and the next request provisions one with the new settings.

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

- Idle scale-to-zero, deleting deployments stuck starting, orphan re-adoption after a restart, teardown on
  remove by the recorded deploy id, and a stop-all on clean shutdown all guard against GPUs being
  left billing.
- **Residual risk:** the idle sweep runs inside CLIProxyAPI, so a hard crash that does not restart
  leaves a running deployment billing until CLIProxyAPI returns (a `systemd Restart=always` unit
  re-adopts and bounds it). Set a spending limit in DeepInfra's billing settings as a backstop.

## License

MIT
