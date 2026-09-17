# justsay

Agent Harness written in Go.

## Run the project

1. Install the Go version declared in [go.mod](go.mod) (currently Go 1.26.5), then confirm it is available:

   ```bash
   go version
   ```

2. Create a local environment file and fill in the required values. `.env` is read by the application and should not be committed.

   ```bash
   cp .env.example .env
   ```

   At a minimum, set:

   ```dotenv
   GEMINI_API_KEY="your Gemini API key"
   GEMINI_MODEL="your Gemini model ID"
   MOCK_AGENT_CALL="false"
   ```

   Set `MOCK_AGENT_CALL` to `true` to start without making model requests. Set `HITL_ENABLED` to `true` to hold the tool calls listed in `agent/human-in-the-loop/hitl_config.yml` for human approval, and configure the `REDIS_*` values for those approvals, including approvals sent through a paired Telegram account, since paused approvals are stored in Redis.

3. Start the CLI from the repository root:

   ```bash
   go run .
   ```

## CLI commands

| Command | Description |
| --- | --- |
| `help` | Show built-in and integration commands. |
| `reset` | Clear the current conversation. |
| `/on` | Enable real model calls (`MOCK_AGENT_CALL=false`). |
| `/off` | Mock model calls (`MOCK_AGENT_CALL=true`). |
| `/verify telegram` | Connect a Telegram bot to the running agent. |
| `exit` | Close the CLI. |

## Telegram integration

Run `/verify telegram` at the `justsay>` prompt. Enter a Telegram bot token when prompted (create one with [@BotFather](https://t.me/BotFather) if needed). The CLI validates the token, prints a one-time `/verify <code>` command, and waits for you to send it to that bot from the Telegram account to pair. After pairing, the CLI keeps listening for messages from that account and restores the saved pairing when it starts again.
