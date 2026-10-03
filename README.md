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
> This repository is a fork of the [openrouter-gpt-telegram-bot](https://github.com/deinfinite/openrouter-gpt-telegram-bot) project. It grew into a button-first assistant: pick a provider and a model from inline menus, let the bot route automatically when it knows your usage, and let the failover chain answer from the next backend when one is down.

<details>
    <summary>Example</summary>
    <img src="./img/example.png">
    <img src="./img/commands.png">
</details>

---

## What's in this fork

| Feature | Notes |
|---|---|
| Streaming responses | Tokens are edited into the message as they arrive, with a 🛑 Stop button |
| Provider chain with failover | OpenRouter, Groq, Gemini, Cerebras, NVIDIA, Mistral, DeepSeek, Together, Ollama — EWMA latency tracking, circuit breakers, exponential backoff, and mid-stream failover |
| Provider → model picker | Inline buttons with live status (`🟢 available`, `🟡 degraded`, `🔴 unavailable`) and capability badges (`⚡ fast`, `🧠 reasoning`, `👁 vision`, `💻 coding`) |
| Smart task routing | Automatically classifies prompts (`CHAT`, `CODING`, `REASONING`, `VISION`, `TRANSLATION`, `SUMMARIZATION`, `RESEARCH`, `FAST`, `LONG_CONTEXT`) and selects the optimal healthy model |
| `/fast` & `/race` modes | Low-latency single-model routing (`/fast`) or concurrent 2–3 model racing (`/race`, `/race judge`) cancelling slower candidates on first completion |
| Multi-step `/agent` | Bounded tool-using agent (`calculator`, `time`, `web`, `url`, `github`, `translator`, `notes`) with loop prevention and strict step/time limits |
| `/research` web search | SSRF-protected web search and page extraction with source deduplication and numbered citations |
| File intelligence | Upload TXT, MD, PDF, DOCX, CSV, JSON, YAML, or source code files for summarization, Q&A, CSV stats, and code review |
| Coding assistant | `/code`, `/review`, `/explain`, `/testgen` plus automatic language and stack-trace detection |
| Memory & token optimizer | Bounded short-term context with automatic summarization plus long-term user fact memory (`/memory`) |
| Personas | Built-in (`Developer`, `Teacher`, `Researcher`, `Writer`, `Translator`, `Coding Agent`, `Business`) and custom personas (`/persona`) |
| Reminders, tasks & notes | Persistent `/remind`, `/reminders`, `/task`, `/tasks`, `/note`, `/notes` surviving restarts |
| Group AI & `/summarize` | Per-group access mode, auto-translation, and `/summarize` with action-item extraction |
| Voice & multimodal | Voice message transcription (Whisper STT) and photo/image-document vision analysis |
| Storage & optional Redis | Crash-safe atomic JSON storage by default (`STORAGE_TYPE=json`) with optional PostgreSQL (`STORAGE_TYPE=postgres`) and Redis caching/rate-limiting (`REDIS_URL`) |

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

## Usage: buttons first

Almost nothing needs to be typed. `/start` (or `/menu`) opens a panel, and the
persistent reply keyboard keeps the four entry points one tap away:

- 🧠 **Model** — pick a provider from the list, then a model inside it. The
  first row is always `✨ Auto — best for my usage`.
- ⚙️ **Settings** — auto model on/off, the model footer, live typing, favourites.
- 📊 **Usage** — your requests and (when allowed) your spend.
- 🔮 **Best for me** — scores every available model against your usage and
  offers a one-tap switch.

Every answer carries its own controls: 🛑 **Stop** while it streams, then
🔁 **Regenerate**, 🧠 **Model**, 👍 / 👎 and ⭐ to bookmark the model that
answered. A group panel (`/group`) gives admins the same treatment for the
chat: who may use the bot, and auto-translation.

## Commands

Commands still work for power users; nothing is hidden behind them.

| Command | Description |
|---|---|
| `/start`, `/menu` | Main panel with buttons |
| `/help` | How to use the bot |
| `/model` | Open the model picker |
| `/model <n\|name>` | Pin a model by catalogue number or id |
| `/model default`, `/auto` | Back to automatic model choice |
| `/addprovider` | Store your own provider and API key (encrypted), then pick one of its models |
| `/myproviders` | List the providers you added |
| `/useprovider <n\|name>` | Switch to one of your own providers |
| `/removeprovider <n\|name>` | Delete one of your own providers |
| `/provider` | Open the provider list |
| `/provider <n\|name>` | Provider view: model health counts, refresh and search |
| `/models` | Live catalogue: 🆓 free & working first, then every provider, with filters |
| `/models <query>` | Search by name, id, provider or capability (e.g. `/models qwen`) |
| `/models <provider>` | That provider's models |
| `/refresh_models` | Re-read every provider's model list and rebuild the picker |
| `/recommend` | Best model for your usage, with a one-tap switch |
| `/settings` | Your preferences as buttons |
| `/stats`, `/usage` | Your usage statistics (respects `STATS_MIN_ROLE`) |
| `/reset` | Clear the conversation history |
| `/reset <prompt>` | Clear history and set a new system prompt |
| `/reset system` | Clear history and restore the default system prompt |
| `/stop` | Stop the request currently streaming for you |
| `/get_models` | Every model priced at $0 for prompt and completion, health first |
| `/fast <prompt>` | Route immediately to the lowest-latency healthy model |
| `/race [judge] <prompt>` | Race 2–3 healthy models concurrently and return the fastest (or synthesised) answer |
| `/research <topic>` | Search the web, extract pages, and answer with numbered citations |
| `/agent <task>` | Run the bounded multi-step tool-using agent |
| `/code`, `/review`, `/explain`, `/testgen` | Coding assistant commands |
| `/persona [name\|custom <prompt>]` | Switch AI persona or set a custom persona |
| `/memory [list\|add\|forget\|clear\|off\|on]` | Manage persistent long-term user memory |
| `/remind <30m\|14:30> <text>`, `/reminders` | Schedule or list persistent reminders |
| `/note <text>`, `/notes` | Save or list persistent notes |
| `/task <text>`, `/tasks` | Manage your persistent todo list |
| `/summarize` | Summarize recent group or private conversation and extract action items |
| `/tr [lang] [text]` | Translate the replied message or inline text (preserves code, URLs, emojis, `@usernames`) |
| `/translate on\|off\|<lang>\|status` | Automatic group translation (groups only) |
| `/group` | Group admin panel: access mode, auto-translation |
| `/admin` | Owner observability panel: uptime, active requests, error rate, latency, model/token usage, cost, cache & storage |
| `/about` | About this bot |

---

## Provider chain (automatic failover)

The bot does not depend on a single backend. Requests walk an ordered list of
providers, and on **any** failure the next model is tried, then the next
provider — at stream creation *and* mid-stream. Partial output is kept, so a
user never loses what already arrived.

Providers are defined in `config.yaml`. A preset name is enough: the endpoint,
the models and the name of the key variable come from the built-in catalogue.

```yaml
providers:
  - name: openrouter        # API_KEY
  - name: groq              # GROQ_API_KEY
  - name: gemini            # GEMINI_API_KEY
  - name: cerebras          # CEREBRAS_API_KEY
  - name: nvidia            # NVIDIA_API_KEY
  #- name: mistral          # MISTRAL_API_KEY
  #- name: deepseek         # DEEPSEEK_API_KEY
  #- name: together         # TOGETHER_API_KEY
  #- name: ollama           # local, no key
```

Built-in presets: `openrouter`, `groq`, `gemini`, `cerebras`, `nvidia`,
`mistral`, `deepseek`, `together`, `ollama`, `lmstudio`. Anything can be
overridden per entry:

```yaml
  - name: groq
    base_url: https://api.groq.com/openai/v1   # optional
    api_key_env: GROQ_API_KEY                  # optional
    models:                                    # optional
      - llama-3.3-70b-versatile
      - openai/gpt-oss-120b
```

- A provider whose key is missing is still listed by `/providers` and in the
  model picker (marked `no key`), but it never costs a request: nothing has to
  be commented out while a key is absent.
- A provider that fails 3 times in a row drops to the back of the chain, first
  for 60 seconds and then for longer (up to 10 minutes) while it keeps failing.
  Healthy providers come first again on their own.
- Every user can override the order for themselves from the picker: pin a
  provider, pin a model, or stay on `auto`. A pin never beats the health
  tracking — a cooling-down backend is still skipped.
- Any OpenAI-compatible endpoint works. Remove the block entirely to fall back
  to the flat `BASE_URL` / `MODEL` / `API_KEY` variables plus every preset
  whose key is set.

### Automatic model choice

In `auto` mode the bot scores every available model against the user's actual
usage — requests per day, average prompt length, conversation size, whether
they send images, and which models they downvoted — and starts with the best
one. The rest of the list stays as the failover order. `/recommend` shows the
same scoring with an explanation, and a 💡 hint appears occasionally when a
pinned model is clearly worse than the recommendation.

## Group behaviour

Two independent behaviours, so the bot never answers a single message twice:

- **`/translate on`** — the bot translates every message and does **not** run a chat completion.
- **Chat mode** (`GROUP_CHAT_MODE`) — controls whether the bot *answers* in groups:
  - `mention` (**default**) — only when you @-mention it or reply to one of its messages
  - `all` — answer every group message
  - `off` — never answer in groups

Who may talk to the bot is a separate switch, and it lives in the chat:

- `everyone` (**default**) — any member can use it, which is what a free
  public bot wants.
- `admins` — only chat administrators and the bot owner are answered;
  everyone else gets a short note instead of silence.
- `owner` — only the bot owner.

Any chat admin can change it from the `/group` panel (no command needed after
that), and the bot notices when it is promoted to admin in a chat.

Direct messages follow their own switch, `PRIVATE_ACCESS`:

- `everyone` (**default**) — anyone who finds the bot gets an answer. This is
  what a public bot wants, and `PUBLIC_MODE` forces it.
- `owner` — the bot only answers you. A stranger who writes anyway gets a
  single short explanation instead of silence, at most once an hour.

So a public group bot and a private assistant are both one setting apart:
`PRIVATE_ACCESS=owner` with `GROUP_ACCESS=everyone`.

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
| `PUBLIC_MODE` | `false` | `true` removes all limits: unlimited budget for every role, no rate limiting, answers every group message |
| `GROUP_ACCESS` | `everyone` | Who may use the bot in a group: `everyone`, `admins` or `owner`. Groups override it from `/group` |
| `PRIVATE_ACCESS` | `everyone` | Who may use the bot in a direct message: `everyone`, `admins` or `owner`. `PUBLIC_MODE` forces `everyone` |
| `LOGS_DIR` | `logs` | Where per-user preferences, favourites and spend are stored. Point it at a mounted volume in a container |
| `MAX_REPLY_CHARS` | `3500` | Longest answer sent as one message (Telegram's hard limit is 4096) |
| `RENDER_MARKDOWN` | `false` | Try MarkdownV2 for the final answer. Off keeps answers plain text |
| `SUGGEST_MODELS` | `true` | Occasionally suggest a model that suits the user's usage |

### Provider keys

| Variable | Provider |
|---|---|
| `API_KEY` | OpenRouter |
| `GROQ_API_KEY` | Groq |
| `GEMINI_API_KEY` | Google Gemini |
| `CEREBRAS_API_KEY` | Cerebras |
| `NVIDIA_API_KEY` | NVIDIA NIM |
| `MISTRAL_API_KEY` | Mistral |
| `HF_TOKEN` | Hugging Face inference providers |

Every extra provider with a key set joins the chain automatically; a matching
preset entry in `config.yaml` is the only other thing needed (a name alone is
enough).

> [!WARNING]
> Set `GUEST_BUDGET=0` (the default) unless you intend to run a public bot.
> `PUBLIC_MODE=true` removes every limit for everyone — anyone who finds your
> bot can spend your provider credits. Set a hard spending cap on the provider
> side as well. With a non-zero guest budget, anyone who finds your bot can spend against your OpenRouter key.

### User owned providers

Users can bring their own key: `/addprovider` walks through name, endpoint and
key, then lists the models that key can use (`🔎 Fetch models`). Keys are sealed
with AES-256-GCM before they touch the disk, are only decrypted for the request
that needs them, and are scoped to the Telegram user who added them.

| Variable | Description |
|---|---|
| `USER_PROVIDER_ENCRYPTION_KEY` | Master key that seals user API keys (`openssl rand -hex 32`). Without it `/addprovider` is disabled rather than storing keys in plaintext |

Model lists are discovered from each provider's own model-list endpoint and
cached for five minutes, so the picker shows what the provider serves right now
instead of a hardcoded list. `🔄 Refresh models` forces a fresh lookup.

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
| `PERSONA_PROMPT` | built in | Replaces the response rules (plain text, no Markdown, short answers) |
| `OLLAMA_BASE_URL` | – | Adds a local Ollama endpoint to the fallback chain |
| `TELEGRAM_API_URL` | – | Self-hosted Bot API server, e.g. `http://localhost:8081/bot%s/%s` |

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
main.go              update loop, group access, chat handling
commands.go          slash commands and reply-keyboard actions
callbacks.go         inline button handling
screens.go           the panels (menu, models, settings, group, owner observability)
features_handlers.go handlers for /fast, /race, /research, /agent, /memory, /persona, /remind, files, voice
hints.go             occasional model suggestions
api/                 provider chain streaming, model race integration, Telegram rendering, chunking
provider/            backends, presets, performance engine, circuit breaker, EWMA latency, failover
router/              smart task classifier, model router, and concurrent model race engine
agent/               bounded multi-step planner, executor, and context state
tools/               safe permission-aware tool registry (calculator, time, url, web, github, translator, notes)
features/            modular capabilities (memory, personas, reminders, research, files, coding, voice)
storage/             persistence abstraction (atomic JSON default and optional PostgreSQL)
 config/             configuration loading, hot reload, persona
ui/                  every keyboard and the callback data format
groups/              per-chat settings, message ring buffer, translation, bot admin
user/                spend tracking, budgets, history, per-user preferences
translator/          language detection and token-preserving translation helper
workers/             background reminder delivery scheduler
internal/            atomicfile, SSRF/path/secret security guards, response cache, Redis client, rate limiter, telemetry
```

---

## Notes on behaviour

- **Long answers** are split into several messages of at most `MAX_REPLY_CHARS` (3500 by default), preferring paragraph boundaries. The assistant is also told to stay under that length.
- **Formatting**: answers are plain text by default, which is why the built-in persona forbids Markdown — Telegram drops the entire message when markup does not parse. Set `RENDER_MARKDOWN=true` to try `MarkdownV2`, then legacy `Markdown`, then plain text.
- **Failover is visible**: with the model footer on, every answer ends with the provider and model that produced it, and a note when the chain had to switch (🔀 switched after N failed attempt(s)).
- **Costs** are read back from the provider that answered, using its generation endpoint (OpenRouter-style). Other providers report usage in the response instead, so there is nothing to add. The lookup retries once, because the statistics can take a moment to finalise.
- **`/stop`** (or the 🛑 button) cancels the request and keeps whatever was generated so far.
- **Buttons expire** after three hours: the state behind 🔁 Regenerate and 👍/👎 is kept in memory only, so a restart simply disables the old buttons.

## License

[MIT](LICENSE)
