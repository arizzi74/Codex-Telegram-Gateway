# Codex Telegram control plane

A Go gateway and cross-platform worker implementing
[the specification](CODEX_TELEGRAM_CONTROL_PLANE_SPEC.md).

Implementation is in progress. See [the implementation tracker](docs/implementation.md)
for verified milestones and remaining work. Do not deploy an incomplete milestone.

The gateway uses PostgreSQL and Telegram webhooks. Workers use a durable local
store, outbound WSS, and supervised Codex app-server stdio connections.
The user's additional requirements include a passkey-authenticated administration
interface and local CLI attachment commands.

Secrets, local configuration, state databases, and build products are excluded
from Git. Never commit `.botsecrets`.
