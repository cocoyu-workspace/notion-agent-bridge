package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"notion-manager/internal/netutil"
)

// Chrome TLS transport using uTLS to mimic Chrome's JA3/JA4 fingerprint.
// Uses http2.Transport for proper HTTP/2 support with custom TLS dial.
//
// dialChromeTLS reads AppConfig.Proxy.NotionProxy at dial time, so
// updating the global proxy via /admin/settings takes effect on the next
// connection without rebuilding the singleton. Idle pooled connections
// are torn down by RebuildChromeTransport so a flipped setting doesn't
// leak across the boundary.
const (
	chromeH2ReadIdleTimeout  = 10 * time.Second
	chromeH2PingTimeout      = 5 * time.Second
	chromeH2WriteByteTimeout = 10 * time.Second
)

type chromeDynamicRoundTripper struct{}

func (chromeDynamicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return currentChromeH2Transport().RoundTrip(req)
}

func (chromeDynamicRoundTripper) CloseIdleConnections() {
	currentChromeH2Transport().CloseIdleConnections()
}

var (
	chromeRoundTripperMu sync.RWMutex
	chromeRoundTripperH2 *http2.Transport
	chromeRoundTripper   http.RoundTripper = chromeDynamicRoundTripper{}
)

func newChromeH2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return dialChromeTLS(ctx, network, addr)
		},
		DisableCompression: true, // We set Accept-Encoding ourselves
		ReadIdleTimeout:    chromeH2ReadIdleTimeout,
		PingTimeout:        chromeH2PingTimeout,
		WriteByteTimeout:   chromeH2WriteByteTimeout,
	}
}

func currentChromeH2Transport() *http2.Transport {
	chromeRoundTripperMu.RLock()
	h2 := chromeRoundTripperH2
	chromeRoundTripperMu.RUnlock()
	if h2 != nil {
		return h2
	}
	chromeRoundTripperMu.Lock()
	if chromeRoundTripperH2 == nil {
		chromeRoundTripperH2 = newChromeH2Transport()
	}
	h2 = chromeRoundTripperH2
	chromeRoundTripperMu.Unlock()
	return h2
}

func getChromeRoundTripper() http.RoundTripper {
	currentChromeH2Transport()
	return chromeRoundTripper
}

// RebuildChromeTransport swaps the active http2.Transport instance and closes
// idle connections on the retired pool. Replacing the transport instance
// guarantees that subsequent requests — including hedged retries from an
// existing http.Client while an older stream is still wedged in flight — dial
// a fresh connection instead of multiplexing onto a connection whose
// CloseIdleConnections call was ignored due to active streams.
func RebuildChromeTransport() {
	chromeRoundTripperMu.Lock()
	old := chromeRoundTripperH2
	chromeRoundTripperH2 = newChromeH2Transport()
	chromeRoundTripperMu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

func dialChromeTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	// Parse host for SNI
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Honour the configured TLS dial timeout via a context deadline
	// before delegating to the shared proxy-aware dialer. The dialer
	// already enforces its own 30s connect timeout, but the project's
	// AppConfig.Timeouts.TLSDialTimeout governs the overall budget
	// (raw TCP + TLS handshake) so the caller's expectations hold
	// regardless of which path we take.
	dialCtx := ctx
	if to := AppConfig.TLSDialTimeoutDuration(); to > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	rawConn, err := netutil.DialThroughProxy(dialCtx, network, addr, AppConfig.NotionProxyURL())
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}

	// Create uTLS connection with Chrome fingerprint + ALPN h2
	tlsConfig := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	tlsConn := utls.UClient(rawConn, tlsConfig, utls.HelloChrome_Auto)

	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}

	return tlsConn, nil
}

// getChromeHTTPClient returns an http.Client with Chrome TLS fingerprint
func getChromeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: getChromeRoundTripper(),
		Timeout:   timeout,
	}
}
