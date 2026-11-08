package gateway

import "net/http"

type uploadTransport struct{ uaTransport }

func (t uploadTransport) CloseIdleConnections() {
	if transport, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

// Hark's API needs a browser ClientHello, but third-party presigned storage
// must use standard TLS. Firefox uTLS fails on S3's P-256 key share. Preserve
// the account's proxy dialer, browser User-Agent and certificate validation.
func (s *Server) uploadClientFor(cookie string) *http.Client {
	client := s.clientFor(cookie, 0, false)
	transport := client.Transport.(uaTransport)
	if base, ok := transport.base.(*http.Transport); ok {
		storage := base.Clone()
		storage.DialTLSContext, storage.DialTLS = nil, nil
		transport.base = storage
		client.Transport = uploadTransport{transport}
	}
	return client
}
