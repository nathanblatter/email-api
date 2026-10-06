# email-api

HTTP + MCP gateway for sending email from `@nathanblatter.com` addresses. The
sibling of [imessage-api](../imessage-api): same `X-API-Key` auth, same
Tailscale-only bind, same tiny surface.

Mail goes out through the house postfix relay in `docker-services` (→ Brevo).
If the relay is down the message is spooled to disk, retried for 48h, and the
content is paged to Nathan's phone through imessage-api so nothing waits in
silence. A daily send budget (Brevo free tier is 300/day) is tracked in Redis;
over budget, mail is spooled until the next UTC day instead of bouncing.

## Endpoints

| Method | Path            | Auth | Description |
|--------|-----------------|------|-------------|
| GET    | `/health`       | no   | relay / iMessage / quota status, sent today, queued count |
| POST   | `/send`         | yes  | send one email (JSON or multipart form) |
| POST   | `/send/batch`   | yes  | send many; per-message results |
| GET    | `/queue`        | yes  | list spooled messages |
| POST   | `/queue/retry`  | yes  | run a retry pass now |
| *      | `/mcp`          | yes  | MCP (streamable HTTP): `send_email`, `send_batch_email`, `email_health`, `list_queued_emails`, `retry_queued_emails` |

Auth: `X-API-Key: …` (or `Authorization: Bearer …`). Base URL on the tailnet: `http://100.79.61.79:4500`.

### POST /send (JSON)

```json
{
  "from": "Nathan Blatter <nathan@nathanblatter.com>",
  "to": ["someone@example.com", "Other <other@example.com>"],
  "cc": "cc@example.com",
  "bcc": ["hidden@example.com"],
  "reply_to": "nathan@nathanblatter.com",
  "subject": "Hello",
  "text": "Plain-text body",
  "html": "<p>HTML body <img src=\"cid:logo\"></p>",
  "attachments": [
    {"filename": "report.pdf", "content": "<base64>", "content_type": "application/pdf"},
    {"filename": "logo.png", "content": "<base64>", "content_id": "logo"}
  ],
  "headers": {"X-Priority": "1", "In-Reply-To": "<msg@example.com>"}
}
```

- `from` defaults to `Nathan Blatter <noreply@nathanblatter.com>` and must be under an allowed domain.
- `to` / `cc` / `bcc` accept a string (comma-separated) or an array. At least one recipient is required.
- `text` and/or `html`; both → `multipart/alternative`.
- `content_id` makes an attachment inline (`multipart/related`), referenced from HTML as `cid:<id>`.
- `headers` adds extra headers (threading, priority, List-Unsubscribe…). Core headers can't be overridden.
- Max message size (after MIME encoding): `EMAIL_MAX_MESSAGE_MB`, default 10 (Brevo's limit).
- **Large files become download links.** Any attachment with `as_link: true`, and (largest first)
  whatever is needed to bring the message under the limit, is uploaded to MinIO and replaced by an
  expiring link (`EMAIL_FILES_TTL`, default 30 days) appended to the text and HTML bodies and
  returned in the response as `links`. Links are served from `https://file.nathanblatter.com/f/<token>/<name>`
  by a separate public listener (`:4501`, behind a Cloudflare tunnel) that knows only that route.
  Single files up to `EMAIL_MAX_UPLOAD_MB` (default 1024). Inline (`content_id`) images are never linked.

Responses:

| Code | Body | Meaning |
|------|------|---------|
| 200  | `{"status":"sent","id":…,"recipients":[…]}` | handed to the relay |
| 202  | `{"status":"queued","reason":"relay_down"\|"daily_budget","fallback":"imessage"\|"none",…}` | spooled; retried automatically |
| 400  | `{"status":"rejected","error":…}` | bad input |
| 401  | `{"error":"unauthorized"}` | |

### POST /send (multipart, for files from curl)

```bash
curl -H "X-API-Key: $KEY" http://100.79.61.79:4500/send \
  -F from="nathan@nathanblatter.com" -F to="a@example.com" -F to="b@example.com" \
  -F subject="Q3 report" -F text="See attached." \
  -F file=@report.pdf -F file=@chart.png
```

Every file field becomes an attachment; the content type is inferred from the filename.
Add `-F as_link=report.pdf` (or `-F as_link=all`) to force download links.

### POST /send/batch

```json
{"messages": [ {…same shape as /send…}, {…} ]}
```

Returns `{"status":"sent"|"partial"|"queued"|"rejected","sent":n,"queued":n,"rejected":n,"results":[…]}`.
One invalid message does not stop the rest.

### MCP

Register in `~/.claude.json`:

```json
"email": {"type": "http", "url": "http://100.79.61.79:4500/mcp", "headers": {"X-API-Key": "…"}}
```

## Inbox (receiving)

Cloudflare Email Routing catches every address on nathanblatter.com and runs the Email Worker in
`worker/`, which POSTs the raw message to `https://file.nathanblatter.com/inbound` (public listener,
shared secret `EMAIL_INBOUND_SECRET`). email-api parses it, stores the message in Postgres
(`DATABASE_URL`, database created on first start) and the attachments plus the raw `.eml` in the
MinIO bucket `email-inbox`, then texts the phone a preview (`EMAIL_INBOX_NOTIFY`). Mail that passes
neither SPF nor DKIM is kept but flagged `suspicious` and hidden from default listings.

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/inbox?limit=&unread=true&q=&suspicious=true\|all&before=` | newest first; `unread` count included |
| GET    | `/inbox/{id}` | full message: bodies, headers, threading ids, attachments |
| GET    | `/inbox/{id}/attachments/{aid}` | attachment bytes |
| POST   | `/inbox/{id}/read?read=false` | mark read / unread |
| DELETE | `/inbox/{id}` | delete message + stored files |

MCP: `list_inbox`, `read_email`, `get_email_attachment` (≤5 MB inlined), `mark_email_read`, `delete_email`.

**Prompt-injection defence (sanitize stage).** Every received message goes through `inbox.Sanitize` before
storage: NFKC normalisation and removal of invisible characters (zero-width, bidi overrides, tag characters,
soft hyphens); HTML comments and elements hidden by style or attribute (`display:none`, zero font size,
white-on-white, off-screen, `hidden`, `aria-hidden`) are stripped and their text kept separately as
`hidden_text`; the subject, bodies and hidden text are scanned for instruction-shaped content (ignore-your-
instructions, "you are an AI", tool-call requests, secret exfiltration, prompt markup, …). Matches set
`injection_suspected` with `injection_reasons`, mark the message `suspicious` (hidden from default listings,
no phone page) and the UI shows a warning. MCP reads return a `{"untrusted": true, "notice": …, "data": …}`
envelope with text only; HTML needs `include_html: true`. None of this makes untrusted text safe for a model;
it narrows the channel and labels it. Keep send/delete as separate, confirmed steps when agents read mail.
To reply, call `send_email` with `headers: {"In-Reply-To": "<message_id>", "References": "<message_id>"}`.

If email-api or the tunnel is down the Worker throws, Cloudflare tempfails the sender, and the
sending server retries later, so nothing is lost. The Worker is deployed by CI from the host's
`CLOUDFLARE_API_TOKEN`; the catch-all rule was set once via the Cloudflare API.

## Failure modes

| Situation | Behaviour |
|-----------|-----------|
| Relay refuses / unreachable | 202 queued, spooled under `/data/spool`, retried every minute for 48h, phone paged (throttled to one page per 10 min with a count of the rest), recovery page when the spool drains |
| Daily budget spent | 202 queued with `retry_after` = next UTC midnight, phone paged once |
| Redis unreachable | budget counted in-process; `/health` reports `quota: redis-unreachable` |
| iMessage API down | outage is only logged; `/health` reports `imessage: down` |
| Message older than `EMAIL_SPOOL_MAX_AGE` | moved to `/data/spool/dead/`, phone paged |
| MinIO unreachable at start | mail still sends; oversized attachments are rejected with a clear error until restart |
| Postgres/MinIO unreachable at start (inbox configured) | process exits so the restart policy retries; meanwhile the Worker tempfails senders, who retry |

## Config

See `.env.example`. Secrets come from `.env`; everything else defaults in `docker-compose.yml`.

## Develop

```bash
go test ./...            # unit + MCP transport tests, no network
go run ./cmd/email-api   # needs EMAIL_API_KEY; SMTP_HOST=127.0.0.1 to use the host-published postfix
```

## Deploy

Push to `main`. The self-hosted runner on the Mac Mini pulls into `~/deploy/email-api`,
builds the image (the Dockerfile runs gofmt + vet + tests before compiling, so a red
suite can't produce an image), recreates the container, waits for the health check,
and rolls back to the previous image if it never comes up.
