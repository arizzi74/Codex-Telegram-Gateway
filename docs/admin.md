# Administrator console

The passkey-only console is served at `/admin/`. It is available only when the
gateway mounts `admin.New(store, admin.Config{Origin: cfg.PublicBaseURL})`.
`PublicBaseURL` must be the gateway's exact public HTTPS origin, such as
`https://gateway.example.com`. WebAuthn derives its relying-party ID from that
origin's hostname; browser requests must match the full configured origin,
including its port when one is specified.

Run `codex-gateway admin bootstrap` locally to create a bootstrap token. It is
shown once, stored only as a SHA-256 hash, expires after 15 minutes, and is
atomically consumed when the first resident, user-verified passkey is saved.
There is no open signup path and no password fallback.

The browser uses these endpoints:

| Endpoint | Purpose |
| --- | --- |
| `POST /api/v1/admin/passkeys/register/begin` | Begins bootstrap or an authenticated additional-passkey ceremony. |
| `POST /api/v1/admin/passkeys/register/finish` | Verifies and persists a registration response. |
| `POST /api/v1/admin/login/begin` / `finish` | Begins and completes a discoverable passkey login. |
| `GET /api/v1/admin/dashboard` | Returns workers, runtimes, sessions, and pending counts. |
| `GET, DELETE /api/v1/admin/passkeys[/{id}]` | Lists credentials or revokes a non-final credential. |
| `POST /api/v1/admin/workers` | Creates a worker and returns its one-time enrollment token. |
| `DELETE /api/v1/admin/workers/{id}` | Revokes a worker. |
| `POST /api/v1/admin/workers/{id}/rotate-token` | Returns a replacement enrollment token. |

All state-changing requests require the exact configured `Origin`, the strict
same-site session, and a double-submit `X-CSRF-Token`. Ceremony cookies bind
the browser to a five-minute, server-persisted WebAuthn session. Admin sessions
are opaque hashed random tokens, expire after eight hours, and can be revoked.
Worker tokens are only included in the create/rotate response and are never
logged or returned later.
