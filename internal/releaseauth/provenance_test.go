package releaseauth

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// This real public Sigstore bundle exercises certificate, SCT, signature,
// offline transparency-log and digest verification without network access.
func TestPublicSignedAttestation(t *testing.T) {
	encoded, err := os.ReadFile("testdata/public.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := root.NewTrustedRootFromJSON(trustedRootJSON)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verify.NewShortCertificateIdentity(githubIssuer, "", "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main", "")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString("46d4e2f74c4877316640000a6fdf8a8b59f1e0847667973e9859f774dd31b8f1e0937813b777fb66a2ac67d50540fe34640966eee9fc2ccca387082b4c85cd3c")
	if err != nil {
		t.Fatal(err)
	}
	policy := verify.WithArtifactDigest("sha512", digest)
	if err := verifyAttestation(trusted, identity, policy, encoded); err != nil {
		t.Fatal(err)
	}
	t.Run("wrong digest", func(t *testing.T) {
		bad := append([]byte(nil), digest...)
		bad[0] ^= 1
		if err := verifyAttestation(trusted, identity, verify.WithArtifactDigest("sha512", bad), encoded); err == nil {
			t.Fatal("accepted changed artifact")
		}
	})
	for _, tc := range []struct{ name, issuer, san string }{
		{"wrong issuer", "https://issuer.invalid", identity.SubjectAlternativeName.SubjectAlternativeName},
		{"wrong repository", githubIssuer, "https://github.com/other/project/.github/workflows/release.yml@refs/heads/main"},
		{"wrong workflow", githubIssuer, "https://github.com/sigstore/sigstore-js/.github/workflows/ci.yml@refs/heads/main"},
		{"wrong ref", githubIssuer, "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/tags/v2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong, err := verify.NewShortCertificateIdentity(tc.issuer, "", tc.san, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyAttestation(trusted, wrong, policy, encoded); err == nil {
				t.Fatal("accepted wrong signer identity")
			}
		})
	}
	t.Run("missing log", func(t *testing.T) {
		var data map[string]any
		if err := json.Unmarshal(encoded, &data); err != nil {
			t.Fatal(err)
		}
		delete(data["verificationMaterial"].(map[string]any), "tlogEntries")
		changed, _ := json.Marshal(data)
		if err := verifyAttestation(trusted, identity, policy, changed); err == nil {
			t.Fatal("accepted missing transparency log")
		}
	})
}

func TestReleaseIdentityIsExact(t *testing.T) {
	id, err := releaseIdentity("example/project", "v1.2.3")
	if err != nil || id.Issuer.Issuer != githubIssuer || id.SubjectAlternativeName.SubjectAlternativeName != "https://github.com/example/project/.github/workflows/release.yml@refs/tags/v1.2.3" {
		t.Fatal(id, err)
	}
	if id.SourceRepositoryURI != "https://github.com/example/project" || id.SourceRepositoryRef != "refs/tags/v1.2.3" {
		t.Fatal("source identity is not pinned", id)
	}
	for _, tag := range []string{"main", "1.2.3", "v1.2.3-rc1", "v1.2.3/other", "v1.2.3\n"} {
		if _, err := releaseIdentity("example/project", tag); err == nil {
			t.Fatalf("accepted tag %q", tag)
		}
	}
	for _, encoded := range [][]byte{nil, []byte("{}"), []byte("garbage"), make([]byte, (4<<20)+1)} {
		if err := Verify("example/project", "v1.2.3", []byte("manifest"), encoded); err == nil {
			t.Fatal("accepted absent or invalid provenance")
		}
	}
}
