package releasemanager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gatewayTestPublicCertificate(t *testing.T, paths *gatewayHTTPSPaths, hostname string, expired bool) gatewayHTTPSOptions {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "Installer test CA"}, NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	parsedCA, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano() + 1), DNSNames: []string{hostname}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if expired {
		leaf.NotBefore = now.Add(-48 * time.Hour)
		leaf.NotAfter = now.Add(-24 * time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, parsedCA, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	options := gatewayHTTPSOptions{CertificateFile: filepath.Join(root, "fullchain.pem"), KeyFile: filepath.Join(root, "key.pem")}
	platformWrite(t, options.CertificateFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), 0600)
	platformWrite(t, options.KeyFile, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})), 0600)
	if paths.CertificateRoots == nil {
		paths.CertificateRoots = x509.NewCertPool()
	}
	paths.CertificateRoots.AddCert(parsedCA)
	return options
}

func TestGatewayStandaloneCertificateUsesOnlyChosenPortAndProtectsCopies(t *testing.T) {
	for _, port := range []string{"8443", "80"} {
		t.Run(port, func(t *testing.T) {
			m, cfg, paths, commands := gatewayHTTPSFixture(t)
			cfg.PublicBaseURL = "https://gateway.example.com:" + port
			options := gatewayTestPublicCertificate(t, &paths, "gateway.example.com", false)
			paths.PortAvailable = func(candidate int) bool {
				return (candidate == 8443 && port == "8443") || (candidate == 80 && port == "80")
			}
			if err := m.setupGatewayHTTPSWithOptions(context.Background(), cfg, paths, options); err != nil {
				t.Fatal(err)
			}
			certificate, key := gatewayHTTPSCertificatePaths(paths)
			for _, path := range []string{certificate, key} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0640 {
					t.Fatalf("copy permissions: %v, %v", info, err)
				}
			}
			info, err := os.Stat(filepath.Dir(key))
			if err != nil || info.Mode().Perm() != 0750 {
				t.Fatal("certificate directory permissions", err)
			}
			data, _ := os.ReadFile(paths.Config)
			if port == "80" && !strings.Contains(string(data), "http_port 8081") {
				t.Fatal("HTTPS80 conflicts with Caddy's default plaintext HTTP port")
			}
			if !strings.Contains(string(data), "auto_https off") || strings.Contains(string(data), options.KeyFile) || !strings.Contains(string(data), key) {
				t.Fatal("proxy did not use dedicated certificate copies")
			}
			chowned, validatedAsUser := false, false
			for _, command := range *commands {
				if command[0] == "chown" && command[1] == "root:caddy" {
					chowned = true
				}
				if command[0] == "runuser" && command[2] == "caddy" {
					validatedAsUser = true
				}
			}
			if !chowned || !validatedAsUser {
				t.Fatal("certificate accessibility was not checked for service user")
			}
			// Sources may later become temporarily unavailable; resuming a completed
			// setup uses its own trusted copies and remembered certificate mode.
			if err := os.Remove(options.CertificateFile); err != nil {
				t.Fatal(err)
			}
			paths.PortAvailable = func(int) bool { return false }
			if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGatewayStandaloneCertificateRejectsBadCredentialsBeforeChangingHost(t *testing.T) {
	for _, name := range []string{"expired", "hostname", "untrusted", "mismatch", "missing-key"} {
		t.Run(name, func(t *testing.T) {
			m, cfg, paths, commands := gatewayHTTPSFixture(t)
			hostname := "gateway.example.com"
			if name == "hostname" {
				hostname = "other.example.com"
			}
			options := gatewayTestPublicCertificate(t, &paths, hostname, name == "expired")
			if name == "untrusted" {
				paths.CertificateRoots = x509.NewCertPool()
			}
			if name == "missing-key" {
				options.KeyFile = ""
			}
			if name == "mismatch" {
				other := gatewayTestPublicCertificate(t, &paths, hostname, false)
				options.KeyFile = other.KeyFile
			}
			if err := m.setupGatewayHTTPSWithOptions(context.Background(), cfg, paths, options); err == nil {
				t.Fatal("invalid certificate accepted")
			}
			if FileExists(paths.State) || len(*commands) != 0 {
				t.Fatal("invalid certificate changed machine")
			}
		})
	}
}

func TestGatewayCertificateRefreshLoadsRenewalAndOnlyRestartsDedicatedProxy(t *testing.T) {
	m, cfg, paths, commands := gatewayHTTPSFixture(t)
	options := gatewayTestPublicCertificate(t, &paths, "gateway.example.com", false)
	if err := m.setupGatewayHTTPSWithOptions(context.Background(), cfg, paths, options); err != nil {
		t.Fatal(err)
	}
	*commands = nil
	if err := m.refreshGatewayHTTPS(context.Background(), paths); err != nil {
		t.Fatal(err)
	}
	if len(*commands) != 0 {
		t.Fatal("unchanged certificate restarted service")
	}
	renewed := gatewayTestPublicCertificate(t, &paths, "gateway.example.com", false)
	cert, _ := os.ReadFile(renewed.CertificateFile)
	key, _ := os.ReadFile(renewed.KeyFile)
	platformWrite(t, options.CertificateFile, string(cert), 0600)
	platformWrite(t, options.KeyFile, string(key), 0600)
	if err := m.refreshGatewayHTTPS(context.Background(), paths); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := gatewayHTTPSCertificatePaths(paths)
	actualCert, _ := os.ReadFile(certPath)
	actualKey, _ := os.ReadFile(keyPath)
	if string(actualCert) != string(cert) || string(actualKey) != string(key) {
		t.Fatal("renewal did not replace both copies")
	}
	restarts := 0
	for _, command := range *commands {
		if command[0] == "systemctl" {
			if strings.Join(command, " ") != "systemctl restart "+gatewayProxyService {
				t.Fatalf("touched unrelated service: %v", command)
			}
			restarts++
		}
	}
	if restarts != 1 {
		t.Fatalf("restarts=%d", restarts)
	}
}

func TestGatewayCertificateRefreshRejectsModifiedProxyAndRollsBackFailures(t *testing.T) {
	for _, failure := range []string{"config", "unit", "chown", "validate", "restart", "invalid-renewal"} {
		t.Run(failure, func(t *testing.T) {
			m, cfg, paths, commands := gatewayHTTPSFixture(t)
			options := gatewayTestPublicCertificate(t, &paths, "gateway.example.com", false)
			if err := m.setupGatewayHTTPSWithOptions(context.Background(), cfg, paths, options); err != nil {
				t.Fatal(err)
			}
			certPath, keyPath := gatewayHTTPSCertificatePaths(paths)
			beforeCert, _ := os.ReadFile(certPath)
			beforeKey, _ := os.ReadFile(keyPath)
			renewal := gatewayTestPublicCertificate(t, &paths, "gateway.example.com", failure == "invalid-renewal")
			cert, _ := os.ReadFile(renewal.CertificateFile)
			key, _ := os.ReadFile(renewal.KeyFile)
			platformWrite(t, options.CertificateFile, string(cert), 0600)
			platformWrite(t, options.KeyFile, string(key), 0600)
			if failure == "config" {
				platformWrite(t, paths.Config, "operator modified proxy", 0644)
			}
			if failure == "unit" {
				platformWrite(t, paths.Unit, "operator modified unit", 0644)
			}
			*commands = nil
			run := m.Run
			failed := false
			m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if !failed && ((failure == "chown" && args[0] == "chown") || (failure == "validate" && args[0] == "runuser") || (failure == "restart" && args[0] == "systemctl" && args[1] == "restart")) {
					failed = true
					return CommandResult{}, errors.New("injected failure")
				}
				return run(ctx, args...)
			}
			if err := m.refreshGatewayHTTPS(context.Background(), paths); err == nil {
				t.Fatal("refresh ignored failure")
			}
			afterCert, _ := os.ReadFile(certPath)
			afterKey, _ := os.ReadFile(keyPath)
			if string(afterCert) != string(beforeCert) || string(afterKey) != string(beforeKey) {
				t.Fatal("failed renewal changed active certificate pair")
			}
			if (failure == "config" || failure == "unit" || failure == "invalid-renewal") && len(*commands) != 0 {
				t.Fatal("rejected refresh changed host")
			}
			info, err := os.Stat(filepath.Dir(keyPath))
			if err != nil || info.Mode().Perm() != 0750 {
				t.Fatal("rollback left inaccessible certificate directory")
			}
		})
	}
}
