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
  `internal/fallback` (iMessage pager) · `internal/api` (HTTP) · `internal/mcpserver` (MCP tools) ·
  `internal/bridge` (SQS outbox consumer + SES inbound consumer). Compose services: `app` (Caddy edge) ·
  `api` · `bridge` · `inbound` · `tunnel`.
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
- SQS bridge: `internal/bridge` (`email-api sqs-bridge`, compose service `bridge`) is the only consumer
  of the auth-service stack's `MailQueue`. auth-service is a Lambda that cannot reach the Tailscale-only
  keyed port, so it drops `{"to","subject","text","from"?}` on SQS and the bridge replays each as
  `POST /send` on `http://app:4500` with the `auth-service` key — every send still has an `actor=`,
  nothing public changes. 2xx/4xx → delete (4xx is logged as an error: retrying cannot fix a bad
  message); 5xx/429/network → keep (SQS redelivers, DLQ after 5 receives). Env: `SQS_QUEUE_URL`
  (empty = idle, never exits), `AWS_REGION`, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` (SDK chain),
  `BRIDGE_API_URL`, `BRIDGE_API_KEY`. Never logs the key or message text.

- byuisresearch.com (second domain, 2026-10-22): everything on AWS, DNS stays in Route 53 (zone
  `Z07173382LY2R2TMZRD27`, also hosts ai./hh./menu./kateri. — never touch those records). Sending:
  `mail.RoutedSender` routes From domains in `SES_DOMAINS` to `mail.SESSender` (SES API, raw MIME, SES
  signs DKIM; MAIL FROM `mail.byuisresearch.com` for aligned SPF); everything else still goes to postfix.
  Receiving: SES receipt rule set `email-api` → S3 `email-api-inbound-byuisresearch-<acct>` (30-day
  expiry) + SNS → SQS `email-api-inbound-byuisresearch` (DLQ after 5) → compose service `inbound`
  (`email-api ses-inbound`, `bridge.Inbound`) → `POST /inbound` on `http://app:4501` with the shared
  secret, prepending an `Authentication-Results` header built from SES's verdicts. Pull-based: an
  outage parks mail in SQS (14 days). IAM user `email-api` is scoped to exactly those three things.
  Env: `SES_DOMAINS`, `SES_REGION`, `SES_AWS_ACCESS_KEY_ID`/`SECRET`, `INBOUND_SQS_QUEUE_URL`.
  Gotcha: SES account is in sandbox until production access is granted (200/day, verified recipients only).

## Keys
Named API keys live hashed in the inbox db (`api_keys`); `EMAIL_API_KEY` stays valid as actor `env`.
Mint per consumer with `docker compose exec api email-api keygen <name>` in `~/deploy/email-api`;
`email-api keys` lists, `email-api revoke <name>` kills the key. Every send logs `actor=`.

## Phone connector (Squawk)
`https://email.nathanblatter.com/mcp` is an RFC 9728 protected resource whose authorization
server is Squawk (`internal/connector`). Claude registers itself at Squawk, Nathan approves the
connector in the Squawk console with his passkey, and the resulting ES256 tokens (aud = this
service's Squawk id, scope `write`) are verified against Squawk's JWKS; connector sends carry
`actor=claude-connector`. Env: `EMAIL_PUBLIC_URL`, `EMAIL_AUTH_ISSUER` (default Squawk),
`EMAIL_AUTH_SERVICE_ID` (required). The old in-app OAuth server and its `oauth_*` tables are gone
(dropped at startup).

## Apple Mail (IMAP + SMTP submission)
`internal/imapd` (compose service `mailgw`, `email-api mail-gateway`) lets Apple Mail use the owned
inbox as nathan@nathanblatter.com. Added 2026-10-25 (flightdeck email-api-22).
- Path: Mail -> `mail.nathanblatter.com` (Cloudflare A record, grey cloud, -> Hetzner 178.104.198.127)
  -> HAProxy on Hetzner (`/etc/haproxy/haproxy.cfg`, TCP mode, `send-proxy-v2`, ufw 993/465)
  -> tailnet -> `100.79.61.79:1993` (IMAPS) / `:1465` (SMTPS) -> `mailgw`. TLS terminates in mailgw
  (certmagic, Let's Encrypt DNS-01 via `CLOUDFLARE_API_TOKEN`, stored in the `mailcerts` volume), so
  Hetzner never sees plaintext. The PROXY header is REQUIRED on those ports.
- Mailboxes: INBOX = `inbox_messages` rows whose `envelope_to` contains `MAIL_ADDRESS`, UIDs from
  `inbox_messages.imap_uid` (sequence `inbox_messages_imap_uid_seq`, UIDVALIDITY 1), extra flags in
  `imap_flags`, \Seen = `read`. Sent/Drafts/Trash/Archive/Junk + folders Mail creates live in
  `imap_folders` / `imap_messages` (raw BYTEA). Held in memory, written through to Postgres; a 10 s
  sync loop picks up new deliveries and web-inbox reads/deletes. Expunging from INBOX (incl. Mail's
  move to Trash) deletes the inbox row, same as the web inbox delete. Single instance only.
- Auth: per-device credentials in `mail_devices` (`internal/maildev`, SHA-256 hashed, `md_` prefix),
  minted ONLY by `https://email.nathanblatter.com/mail`: owner signs in via Squawk OAuth (public PKCE
  client `EMAIL_MAIL_CLIENT_ID`, resource `/mcp`, token must have sub=owner, approved with the owner
  passkey in the Squawk console), gets a .mobileconfig with the credential; revoke on the same page.
  10 failed logins per IP per 15 min -> blocked. Username must be MAIL_ADDRESS.
- Sending: SMTP AUTH PLAIN, MAIL FROM and the From header must be MAIL_ADDRESS; the raw message goes
  to keyed `POST /send/raw` (actor `mail-gateway`, key `MAIL_GATEWAY_API_KEY`) which strips Bcc and
  applies the From policy, budget, spool and pager. Mail itself APPENDs the copy to Sent.
- Local test: run `email-api mail-gateway` with `MAIL_TLS_CERT`/`MAIL_TLS_KEY` (self-signed),
  `MAIL_PROXY_PROTOCOL=false` and a throwaway DATABASE_URL; drive it with python imaplib/smtplib.

## Verify
`gofmt -l . && go vet ./... && go test ./...` — the Dockerfile runs the same, so the image is the gate.

## Deploy
Push to `main` → `.github/workflows/deploy.yml` on the self-hosted runner → `~/deploy/email-api`:
build, then `--scale api=2 --no-recreate`, wait for the new container's health, retire the old
(zero downtime behind the Caddy edge `app`, which owns port 4500 and the tunnel targets). An
unhealthy new container is removed and the old one keeps serving.
Never hand-deploy. Runtime secrets live in `~/deploy/email-api/.env` (see `.env.example`); the
workflow also deploys `worker/` with wrangler using `CLOUDFLARE_API_TOKEN` from that file.

## Gotchas
- Brevo counts recipients, not messages; so does the budget. Default budget 250 leaves room for
  personal-site's own contact-form sends, which do NOT go through this service (yet).
- `/health` returns 200 even when degraded (it is the container liveness probe); read `status`.
- Spool files are safe to delete by hand; `dead/` holds gave-up messages for forensics.
