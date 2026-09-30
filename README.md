<h1 align="center">
    <img src="img/logo.png" width="220" />
    <div>
    OpenRouter
    <br>
    Bot
    </div>
</h1>

<h4 align="center">
    <strong>English (🇺🇸)</strong> | <a href="README_RU.md">Русский (🇷🇺)</a>
</h4>

This project allows you to launch your Telegram bot in a few minutes to communicate with free and paid AI models via [OpenRouter](https://openrouter.ai), or local LLMs, for example, via [LM Studio](https://lmstudio.ai).

> [!NOTE]
> This repository is a fork of the [openrouter-gpt-telegram-bot](https://github.com/deinfinite/openrouter-gpt-telegram-bot) project, which adds new features (such as switch current model, group translation, Gemini fallback and `Markdown` formatting in bot responses) and optimizes the container startup process.

<details>
    <summary>Example</summary>
    <img src="./img/example.png">
    <img src="./img/commands.png">
</details>

---

## What's in this fork

| Feature | Notes |
|---|---|
| Streaming responses | Tokens are edited into the message as they arrive |
| Gemini fallback | Switches provider on rate limits or 5xx, at start **and** mid-stream |
| `/tr` and `/translate` | Translate a single message, or auto-translate a whole group |
| Vision | Send a photo and ask about it |
| Per-user budgets | Spend caps per day / month / all time |
| Rate limiting | Requests per user per minute |
| Long answers | Automatically split across several Telegram messages |

---

## Preparation

- Register with [OpenRouter](https://openrouter.ai) and get an [API key](https://openrouter.ai/settings/keys).
- Create your Telegram bot using [@BotFather](https://telegram.me/BotFather) and get its API token.
- Get your Telegram id using [@getmyid_bot](https://t.me/getmyid_bot).

> [!TIP]
> When you launch the bot, you will be able to see the IDs of other users in the log, to whom you can also grant access to the bot in the future.

> [!IMPORTANT]
> **Never commit your `.env`.** It is git-ignored; `.env.example` is the documented template.

---

## Quick start (Docker)

```bash
git clone https://github.com/clientmusic33-hue/openrouter-bot.git
cd openrouter-bot

cp .env.example .env
$EDITOR .env            # add TELEGRAM_BOT_TOKEN and API_KEY at minimum

docker compose up -d
docker compose logs -f
```

The container exposes a health check on port `10000` (`/healthz`).

### Running a pre-built binary

```bash
CGO_ENABLED=0 go build -o openrouter-bot .
TELEGRAM_BOT_TOKEN=... API_KEY=... ./openrouter-bot
```

The binary reads `./config.yaml` and `./lang/` from the working directory, so run it from the repository root (or ship those two next to the binary).

---

## Commands

| Command | Description |
|---|---|
| `/start` | Welcome message and help |
| `/help` | Show the command list |
| `/get_models` | List models that are free for prompt **and** completion |
| `/set_model <name>` | Change the model |
| `/set_model default` | Restore the configured model |
| `/reset` | Clear the conversation history |
| `/reset <prompt>` | Clear history and set a new system prompt |
| `/reset system` | Clear history and restore the default system prompt |
| `/stats` | Usage statistics (respects `STATS_MIN_ROLE`) |
| `/stop` | Stop the request currently streaming for you |
| `/tr [lang]` | Translate the message you replied to (defaults to English) |
| `/translate on\|off\|<lang>\|status` | Automatic group translation (groups only) |
| `/about` | About this bot |

---

## Group behaviour

Two independent behaviours, so the bot never answers a single message twice:

- **`/translate on`** — the bot translates every message and does **not** run a chat completion.
- **Chat mode** (`GROUP_CHAT_MODE`) — controls whether the bot *answers* in groups:
  - `mention` (**default**) — only when you @-mention it or reply to one of its messages
  - `all` — answer every group message
  - `off` — never answer in groups

Private chats always get a reply.

---

## Configuration

Every value can be set as a real environment variable **or** in `.env`. `config.yaml` supplies defaults and is hot-reloaded on change.

### Required

| Variable | Description |
|---|---|
| `TELEGRAM_BOT_TOKEN` | Bot token from @BotFather |
| `API_KEY` | OpenRouter API key |

### Access control

| Variable | Default | Description |
|---|---|---|
| `ADMIN_IDS` | – | Comma separated Telegram ids. Admins bypass the budget and always see stats |
| `ALLOWED_USER_IDS` | – | Comma separated ids. **Empty means everyone is treated as a USER** |
| `USER_BUDGET` | `1` | USD allowance per period for admins and allowed users |
| `GUEST_BUDGET` | `0` | USD allowance per period for everybody else. `0` blocks guests |
| `BUDGET_PERIOD` | `monthly` | `daily`, `monthly` or `total` |
| `STATS_MIN_ROLE` | `ADMIN` | `ADMIN`, `USER` or `GUEST` |
| `RATE_LIMIT_PER_MINUTE` | `10` | Max requests per user per minute. `0` disables it |
| `MAX_CONCURRENT_REQUESTS` | `8` | Max AI requests in flight at once |

> [!WARNING]
> Set `GUEST_BUDGET=0` (the default) unless you intend to run a public bot. With a non-zero guest budget, anyone who finds your bot can spend against your OpenRouter key.

### Model and responses

| Variable | Default | Description |
|---|---|---|
| `BASE_URL` | `https://openrouter.ai/api/v1` | Any OpenAI-compatible endpoint |
| `MODEL` | `deepseek/deepseek-r1:free` | Model id |
| `TYPE` | `openrouter` | Set to something else to disable generation cost accounting |
| `MAX_TOKENS` | `2000` | Max completion tokens |
| `TEMPERATURE` / `TOP_P` | `0.7` | Sampling |
| `FREQUENCY_PENALTY` / `PRESENCE_PENALTY` | `0` | Sampling |
| `ASSISTANT_PROMPT` | – | Prepended to the system prompt |
| `REQUEST_TIMEOUT_SECONDS` | `300` | Total budget for one request, streaming included |
| `STREAM_IDLE_TIMEOUT_SECONDS` | `120` | Abort a stream that delivers nothing for this long |

### Optional

| Variable | Default | Description |
|---|---|---|
| `GEMINI_API_KEY` | – | Enables the Gemini fallback when OpenRouter fails |
| `VISION` | `false` | Analyse photos. Uses `VISION_PROMPT` and `VISION_DETAIL` |
| `MAX_HISTORY_SIZE` / `MAX_HISTORY_TIME` | `10` / `60` | Conversation memory (messages / minutes) |
| `LANG` | `EN` | `EN` or `RU` |
| `GROUP_CHAT_MODE` | `mention` | `mention`, `all` or `off` |
| `PORT` | `10000` | Health server port |
| `ENV_FILE` | `.env` | Path to the dotenv file |

---

## Kubernetes

The Helm chart injects configuration as environment variables, so no `.env` file is mounted.

```bash
helm install my-bot ./chart \
  --set secrets.API_KEY="$OPENROUTER_KEY" \
  --set secrets.TELEGRAM_BOT_TOKEN="$TELEGRAM_TOKEN" \
  --set config.ALLOWED_USER_IDS="$MY_TELEGRAM_ID" \
  --set persistence.enabled=true
```

Secrets live in a `Secret`, not a `ConfigMap`. Enable `persistence` to keep group translation settings across restarts.

> [!NOTE]
> The chart sets `GROUP_CHAT_MODE=mention` by default. Change it to `all` only if you want the bot to answer every message in every group it joins.

---

## Development

```bash
go build ./...          # compile
go vet ./...            # static checks
gofmt -l .              # formatting (should print nothing)
go test -race ./...     # tests, with the race detector
```

CI (`.github/workflows/ci.yml`) runs formatting, `go vet`, `go mod tidy` verification, the build and the race-enabled test suite on every push and pull request.

### Layout

```
main.go              update loop, commands, group behaviour
api/                 OpenRouter + Gemini streaming, Telegram rendering
config/              configuration loading and hot reload
lang/                translation bundles (EN.json, RU.json)
user/                spend tracking, budgets, rate limits, history
grouptranslate/      per-group auto-translation settings
translator/          one-shot translation helper
internal/atomicfile/ crash-safe file writes
```

---

## Notes on behaviour

- **Long answers** are split into several messages of at most 3800 characters, preferring paragraph boundaries.
- **Formatting**: streaming uses plain text (a half-written `**bold` would be rejected by Telegram). The final message retries with `MarkdownV2`, then legacy `Markdown`, then plain text.
- **Costs** come from the OpenRouter generation endpoint after the stream ends. Statistics can take a moment to finalise, so the lookup retries once.
- **`/stop`** cancels the request and keeps whatever was generated so far.

## License

[MIT](LICENSE)
