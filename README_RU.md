<h1 align="center">
    <img src="img/logo.png" width="220" />
    <div>
    OpenRouter
    <br>
    Bot
    </div>
</h1>

<h4 align="center">
    <a href="README.md">English (🇺🇸)</a> | <strong>Русский (🇷🇺)</strong>
</h4>

Этот проект позволяет за несколько минут запустить своего Telegram бота для общения с бесплатными и платными моделями ИИ через [OpenRouter](https://openrouter.ai), или локальными LLM, например, через [LM Studio](https://lmstudio.ai).

> [!NOTE]
> Этот репозиторий является форком проекта [openrouter-gpt-telegram-bot](https://github.com/deinfinite/openrouter-gpt-telegram-bot), который добавляет новые функции (например, изменение текущей модели и форматирование `Markdown` в ответах бота) и оптимизирует процесс запуска в контейнере.

<details>
    <summary>Пример</summary>
    <img src="./img/example.png">
    <img src="./img/commands.png">
</details>

> [!IMPORTANT]
> Полное описание новых параметров конфигурации (бюджеты, ограничение запросов,
> режимы работы в группах) смотрите в [англоязычном README](README.md).

## Что добавлено в этом форке

| Возможность | Описание |
|---|---|
| Потоковые ответы | Токены дописываются в сообщение по мере генерации |
| Резервный Gemini | Автопереключение при лимитах и ошибках 5xx |
| `/tr` и `/translate` | Перевод одного сообщения или автоматический перевод группы |
| Бюджеты пользователей | Лимиты трат в день / месяц / за всё время |
| Ограничение частоты | Максимум запросов от пользователя в минуту |
| Длинные ответы | Автоматически разбиваются на несколько сообщений |

## Подготовка

- Зарегистрируйтесь в [OpenRouter](https://openrouter.ai) и получите [API ключ](https://openrouter.ai/settings/keys).

- Создайте своего Telegram бота, используя [@BotFather](https://telegram.me/BotFather) и получите его API токен.

- Получите свой Telegram id, используя [@getmyid_bot](https://t.me/getmyid_bot).

> [!TIP]
> При запуске бота вы сможете увидеть в логах идентификаторы других пользователей, которым вы также сможете предоставить доступ к боту в дальнейшем.

## Установка

Для локального запуска в системе Windows или Linux, загрузите предварительно собранный бинарный файл (без зависимостей) на странице [релизов](https://github.com/Lifailon/openrouter-bot/releases).

### Запуск в Docker

- Создайте рабочий каталог бота:

```bash
mkdir openrouter-bot
cd openrouter-bot
```

- Создайте `.env` файл и заполните базовые параметры:

```bash
# OpenRouter api key
API_KEY=
# Список бесплатных моделей: https://openrouter.ai/models?max_price=0
MODEL=deepseek/deepseek-r1:free
# Telegram api key
TELEGRAM_BOT_TOKEN=
# Ваш Telegram id
ADMIN_IDS=
# Список пользователей для доступа к боту, разделенных запятыми
ALLOWED_USER_IDS=
# Отключить гостевой доступ (включен по умолчанию)
GUEST_BUDGET=0
# Язык, используемый в ответах бота
LANG=RU
```

Список всех доступных параметров приведен в файле [.env.example](https://github.com/Lifailon/openrouter-bot/blob/main/.env.example)

- Запустите контейнер, используя образ из [Docker Hub](https://hub.docker.com/r/lifailon/openrouter-bot):

```bash
docker run -d --name OpenRouter-Bot \
    -v ./.env:/openrouter-bot/.env \
    --restart unless-stopped \
    lifailon/openrouter-bot:latest
```

Образ собран для платформ `amd64` и `aarch64` (Raspberry Pi) с использованием [docker buildx](https://github.com/docker/buildx).

## Сборка

```bash
git clone https://github.com/Lifailon/openrouter-bot
cd openrouter-bot
docker-compose up -d --build
```
