# email-api

VISION: the one way anything of Nathan's sends or receives email — a dumb-simple keyed
HTTP/MCP call that is always accepted, always lands or always tells Nathan why not, and an
inbox that is a table + bucket he owns rather than a third-party mailbox. Templates,
marketing features and tracking are out of scope.

## Stack
- Go (stdlib + `modelcontextprotocol/go-sdk` + `go-redis`), single binary, alpine image.
- Layout: `cmd/email-api` (wiring) · `internal/mail` (validation + MIME + SMTP) ·
  `internal/service` (delivery policy: send → spool → page → retry) · `internal/spool`
  (one JSON file per queued message) · `internal/quota` (daily budget in Redis, memory fallback) ·
  `internal/fallback` (iMessage pager) · `internal/api` (HTTP) · `internal/mcpserver` (MCP tools).
- Relay: `postfix` service in `~/docker-services` (→ Brevo). Reached over the external
  `docker-services_default` network. Postfix's `sender_canonical` map keeps `@nathanblatter.com`
  senders and rewrites everything else to noreply@ (`~/docker-services/postfix-sender-canonical`).
- Fallback: imessage-api on the host (`http://100.79.61.79:8899`).
- Inbox: `worker/` (Cloudflare Email Worker, deployed by CI) → `POST /inbound` on the public listener
  → `internal/inbox` (MIME parser, Postgres store + MinIO bucket `email-inbox`, iMessage preview).
  Keyed `/inbox*` routes and `list_inbox`/`read_email`/… MCP tools. Suspicious = failed SPF and DKIM, or
  `inbox.Sanitize` flagged prompt-injection (hidden HTML text, invisible Unicode, instruction-shaped phrasing);
  MCP reads are text-only inside an `untrusted` envelope. Add new injection patterns to `injectionPatterns`
  with a good/bad sample in `sanitize_test.go`.
- Large attachments: `internal/files` uploads to the shared MinIO bucket `email-files` (lifecycle-expired)
  and `api.Files` serves `/f/{token}/{name}` on the public listener `:4501`; the compose `tunnel`
  service publishes it as `file.nathanblatter.com`. The keyed API on `:4500` stays Tailscale-only.

## Keys
Named API keys live hashed in the inbox db (`api_keys`); `EMAIL_API_KEY` stays valid as actor `env`.
Mint per consumer with `docker compose exec app email-api keygen <name>` in `~/deploy/email-api`;
`email-api keys` lists, `email-api revoke <name>` kills the key and its OAuth tokens. Every send logs
`actor=`; OAuth tokens carry the key name they were minted with.

## Verify
`gofmt -l . && go vet ./... && go test ./...` — the Dockerfile runs the same, so the image is the gate.

## Deploy
Push to `main` → `.github/workflows/deploy.yml` on the self-hosted runner → `~/deploy/email-api`
(`docker compose build && up -d`, health-gated, auto-rollback to `email-api:previous`).
Never hand-deploy. Runtime secrets live in `~/deploy/email-api/.env` (see `.env.example`); the
workflow also deploys `worker/` with wrangler using `CLOUDFLARE_API_TOKEN` from that file.

## Gotchas
- Brevo counts recipients, not messages; so does the budget. Default budget 250 leaves room for
  personal-site's own contact-form sends, which do NOT go through this service (yet).
- `/health` returns 200 even when degraded (it is the container liveness probe); read `status`.
- Spool files are safe to delete by hand; `dead/` holds gave-up messages for forensics.
