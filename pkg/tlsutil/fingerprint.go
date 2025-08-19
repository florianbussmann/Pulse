package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FingerprintVerifier creates a custom TLS config that verifies server certificate fingerprint
func FingerprintVerifier(fingerprint string) *tls.Config {
	// Normalize fingerprint (remove colons, convert to lowercase)
	expectedFingerprint := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))

	return &tls.Config{
		InsecureSkipVerify: true, // We'll do our own verification
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no certificates presented by server")
			}

			// Calculate SHA256 fingerprint of the leaf certificate
			fingerprint := sha256.Sum256(rawCerts[0])
			actualFingerprint := hex.EncodeToString(fingerprint[:])

			if actualFingerprint != expectedFingerprint {
				return fmt.Errorf("certificate fingerprint mismatch: expected %s, got %s",
					expectedFingerprint, actualFingerprint)
			}

			return nil
		},
	}
}

// CreateHTTPClient creates an HTTP client with appropriate TLS configuration
func CreateHTTPClient(verifySSL bool, fingerprint string) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Performance optimizations for concurrent requests
		MaxIdleConns:        100, // Increase from default 2
		MaxIdleConnsPerHost: 20,  // Increase from default 2
		MaxConnsPerHost:     20,  // Limit concurrent connections per host
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // Disable compression for lower latency
	}

	if !verifySSL && fingerprint == "" {
		// Insecure mode - skip all verification
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} else if fingerprint != "" {
		// Fingerprint verification mode
		transport.TLSClientConfig = FingerprintVerifier(fingerprint)
	}
	// else: default secure mode with system CA verification

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}
