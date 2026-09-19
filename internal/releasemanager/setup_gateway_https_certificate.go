package releasemanager

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/iaia/telegramgw/internal/config"
)

func gatewayHTTPSCertificatePaths(paths gatewayHTTPSPaths) (string, string) {
	directory := filepath.Join(filepath.Dir(paths.Config), "tls")
	return filepath.Join(directory, "fullchain.pem"), filepath.Join(directory, "privkey.pem")
}

func readGatewayHTTPSCertificate(options gatewayHTTPSOptions, hostname string, roots *x509.CertPool) ([]byte, []byte, error) {
	read := func(path string) ([]byte, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return nil, errors.New("certificate inputs must be regular files smaller than 1 MiB")
		}
		return io.ReadAll(io.LimitReader(file, 1024*1024+1))
	}
	certificate, err := read(options.CertificateFile)
	if err != nil {
		return nil, nil, errors.New("could not read the public certificate chain")
	}
	key, err := read(options.KeyFile)
	if err != nil {
		return nil, nil, errors.New("could not read the certificate private key")
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, nil, errors.New("the PEM certificate and private key do not form a valid matching pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, errors.New("could not parse the public certificate")
	}
	intermediates := x509.NewCertPool()
	for _, encoded := range pair.Certificate[1:] {
		intermediate, err := x509.ParseCertificate(encoded)
		if err != nil {
			return nil, nil, errors.New("could not parse the certificate chain")
		}
		intermediates.AddCert(intermediate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: hostname, Roots: roots, Intermediates: intermediates}); err != nil {
		return nil, nil, errors.New("the certificate must be currently valid, trusted by this system, and cover the gateway hostname; provide its full public certificate chain")
	}
	return certificate, key, nil
}

func (m *Manager) writeGatewayHTTPSCertificate(ctx context.Context, paths gatewayHTTPSPaths, certificate, key []byte) error {
	certificatePath, keyPath := gatewayHTTPSCertificatePaths(paths)
	directory := filepath.Dir(certificatePath)
	if FileExists(directory) {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed certificate directory is not a regular directory")
		}
	} else if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	// Keep the directory private until both files have their intended group.
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	for path, data := range map[string][]byte{certificatePath: certificate, keyPath: key} {
		if err := AtomicWrite(path, data, 0600, nil); err != nil {
			return err
		}
		if err := os.Chmod(path, 0640); err != nil {
			return err
		}
	}
	if _, err := m.command(ctx, "chown", "root:caddy", directory, certificatePath, keyPath); err != nil {
		return errors.New("could not grant the dedicated proxy access to its certificate files")
	}
	return os.Chmod(directory, 0750)
}

// RefreshGatewayHTTPS picks up certificates renewed at the source paths saved
// by setup. It affects only this installer's dedicated proxy service.
func (m *Manager) RefreshGatewayHTTPS(ctx context.Context) error {
	return m.refreshGatewayHTTPS(ctx, defaultGatewayHTTPSPaths())
}

func (m *Manager) refreshGatewayHTTPS(ctx context.Context, paths gatewayHTTPSPaths) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := os.ReadFile(paths.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, lockErr := Lock(paths.State + ".refresh.lock")
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	var state gatewayHTTPSState
	if err != nil || !privateRegularSetupFile(paths.State) || json.Unmarshal(data, &state) != nil || state.Phase != "ready" {
		return errors.New("managed HTTPS must finish setup before certificates can be refreshed")
	}
	if state.Options.CertificateFile == "" && state.Options.KeyFile == "" {
		return nil
	}
	origin, err := url.Parse(state.Origin)
	if err != nil || !gatewayStandaloneHTTPSOrigin(state.Origin) {
		return errors.New("invalid managed HTTPS origin")
	}
	cfg := config.GatewayConfig{PublicBaseURL: state.Origin, Listen: state.Listen}
	for path, expected := range map[string]string{paths.Config: gatewayManagedCaddyfile(cfg, paths, true, state.Options.TLSALPNOnly), paths.Unit: gatewayCaddyUnit(paths.Config)} {
		actual, err := os.ReadFile(path)
		if err != nil || !regularNoSymlink(path) || string(actual) != expected {
			return errors.New("managed HTTPS files have changed; refusing to refresh certificates or restart the proxy")
		}
	}
	certificate, key, err := readGatewayHTTPSCertificate(state.Options, origin.Hostname(), paths.CertificateRoots)
	if err != nil {
		return err
	}
	certificatePath, keyPath := gatewayHTTPSCertificatePaths(paths)
	previousCertificate, certErr := os.ReadFile(certificatePath)
	previousKey, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil || !regularNoSymlink(certificatePath) || !regularNoSymlink(keyPath) {
		return errors.New("could not read the managed certificate copies")
	}
	if bytes.Equal(certificate, previousCertificate) && bytes.Equal(key, previousKey) {
		return nil
	}
	rollback := func(cause error, restart bool) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := m.writeGatewayHTTPSCertificate(cleanupCtx, paths, previousCertificate, previousKey); err != nil {
			return errors.Join(cause, errors.New("could not restore the previous certificate copies; inspect the dedicated proxy's certificate files"))
		}
		if restart {
			if _, err := m.command(cleanupCtx, "systemctl", "restart", gatewayProxyService); err != nil {
				return errors.Join(cause, errors.New("the previous certificates were restored but the proxy could not restart"))
			}
		}
		return fmt.Errorf("%w; restored the previous certificate copies", cause)
	}
	if err := m.writeGatewayHTTPSCertificate(ctx, paths, certificate, key); err != nil {
		return rollback(err, false)
	}
	if _, err := m.command(ctx, "runuser", "-u", "caddy", "--", "/usr/bin/caddy", "validate", "--config", paths.Config, "--adapter", "caddyfile"); err != nil {
		return rollback(errors.New("renewed certificate validation failed"), false)
	}
	// admin off isolates this service from an existing Caddy API. Restarting
	// only this proxy is the supported way to load externally renewed files.
	if _, err := m.command(ctx, "systemctl", "restart", gatewayProxyService); err != nil {
		return rollback(errors.New("could not restart the proxy with its renewed certificate"), true)
	}
	fmt.Fprintln(m.Out, "Refreshed the gateway proxy's public certificate and restarted its dedicated HTTPS service.")
	return nil
}
