The updater embeds Sigstore's public-good trusted root from
[sigstore/root-signing commit c9bda74ad2221f938f7d2e0295ca3aad2da710a8](https://github.com/sigstore/root-signing/blob/c9bda74ad2221f938f7d2e0295ca3aad2da710a8/targets/trusted_root.json).
The imported file's SHA-256 is
`6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66`.

Verification is offline: downloaded attestations cannot add roots or relax the
repository, workflow, issuer, or tag policy. Root rotations require a reviewed
source change and release. The GitHub release workflow verifies its fresh bundle
with this exact policy before publishing, catching incompatible signing-service
changes before clients receive a release they cannot verify.

The root metadata is published by The Sigstore Authors under the
[Apache 2.0 license](https://github.com/sigstore/root-signing/blob/main/LICENSE).
