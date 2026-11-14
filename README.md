# Twarm legacy compatibility

The project is now [Twarm](https://github.com/arizzi74/Twarm). Use the original
repository there for source, issues, documentation, history and future releases.

This small repository preserves the historical installer and website URLs and
the unchanged v0.6.23 and v0.7.32 compatibility releases for installed managers
requiring `arizzi74/Codex-Telegram-Gateway` asset URLs and signing identities.
For v0.7.32, all application files and SHA256SUMS match the canonical release;
only the provenance bundle is separately signed by this exact repository's
release workflow and tag for the oldest clients.

Current managers resolve Twarm through the original immutable repository ID.
Existing credentials, service names, paths, session data and Telegram `/tg`
commands are preserved. Intermediate old managers still saving the Codex name
may need recovery through the current Twarm installer because they hardcode the
previous canonical MACT name and repository ID.

The main-branch workflow uses Twarm to locate the original repository but keeps
MACT as the original v0.7.32 signing identity. Historical tags and published
release assets must never be moved or replaced. Existing release publication is
refused. The website forwards historical paths, queries and fragments to Twarm;
the installer forwards arguments unchanged to the current Twarm installer.
