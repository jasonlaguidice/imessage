package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RelayInfo is written to relay-info.json so extract-key can read it.
type RelayInfo struct {
	Token           string `json:"token"`
	CertFingerprint string `json:"cert_fingerprint"` // SHA-256 hex of DER cert
}

const relayDirName = "nac-relay"

// relayDataDir returns ~/Library/Application Support/nac-relay/,
// creating it if necessary.
func relayDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "Application Support", relayDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureRelayAuth loads or generates the TLS cert and bearer token.
// Returns the tls.Config, the bearer token, and the relay-info path.
func ensureRelayAuth() (*tls.Config, string, error) {
	dir, err := relayDataDir()
	if err != nil {
		return nil, "", fmt.Errorf("relay data dir: %w", err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	tokenPath := filepath.Join(dir, "token")
	infoPath := filepath.Join(dir, "relay-info.json")

	// Generate token if missing
	token, err := loadOrGenerateToken(tokenPath)
	if err != nil {
		return nil, "", fmt.Errorf("token: %w", err)
	}

	// Generate self-signed cert if missing
	if !fileExists(certPath) || !fileExists(keyPath) {
		if err := generateSelfSignedCert(certPath, keyPath); err != nil {
			return nil, "", fmt.Errorf("generate cert: %w", err)
		}
	}

	// Load the cert
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("load cert: %w", err)
	}

	// Compute fingerprint
	fingerprint := certFingerprint(cert)

	// Write relay-info.json for extract-key to read
	info := RelayInfo{
		Token:           token,
		CertFingerprint: fingerprint,
	}
	infoJSON, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(infoPath, infoJSON, 0600); err != nil {
		return nil, "", fmt.Errorf("write relay-info.json: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	log.Printf("TLS cert fingerprint: %s", fingerprint)
	log.Printf("Auth token: %s...%s", token[:4], token[len(token)-4:])
	log.Printf("Relay info: %s", infoPath)

	return tlsConfig, token, nil
}

func loadOrGenerateToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(data))
		if len(t) >= 32 {
			return t, nil
		}
	}
	// Generate a 32-byte random token as hex
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return "", err
	}
	return token, nil
}

func generateSelfSignedCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	// Collect all local IPs for SAN
	var ips []net.IP
	ips = append(ips, net.IPv4(127, 0, 0, 1))
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP)
			}
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "nac-relay"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     []string{"localhost", "nac-relay"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return err
	}

	log.Println("Generated self-signed TLS certificate (valid 10 years)")
	return nil
}

func certFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	h := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(h[:])
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// authMiddleware wraps an http.Handler and requires a bearer token on all
// endpoints except /health.
func authMiddleware(token string, next http.Handler) http.Handler {
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow /health without auth for monitoring/probing
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
