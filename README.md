# MACT legacy compatibility

The project is now **[MACT — Multi Agent Control Tower](https://github.com/arizzi74/MACT)**.
Use MACT for source, issues, documentation, history, and future releases.

This small repository preserves the old installer and website addresses and a
single compatibility release, `v0.6.23`, for installed updaters that require
`arizzi74/Codex-Telegram-Gateway` release asset URLs and signing identities.
Its application archives, installers, and checksums are copied unchanged from
the official MACT release. Only `SHA256SUMS.sigstore.json` is replaced with a
fresh attestation issued by this repository's exact release workflow and tag.
The workflow verifies the official attestation and every artifact before
signing, then verifies the compatibility attestation with both the current
verifier and the actual v0.6.19 verifier before publishing.

After a legacy updater installs v0.6.23, the next successful normal update
persists the canonical MACT repository in its saved settings. Earlier historical
releases are available in MACT; this repository does not mirror their old
addresses. This is not a source or development fork.

The forwarding installer passes all command arguments unchanged to the current
MACT bootstrap, including no arguments, `version`, and explicit `--repo`
selections. The v0.6.23 manager handles migration of saved legacy settings.

The release workflow runs only on the `v0.6.23` tag. A manual dispatch must select
that tag, after its official MACT release exists. Existing releases are never
overwritten. The canonical source commit must be pinned in the workflow before
use. Website files support GitHub Pages published from the `main` branch root.
