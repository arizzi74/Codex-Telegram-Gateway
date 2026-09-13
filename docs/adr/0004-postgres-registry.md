# PostgreSQL registry

Context: routing, callbacks and command acceptance must survive gateway restart.

Decision: PostgreSQL stores relational identities and routing. Embedded,
checksummed migrations and transactions govern all authoritative changes.

Consequences: readiness requires a reachable database with matching schema.
Workers continue independently while the gateway database is unavailable.
