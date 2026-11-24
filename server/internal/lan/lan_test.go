package lan

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCertificateServesHTTPS(t *testing.T) {
	dir := t.TempDir()
	cert, err := Certificate(dir, []string{"192.168.1.20", "ambivox.lan"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "192.168.1.20", "ambivox.lan"} {
		if err := cert.Leaf.VerifyHostname(host); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	again, err := Certificate(dir, []string{"ambivox.lan"})
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(again) != Fingerprint(cert) {
		t.Errorf("certificate regenerated although it still covers every host")
	}
	moved, err := Certificate(dir, []string{"10.0.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(moved) == Fingerprint(cert) || moved.Leaf.VerifyHostname("10.0.0.7") != nil {
		t.Errorf("certificate not renewed for a new address")
	}
}
