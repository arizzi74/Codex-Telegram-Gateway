# Legacy compatibility for MACT and Twarm

The official project currently lives at [arizzi74/MACT](https://github.com/arizzi74/MACT).
The v0.7.32 bridge prepares installed clients for the Twarm repository name.
Source, issues, history and future development remain in that original repository.

This small compatibility repository preserves the old installer and website URLs
and signed bridge releases for managers that require the exact
`arizzi74/Codex-Telegram-Gateway` asset URLs and certificate identity.
The historical v0.6.23 release remains unchanged. The newer v0.7.32 bridge copies
all application archives, native managers, installer and SHA256SUMS unchanged
from the official v0.7.32 release. Only SHA256SUMS.sigstore.json differs: this
repository's exact release workflow signs the unchanged official manifest at
`refs/tags/v0.7.32`.

The workflow verifies the official MACT provenance and every artifact before
signing. It verifies the resulting Codex attestation with both the current
verifier and the actual v0.6.19 verifier, then checks all uploaded draft files
before publication. It refuses to overwrite existing releases.

After installing v0.7.32, managers resolve the original official repository by
its immutable GitHub ID and accept only the explicitly authorized MACT or Twarm
canonical names. A successful normal update persists the resolved canonical
name. Existing credentials, service names, configuration and session storage
remain in place. Existing Telegram `/tg` command names remain unchanged.

Some intermediate managers that still save the Codex repository name hardcode
MACT's original name and ID. After the canonical repository is renamed, those
managers require recovery through the current official installer; an HTTP
redirect cannot satisfy their old identity check. This repository is a bridge,
not a development fork or a complete historical release mirror.

The release workflow runs only at `refs/tags/v0.7.32`. A manual dispatch must
select that tag, after the official v0.7.32 release exists and its source commit
is pinned in the workflow. Website files support GitHub Pages from `main` root.
