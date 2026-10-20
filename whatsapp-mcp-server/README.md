# WhatsApp MCP server

By default the server reads `../whatsapp-bridge/store/messages.db` and calls the bridge at
`http://localhost:8080/api` (legacy mode).

## Service mode

Set `WA_SERVICE_URL` to talk to a remote bridge service behind the gateway instead. The server
then never reads the local database or local files.

| Variable | Purpose |
|---|---|
| `WA_SERVICE_URL` | Base URL, no trailing slash (e.g. `https://wa.example.com`). Must be `https://` (plain `http://` only for `localhost`/`127.0.0.1`). |
| `WA_SERVICE_TOKEN` | Sent as `Authorization: Bearer ...` (read or send token). |
| `CF_ACCESS_CLIENT_ID` | Sent as `CF-Access-Client-Id`. |
| `CF_ACCESS_CLIENT_SECRET` | Sent as `CF-Access-Client-Secret`. |

Missing credentials fail on first use with a message naming the missing variables (values are
never printed). Requests use a 5 s connect / 20 s read timeout and are never retried; errors
returned to the model contain only the HTTP status or a short reason.

| Tool | Service mode |
|---|---|
| `list_chats` | `GET /api/chats` (`query`, `limit` only) |
| `list_messages` | `GET /api/messages` (`chat_jid`, `before`, `query`, `limit`); without `chat_jid`, `query` searches all chats |
| `search_messages` | `GET /api/search` |
| `search_contacts` | `GET /api/chats?query=` (groups excluded) |
| `send_message` | `POST /api/send` |
| `download_media` | `POST /api/download` (returned path is on the service host) |
| `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context` | not available |
| `send_file`, `send_audio_message` | not available: local files are not read and client paths are not sent |

A read-only profile cannot send: the gateway rejects its token on `/api/send` with 403, and the
tool returns "service denied this operation (check profile credentials)".

## Tests

```sh
uv run --with pytest pytest -q
```
