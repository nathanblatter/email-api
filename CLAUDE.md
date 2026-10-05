# email-api

VISION: the one way anything of Nathan's sends email — a dumb-simple keyed HTTP/MCP
call that is always accepted, always lands or always tells Nathan why not. Anything
that isn't "send mail reliably from nathanblatter.com" (reading mail, templates,
marketing features, tracking) is out of scope.

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

## Verify
`gofmt -l . && go vet ./... && go test ./...` — the Dockerfile runs the same, so the image is the gate.

## Deploy
Push to `main` → `.github/workflows/deploy.yml` on the self-hosted runner → `~/deploy/email-api`
(`docker compose build && up -d`, health-gated, auto-rollback to `email-api:previous`).
Never hand-deploy. Runtime secrets live in `~/deploy/email-api/.env` (see `.env.example`).

## Gotchas
- Brevo counts recipients, not messages; so does the budget. Default budget 250 leaves room for
  personal-site's own contact-form sends, which do NOT go through this service (yet).
- `/health` returns 200 even when degraded (it is the container liveness probe); read `status`.
- Spool files are safe to delete by hand; `dead/` holds gave-up messages for forensics.
