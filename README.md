# cpa-plugin-deepinfra-autoscale

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that serves your **own
Hugging Face weights** on [DeepInfra](https://deepinfra.com) dedicated deployments and autoscales
them, so you pay for GPUs only while a model is in use.

## What it does

- **Serve** a managed model through CLIProxyAPI as a normal model id (e.g. `red-snow`), forwarding
  chat to the DeepInfra deployment behind it.
- **Provision on demand**: the first request to an unprovisioned/scaled-to-zero model creates or
  wakes its DeepInfra deployment, then fails fast with a "warming up, retry" message during the
  ~12-minute cold start.
- **Scale to zero** when a model has been idle, so GPUs stop billing between bursts.
- **Add / remove models on the fly** via the Management API, from a Hugging Face repo + GPU/context.

Managed models are persisted so they survive a CLIProxyAPI restart.

## Status

Early scaffold. Design and build in progress.

## License

MIT
