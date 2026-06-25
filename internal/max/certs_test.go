package max

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestRussianTrustedCABundle(t *testing.T) {
	rest := []byte(russianTrustedCABundlePEM)
	var names []string
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			t.Fatalf("failed to decode certificate %d", len(names)+1)
		}
		if block.Type != "CERTIFICATE" {
			t.Fatalf("unexpected PEM block type %q", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parse certificate %d: %v", len(names)+1, err)
		}
		names = append(names, cert.Subject.CommonName)
	}
	want := []string{"Russian Trusted Root CA", "Russian Trusted Sub CA"}
	if len(names) != len(want) {
		t.Fatalf("got %d certificates, want %d", len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("certificate %d common name = %q, want %q", i+1, names[i], want[i])
		}
	}
	tr, ok := newMaxHTTPClient().Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("MAX HTTP client has no custom RootCAs")
	}
}

func TestLivePlatformAPI2TLS(t *testing.T) {
	if os.Getenv("KLAX_LIVE_MAX_TLS") != "1" {
		t.Skip("set KLAX_LIVE_MAX_TLS=1 to probe the live MAX endpoint")
	}
	_, err := New("invalid-token-for-tls-probe").GetMe()
	if err == nil {
		t.Fatal("GetMe with invalid token unexpectedly succeeded")
	}
	if strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Fatalf("TLS failed before MAX API response: %v", err)
	}
}
