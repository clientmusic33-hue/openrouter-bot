Helm chart for installing the [OpenRouter Bot](https://github.com/Lifailon/openrouter-bot) in Kubernetes cluster.

```bash
helm repo add openrouter-bot https://lifailon.github.io/openrouter-bot

helm upgrade --install openrouter-bot openrouter-bot/openrouter-bot \
    --set secrets.API_KEY="sk-or-v1-XXX" \
    --set secrets.TELEGRAM_BOT_TOKEN="7777777777:XXX" \
    --set secrets.GROQ_API_KEY="gsk_XXX" \
    --set secrets.GEMINI_API_KEY="AIzaXXX" \
    --set config.ADMIN_IDS="7777777777" \
    --set config.ALLOWED_USER_IDS="7777777777\,8888888888" \
    --set config.MODEL=deepseek/deepseek-r1:free \
    --set config.PUBLIC_MODE="true" \
    --set config.GROUP_ACCESS=everyone \
    --set config.VISION=false \
    --set config.MAX_HISTORY_SIZE=20 \
    --set config.LANG=RU
```

> **One replica only.** The bot reads updates with long polling, and Telegram
> delivers each update to a single poller, so a second replica would answer
> only part of the traffic. `replicaCount` is 1 for that reason.

With `persistence.enabled=true` the claim is mounted at `data/`, and
`LOGS_DIR` points per-user preferences at the same volume, so pins,
favourites and spend survive a restart.

Secrets are kept out of the ConfigMap:

- `secrets.TELEGRAM_BOT_TOKEN` and `secrets.API_KEY` are required.
- Every other `secrets.*` key is rendered into the Secret as-is, so
  `secrets.GROQ_API_KEY`, `secrets.GEMINI_API_KEY`, `secrets.CEREBRAS_API_KEY`
  and friends join the failover chain without editing the chart. Empty values
  are skipped, and a provider without a key never receives a request.
- Everything under `config.*` becomes a plain environment variable; the full
  list of supported names lives in the [main README](../README.md).

The deployment runs with a non-root user, a read-only root filesystem, probes
on `/healthz` and no secrets on disk: configuration arrives purely through the
environment, so no `.env` file has to be mounted.

Provider wiring (which provider uses which key, the model lists and the
failover order) lives in `config.yaml` inside the image. A name alone is
enough there — the preset fills in the endpoint and the key variable:

```yaml
providers:
  - name: openrouter
  - name: groq
  - name: gemini
```
