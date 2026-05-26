package agent

import (
	"crypto/tls"
	"net/http"
)

// newInsecureClient is used only when --insecure is set, for local testing
// against a server with a self-signed certificate.
func newInsecureClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}
