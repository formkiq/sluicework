package clusterconfig

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sluicework/internal/state"
)

type certificate = tls.Certificate

func networkURL(n Network) string {
	return "https://" + net.JoinHostPort(n.Hostname, strconv.Itoa(n.Port))
}
func fingerprint(b []byte) string {
	p, _ := pem.Decode(b)
	if p == nil {
		return ""
	}
	sum := sha256.Sum256(p.Bytes)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}
func validHostname(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsPrivate() || ip.IsLoopback()
	}
	if len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, "/:@\\\r\n ") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func (m *Manager) StartNetwork(handler http.Handler) error {
	m.write.Lock()
	defer m.write.Unlock()
	m.handler = handler
	if !m.renewStarted {
		m.renewStarted = true
		go m.renewCertificates()
	}
	cfg := m.current()
	if !cfg.Network.Enabled {
		return nil
	}
	cert, err := loadCertificate(cfg.Network)
	if err == nil {
		if time.Until(cert.Leaf.NotAfter) < 30*24*time.Hour {
			cfg.Network, err = m.generateCertificate(cfg.Network)
			if err == nil {
				err = m.store("system certificate renewal", cfg)
			}
			if err == nil {
				cert, err = loadCertificate(cfg.Network)
			}
		}
		if err == nil {
			err = m.startNetwork(cfg.Network, &cert)
		}
	}
	if err != nil {
		m.mu.Lock()
		m.networkIssue = "HTTPS listener could not start. Check the selected port and re-save network setup."
		m.mu.Unlock()
	}
	return err
}
func (m *Manager) startNetwork(n Network, cert *tls.Certificate) error {
	listener, err := listenNetwork(":" + strconv.Itoa(n.Port))
	if err != nil {
		return err
	}
	m.certificate.Store(cert)
	server := &http.Server{Handler: m.handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return m.certificate.Load(), nil }}}
	m.networkServer = server
	go m.serveNetwork(server, listener)

	return nil
}
func (m *Manager) SetNetwork(actor string, in NetworkInput) error {
	m.write.Lock()
	defer m.write.Unlock()
	return m.setNetwork(actor, in)
}
func (m *Manager) setNetwork(actor string, in NetworkInput) error {
	if m.closed.Load() {
		return state.ErrConflict
	}
	if m.handler == nil {
		return fmt.Errorf("%w: network setup is not ready", state.ErrConflict)
	}
	cfg := m.current()
	old := cfg.Network
	if !in.Enabled {
		cfg.Network.Enabled = false
		if err := m.store(actor, cfg); err != nil {
			return err
		}
		previous := m.networkServer
		m.networkServer = nil
		if previous != nil {
			go shutdownNetwork(previous)
		}
		m.mu.Lock()
		m.networkIssue = ""
		m.mu.Unlock()
		return nil
	}
	if !validHostname(in.Hostname) || in.Port < 1024 || in.Port > 65535 {
		return fmt.Errorf("%w: provide a local DNS hostname or private IP and a port from 1024 to 65535", state.ErrInvalid)
	}
	next := old
	next.Enabled = true
	next.Hostname = in.Hostname
	next.Port = in.Port
	if in.RotateCA {
		next.CAFile, next.CAKeyFile, next.CertFile, next.KeyFile = "", "", "", ""
	}
	var err error
	if next.CertFile == "" || next.Hostname != old.Hostname {
		next, err = m.generateCertificate(next)
		if err != nil {
			return err
		}
	}
	cert, err := loadCertificate(next)
	if err != nil {
		return errors.New("network certificate is unavailable")
	}
	if cert.Leaf == nil {
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return err
		}
	}
	if err = cert.Leaf.VerifyHostname(in.Hostname); err != nil {
		return fmt.Errorf("%w: certificate does not match the network hostname", state.ErrInvalid)
	}
	if time.Until(cert.Leaf.NotAfter) < 30*24*time.Hour {
		next, err = m.generateCertificate(next)
		if err != nil {
			return err
		}
		cert, err = loadCertificate(next)
		if err != nil {
			return err
		}
	}
	// Reserve the new port before committing. A port conflict keeps the current
	// listener and persisted settings untouched.
	var listener net.Listener
	if m.networkServer == nil || old.Port != next.Port {
		listener, err = listenNetwork(":" + strconv.Itoa(next.Port))
		if err != nil {
			return fmt.Errorf("%w: this HTTPS port is unavailable; choose another port", state.ErrConflict)
		}
	}
	cfg.Network = next
	if err = m.store(actor, cfg); err != nil {
		if listener != nil {
			listener.Close()
		}
		return err
	}
	m.certificate.Store(&cert)
	if listener != nil {
		previous := m.networkServer
		server := &http.Server{Handler: m.handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return m.certificate.Load(), nil }}}
		m.networkServer = server
		go m.serveNetwork(server, listener)

		if previous != nil {
			go shutdownNetwork(previous)
		}
	}
	m.mu.Lock()
	m.networkIssue = ""
	m.mu.Unlock()
	return nil
}
func shutdownNetwork(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if server.Shutdown(ctx) != nil {
		server.Close()
	}
}
func (m *Manager) Close() {
	m.closeOnce.Do(func() { m.closed.Store(true); close(m.renewStop) })
	m.write.Lock()
	server := m.networkServer
	m.networkServer = nil
	m.write.Unlock()
	if server != nil {
		shutdownNetwork(server)
	}
}
func serial() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
func (m *Manager) generateCertificate(n Network) (Network, error) {
	now := time.Now()
	var ca *x509.Certificate
	var caKey *rsa.PrivateKey
	if n.CAFile != "" && n.CAKeyFile != "" {
		b, err := os.ReadFile(n.CAFile)
		if err != nil {
			return n, err
		}
		block, _ := pem.Decode(b)
		if block == nil {
			return n, errors.New("invalid cluster CA")
		}
		ca, err = x509.ParseCertificate(block.Bytes)
		if err != nil {
			return n, err
		}
		b, err = os.ReadFile(n.CAKeyFile)
		if err != nil {
			return n, err
		}
		block, _ = pem.Decode(b)
		if block == nil {
			return n, errors.New("invalid cluster CA key")
		}
		caKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return n, err
		}
	} else {
		var err error
		caKey, err = rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return n, err
		}
		id, err := serial()
		if err != nil {
			return n, err
		}
		template := &x509.Certificate{SerialNumber: id, Subject: pkix.Name{CommonName: "Sluicework cluster CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
		if err != nil {
			return n, err
		}
		ca, err = x509.ParseCertificate(der)
		if err != nil {
			return n, err
		}
		n.CAKeyFile, err = m.privateFile("ca-key-", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}))
		if err != nil {
			return n, err
		}
		n.CAFile, err = m.privateFile("ca-public-", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		if err != nil {
			return n, err
		}
	}
	if time.Until(ca.NotAfter) < 30*24*time.Hour {
		return n, fmt.Errorf("%w: the cluster CA is expiring; explicitly replace it in Network access and update worker trust", state.ErrInvalid)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return n, err
	}
	id, err := serial()
	if err != nil {
		return n, err
	}
	leaf := &x509.Certificate{SerialNumber: id, Subject: pkix.Name{CommonName: n.Hostname}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(n.Hostname); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = []string{n.Hostname}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return n, err
	}
	n.KeyFile, err = m.privateFile("server-key-", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		return n, err
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})...)
	n.CertFile, err = m.privateFile("server-cert-", chain)
	return n, err
}

// An additional listener must not share its port with an unrelated loopback
// service (BSD otherwise permits overlapping SO_REUSEADDR listeners).
func listenNetwork(address string) (net.Listener, error) {
	if runtime.GOOS != "darwin" {
		return net.Listen("tcp", address)
	}
	config := net.ListenConfig{Control: func(_, _ string, connection syscall.RawConn) error {
		var socketError error
		if err := connection.Control(func(fd uintptr) {
			socketError = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
		}); err != nil {
			return err
		}
		return socketError
	}}
	return config.Listen(context.Background(), "tcp", address)
}

func (m *Manager) serveNetwork(server *http.Server, listener net.Listener) {
	if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		m.write.Lock()
		defer m.write.Unlock()
		if m.networkServer == server {
			m.networkServer = nil
			m.mu.Lock()
			m.networkIssue = "HTTPS listener stopped unexpectedly. Re-save network setup to retry."
			m.mu.Unlock()
		}
	}
}

func loadCertificate(n Network) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(n.CertFile, n.KeyFile)
	if err == nil && cert.Leaf == nil {
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	}
	return cert, err
}
func (m *Manager) renewCertificates() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-m.renewStop:
			return
		case <-ticker.C:
		}
		m.renewCertificateIfNeeded()
	}
}
func (m *Manager) renewCertificateIfNeeded() {
	m.write.Lock()
	defer m.write.Unlock()
	cfg := m.current()
	cert := m.certificate.Load()
	if !m.closed.Load() && cfg.Network.Enabled && cert != nil && cert.Leaf != nil && time.Until(cert.Leaf.NotAfter) < 30*24*time.Hour {
		err := m.setNetwork("system certificate renewal", NetworkInput{Enabled: true, Hostname: cfg.Network.Hostname, Port: cfg.Network.Port})
		if err != nil {
			m.mu.Lock()
			m.networkIssue = "Certificate renewal needs attention in Network access. Check the cluster CA and re-save the configuration."
			m.mu.Unlock()
		}
	}
}
