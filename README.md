# slack-translator

A Slack bot written in Go that translates channel messages into whatever
language each person configured, using free [OpenRouter](https://openrouter.ai)
models with **automatic fallback**: if a model hits its daily rate limit,
the next one in the list takes over with no manual intervention, and each
translation quietly shows which model produced it.

## How it works

1. Each person runs `/idioma es-mx` (or whatever code they want) once, to
   say which language they want to read translations in.
2. When someone posts a message in the channel, the bot:
   - groups the other channel members by their preferred language (so it
     never translates the same text twice for the same language);
   - skips anyone who hasn't configured anything by falling back to
     **English**;
   - translates once per distinct language, using the first available
     model from the list (`OPENROUTER_MODELS`);
   - sends an **ephemeral** message (visible only to people who prefer
     that language) with the translation, a small footer
     (`🌐 → Español · via llama-3.3-70b`) and three buttons:
     **View original**, **View translated**, **View in English**.
3. If a model responds with a rate limit (HTTP 429), it's put on cooldown
   until UTC midnight (OpenRouter's daily free-tier reset) and the bot
   automatically moves to the next model. This state is saved in SQLite,
   so it survives a process restart.
4. Repeated translations (same text, same language) are cached — handy for
   "good morning", "thanks", etc., and it avoids spending free-tier
   requests on things already translated.

---

## 1. Prerequisites

- [Go](https://go.dev/dl/) 1.23 or newer
- An [OpenRouter](https://openrouter.ai/keys) account (no credit card
  needed to use the `:free` models)
- A Slack workspace where you can install apps (see next section if you
  don't have one)

---

## 2. Get a Slack workspace where you have permission

If you try to create an app on your company's Slack workspace and get a
permission error (this is common on managed/corporate workspaces where app
installation is restricted to admins), the simplest path is to create your
own free workspace instead:

1. Go to **https://slack.com/get-started** (or `slack.com/create`)
2. Enter a personal email address (not your work one, to avoid mixing it
   up with your company's workspace)
3. Confirm the code Slack emails you
4. Give the workspace a name (anything works, e.g. "My Sandbox") — you can
   skip inviting anyone else at this step
5. You are automatically the **Owner/Admin** of this new workspace, which
   means no permission issues when creating apps

Slack's **Free plan** is permanent (not a trial) and covers everything
this project needs: creating apps, Socket Mode, slash commands, bots, and
inviting other people later if you want to test translation between two or
more real accounts.

---

## 3. Create the Slack App from the manifest

1. Go to **https://api.slack.com/apps**
2. Click **"Create New App"** (top right)
3. Choose **"From an app manifest"**
4. Select the workspace you just created (or your own workspace, if you
   have permission there)
5. Paste the contents of `slack-app-manifest.yaml` (included in this
   project) into the text box
6. Click **"Next"** → review the summary of scopes/settings → **"Create"**

The app now exists, but it's not installed yet and has no tokens.

---

## 4. Enable Socket Mode and get `SLACK_APP_TOKEN`

The manifest already sets `socket_mode_enabled: true`, so the toggle will
already show as enabled — but the toggle alone doesn't generate a token,
you still need to create one:

1. In the left sidebar of your app's page, go to **"Basic Information"**
2. Scroll down to the **"App-Level Tokens"** section
3. Click **"Generate Token and Scopes"**
4. Give it any name (e.g. `socket-token`)
5. Click **"Add Scope"** and add `connections:write`
6. Click **"Generate"**
7. Copy the value — it starts with `xapp-...`

This is your `SLACK_APP_TOKEN`.

> Socket Mode is what lets the bot run without a public server: it opens
> an outbound WebSocket connection *from* your Go process *to* Slack, so it
> works fine on your laptop with no domain or HTTPS required.

---

## 5. Install the app and get `SLACK_BOT_TOKEN`

1. In the left sidebar, go to **"OAuth & Permissions"**
2. Scroll to the top and click **"Install to Workspace"**
3. Review the requested permissions and click **"Allow"**
4. After installing, the same page shows a **"Bot User OAuth Token"** —
   it starts with `xoxb-...`

This is your `SLACK_BOT_TOKEN`.

> If you land on **"Your Apps"** (a list of all your apps) instead of the
> app's own configuration page, click the app's name first — that list
> view has a "Generate Token" button too, but it's for a different kind of
> token (app configuration tokens) and is not what you need here.

---

## 6. Get an OpenRouter API key

1. Go to **https://openrouter.ai/keys**
2. Sign up (no credit card required for free-tier models)
3. Create a key — it starts with `sk-or-v1-...`

Before hardcoding specific models, check the current free model list at
[openrouter.ai/models](https://openrouter.ai/models) (filter by "Free") —
the lineup changes fairly often.

---

## 7. Configure the `.env` file

```bash
cp .env.example .env
```

Open `.env` and fill in the three values you just collected:

```
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
OPENROUTER_API_KEY=sk-or-v1-...
```

The other variables (`OPENROUTER_MODELS`, `APP_URL`, `DB_PATH`) have
sensible defaults in `.env.example` and don't need to be changed to get
started.

---

## 8. Invite the bot to a channel

Even with everything installed, the bot only sees messages in channels
it's been invited to. Inside the Slack channel you want to use:

```
/invite @translator
```

(or whatever name you gave the bot in the manifest — the default is
`translator`, displayed as "Translator")

---

## 9. Run the project locally

**Check Go is installed:**
```bash
go version
```
If not, on macOS: `brew install go`

**Get the project and install dependencies:**
```bash
cd slack-translator
go mod download
```

**Run it:**

SQLite (`mattn/go-sqlite3`) needs CGO enabled — usually on by default on
macOS, but it doesn't hurt to be explicit:

```bash
export CGO_ENABLED=1
export $(grep -v '^#' .env | xargs)
go run ./cmd/slack-translator
```

If everything is configured correctly, you'll see:
```
slack-translator starting (models: meta-llama/llama-3.3-70b-instruct:free, qwen/qwen3-coder:free)
connecting to Slack...
connected to Slack
```

Or build a binary instead of using `go run`:
```bash
go build -o bin/slack-translator ./cmd/slack-translator
./bin/slack-translator
```

### Troubleshooting

| Error | Likely cause |
|---|---|
| `cgo: C compiler not found` | Missing Xcode command line tools on macOS — run `xcode-select --install` |
| `missing required environment variable: ...` | The `.env` wasn't loaded, or you're not in the project directory. Check with `cat .env` |
| `slack auth test failed (check SLACK_BOT_TOKEN)` | Token is wrong, or the app hasn't been installed to the workspace yet (see step 5) |
| Bot never posts anything in the channel | The bot wasn't invited to the channel (see step 8), or nobody in the channel has set a language with `/idioma` yet |

---

## 10. Try it out

1. In Slack, have at least two accounts in the channel (yourself + a
   second account, or a teammate) each set a different language:
   ```
   /idioma pt-br
   /idioma es-mx
   ```
2. Post a message as one of them
3. The other account should get an **ephemeral** message (only visible to
   them) with the translation and the three view-toggle buttons

`/idioma` with no argument lists all supported language codes.

---

## Project structure

```
cmd/slack-translator/   entry point (main.go): reads env vars, wires everything, runs
translator/             OpenRouter client + model pool with automatic fallback
store/                  SQLite persistence (preferences, cache, cooldowns, usage stats)
slackbot/               Socket Mode event loop, translation fan-out, slash command, buttons
slack-app-manifest.yaml ready-to-paste Slack app manifest (scopes, Socket Mode, slash command)
```

## Known limitations / possible next steps

- No automatic detection of the sender's own language — the bot assumes
  people write in whatever language they configured with `/idioma` (if
  they configured one) and simply asks the model to translate into the
  recipient's language, which works well because LLMs detect the source
  language on their own.
- Free-tier model rate limits are typically 20 requests/minute and 200
  requests/day per model (can be raised to 1,000/day by adding $10 of
  credit to your OpenRouter account, without needing to spend it). If
  every model in the list is exhausted on the same day, translation fails
  silently (only logged) until the reset — worth adding an ephemeral
  warning to the message author in that case.
- No retry/backoff for transient errors (timeouts, 5xx) beyond falling
  through to the next model in the list.
- Some free OpenRouter providers may retain or use submitted data for
  training, depending on their policy — check before using this in
  channels with sensitive information.
