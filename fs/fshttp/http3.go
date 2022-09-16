// Package fshttp contains the common http parts of the config, Transport and Client
package fshttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// QUICTransport is our QUIC Transport which wraps a http3.Transport
// * Sets the User Agent
// * Does logging
// * Updates metrics
type QUICTransport struct {
	Transport     *http3.RoundTripper
	dump          fs.DumpFlags
	filterRequest func(req *http.Request)
	userAgent     string
	headers       []*fs.HTTPOption
	metrics       *Metrics
}

// newQUICTransport wraps the http3.Transport passed in and logs all
// round-trips including the body if logBody is set.
func newQUICTransport(ci *fs.ConfigInfo, transport *http3.RoundTripper) *QUICTransport {
	return &QUICTransport{
		Transport: transport,
		dump:      ci.Dump,
		userAgent: ci.UserAgent,
		headers:   ci.Headers,
		metrics:   DefaultMetrics,
	}
}

// NewQUICTransportCustom returns a http3.RoundTripper with the correct timeouts.
// The customize function is called if set to give the caller an opportunity to
// customize any defaults in the Transport.
func NewQUICTransportCustom(ctx context.Context, _ func(tripper *http.RoundTripper)) http.RoundTripper {
	t := new(http3.RoundTripper)
	t.QuicConfig = &quic.Config{}

	ci := fs.GetConfig(ctx)

	// Unsupported: https://github.com/lucas-clemente/quic-go/issues/3370
	//t.Proxy = http.ProxyFromEnvironment

	t.QuicConfig.MaxIncomingUniStreams = int64(2 * (ci.Checkers + ci.Transfers + 1))
	t.QuicConfig.MaxIncomingStreams = 2 * t.QuicConfig.MaxIncomingUniStreams

	t.QuicConfig.HandshakeIdleTimeout = ci.ConnectTimeout

	// Unsupported
	//t.ResponseHeaderTimeout = ci.Timeout
	//t.ExpectContinueTimeout = ci.ExpectContinueTimeout

	t.QuicConfig.MaxIdleTimeout = 60 * time.Second
	if ci.Timeout > 60*time.Second {
		t.QuicConfig.MaxIdleTimeout = ci.Timeout
	}

	if ci.DisableHTTPKeepAlives {
		t.QuicConfig.KeepAlivePeriod = 0
	}

	// TLS Config
	t.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: ci.InsecureSkipVerify,
	}

	// Load client certs
	if ci.ClientCert != "" || ci.ClientKey != "" {
		if ci.ClientCert == "" || ci.ClientKey == "" {
			log.Fatalf("Both --client-cert and --client-key must be set")
		}
		cert, err := tls.LoadX509KeyPair(ci.ClientCert, ci.ClientKey)
		if err != nil {
			log.Fatalf("Failed to load --client-cert/--client-key pair: %v", err)
		}
		t.TLSClientConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA cert
	if ci.CaCert != "" {
		caCert, err := ioutil.ReadFile(ci.CaCert)
		if err != nil {
			log.Fatalf("Failed to read --ca-cert: %v", err)
		}
		caCertPool := x509.NewCertPool()
		ok := caCertPool.AppendCertsFromPEM(caCert)
		if !ok {
			log.Fatalf("Failed to add certificates from --ca-cert")
		}
		t.TLSClientConfig.RootCAs = caCertPool
	}

	t.DisableCompression = ci.NoGzip
	t.Dial = func(ctx context.Context, addr string, tlsCfg *tls.Config, quicCfg *quic.Config) (quic.EarlyConnection, error) {
		return quic.DialAddrEarlyContext(ctx, addr, tlsCfg, quicCfg)
	}

	//t.QuicConfig.EnableDatagrams = true

	if ci.Dump&(fs.DumpHeaders|fs.DumpBodies|fs.DumpAuth|fs.DumpRequests|fs.DumpResponses) != 0 {
		fs.Debugf(nil, "You have specified to dump information. Please be noted that the "+
			"Accept-Encoding as shown may not be correct in the request and the response may not show "+
			"Content-Encoding if the go standard libraries auto gzip encoding was in effect. In this case"+
			" the body of the request will be gunzipped before showing it.")
	}

	//// customize the transport if required
	//if customize != nil {
	//	customize(t.(*http.RoundTripper))
	//}

	// Wrap that http3.RoundTripper in our own transport
	return newQUICTransport(ci, t)
}

// RoundTrip implements the RoundTripper interface.
func (t *QUICTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	// Limit transactions per second if required
	accounting.LimitTPS(req.Context())
	// Force user agent
	req.Header.Set("User-Agent", t.userAgent)
	// Set user defined headers
	for _, option := range t.headers {
		req.Header.Set(option.Key, option.Value)
	}
	// Filter the request if required
	if t.filterRequest != nil {
		t.filterRequest(req)
	}
	// Logf request
	if t.dump&(fs.DumpHeaders|fs.DumpBodies|fs.DumpAuth|fs.DumpRequests|fs.DumpResponses) != 0 {
		buf, _ := httputil.DumpRequestOut(req, t.dump&(fs.DumpBodies|fs.DumpRequests) != 0)
		if t.dump&fs.DumpAuth == 0 {
			buf = cleanAuths(buf)
		}
		logMutex.Lock()
		fs.Debugf(nil, "%s", separatorReq)
		fs.Debugf(nil, "%s (req %p)", "HTTP REQUEST", req)
		fs.Debugf(nil, "%s", string(buf))
		fs.Debugf(nil, "%s", separatorReq)
		logMutex.Unlock()
	}
	// Do round trip
	resp, err = t.Transport.RoundTrip(req)
	// Logf response
	if t.dump&(fs.DumpHeaders|fs.DumpBodies|fs.DumpAuth|fs.DumpRequests|fs.DumpResponses) != 0 {
		logMutex.Lock()
		fs.Debugf(nil, "%s", separatorResp)
		fs.Debugf(nil, "%s (req %p)", "HTTP RESPONSE", req)
		if err != nil {
			fs.Debugf(nil, "Error: %v", err)
		} else {
			buf, _ := httputil.DumpResponse(resp, t.dump&(fs.DumpBodies|fs.DumpResponses) != 0)
			fs.Debugf(nil, "%s", string(buf))
		}
		fs.Debugf(nil, "%s", separatorResp)
		logMutex.Unlock()
	}
	// Update metrics
	t.metrics.onResponse(req, resp)

	if err == nil {
		checkServerTime(req, resp)
	}
	return resp, err
}

// NewQUICTransport returns a http3.RoundTripper with the correct timeouts
func NewQUICTransport(ctx context.Context) http.RoundTripper {
	(*noTransport).Do(func() {
		transport = NewQUICTransportCustom(ctx, nil)
	})
	return transport
}
