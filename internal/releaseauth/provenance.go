// Package releaseauth verifies release manifests against a compiled-in Sigstore
// trust root. Neither release metadata nor downloaded bundles can add trust roots.
package releaseauth

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

//go:embed trusted_root.json
var trustedRootJSON []byte

const BundleName = "SHA256SUMS.sigstore.json"
const githubIssuer = "https://token.actions.githubusercontent.com"

var repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var tagRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func releaseIdentity(repo, tag string) (verify.CertificateIdentity, error) {
	if !repositoryRE.MatchString(repo) || !tagRE.MatchString(tag) {
		return verify.CertificateIdentity{}, errors.New("invalid release provenance repository or tag")
	}
	identity, err := verify.NewShortCertificateIdentity(githubIssuer, "", "https://github.com/"+repo+"/.github/workflows/release.yml@refs/tags/"+tag, "")
	// Bind the source as well as the signer, including if the workflow ever
	// becomes reusable by another repository or another ref.
	identity.SourceRepositoryURI = "https://github.com/" + repo
	identity.SourceRepositoryRef = "refs/tags/" + tag
	return identity, err
}

// Verify requires a GitHub Actions build attestation for the exact repository,
// release workflow and tag, covering the complete checksum manifest. Validation
// is offline, including the certificate chain, SCT and transparency-log proof.
func Verify(repo, tag string, manifest, encoded []byte) error {
	identity, err := releaseIdentity(repo, tag)
	if err != nil {
		return err
	}
	trusted, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		return fmt.Errorf("invalid embedded release trust root: %w", err)
	}
	return verifyBundle(trusted, identity, manifest, encoded)
}

func verifyBundle(trusted root.TrustedMaterial, identity verify.CertificateIdentity, manifest, encoded []byte) error {
	if len(manifest) == 0 || len(manifest) > 1<<20 || len(encoded) == 0 || len(encoded) > 4<<20 {
		return errors.New("missing or oversized release provenance")
	}
	return verifyAttestation(trusted, identity, verify.WithArtifact(bytes.NewReader(manifest)), encoded)
}

func verifyAttestation(trusted root.TrustedMaterial, identity verify.CertificateIdentity, artifact verify.ArtifactPolicyOption, encoded []byte) error {
	var signed bundle.Bundle
	if err := json.Unmarshal(encoded, &signed); err != nil {
		return fmt.Errorf("invalid release provenance bundle: %w", err)
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	result, err := verifier.Verify(&signed, verify.NewPolicy(artifact, verify.WithCertificateIdentity(identity)))
	if err != nil {
		return fmt.Errorf("release provenance verification failed: %w", err)
	}
	if result.Statement == nil || result.Statement.PredicateType != "https://slsa.dev/provenance/v1" {
		return errors.New("release does not have a SLSA v1 build provenance statement")
	}
	return nil
}
