package fshttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/imroc/req/v3"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// ReqTransport is our req Transport which wraps a req.Transport
// * Sets the User Agent
// * Does logging
// * Updates metrics
type ReqTransport struct {
	Transport     *req.Transport
	dump          fs.DumpFlags
	filterRequest func(req *http.Request)
	userAgent     string
	headers       []*fs.HTTPOption
	metrics       *Metrics
}

// newReqTransport wraps the http3.Transport passed in and logs all
// round-trips including the body if logBody is set.
func newReqTransport(ci *fs.ConfigInfo, transport *req.Transport) *ReqTransport {
	return &ReqTransport{
		Transport: transport,
		dump:      ci.Dump,
		userAgent: ci.UserAgent,
		headers:   ci.Headers,
		metrics:   DefaultMetrics,
	}
}

// NewReqTransportCustom returns a req.Transport with the correct timeouts.
// The customize function is called if set to give the caller an opportunity to
// customize any defaults in the Transport.
func NewReqTransportCustom(ctx context.Context, _ func(tripper *http.RoundTripper)) http.RoundTripper {
	c := req.C().EnableHTTP3().DisableAutoDecode().DisableAutoReadResponse()
	//c.EnableDebugLog()

	t := c.GetTransport()

	ci := fs.GetConfig(ctx)

	// Unsupported: https://github.com/lucas-clemente/quic-go/issues/3370
	//t.Proxy = http.ProxyFromEnvironment

	t.Proxy = http.ProxyFromEnvironment
	t.MaxIdleConnsPerHost = 2 * (ci.Checkers + ci.Transfers + 1)
	t.MaxIdleConns = 2 * t.MaxIdleConnsPerHost
	t.TLSHandshakeTimeout = ci.ConnectTimeout
	t.ResponseHeaderTimeout = ci.Timeout
	t.DisableKeepAlives = ci.DisableHTTPKeepAlives

	// TLS Config
	t.TLSClientConfig.InsecureSkipVerify = ci.InsecureSkipVerify

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
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialContext(ctx, network, addr, ci)
	}
	t.IdleConnTimeout = 60 * time.Second
	t.ExpectContinueTimeout = ci.ExpectContinueTimeout

	if ci.Dump&(fs.DumpHeaders|fs.DumpBodies|fs.DumpAuth|fs.DumpRequests|fs.DumpResponses) != 0 {
		fs.Debugf(nil, "You have specified to dump information. Please be noted that the "+
			"Accept-Encoding as shown may not be correct in the request and the response may not show "+
			"Content-Encoding if the go standard libraries auto gzip encoding was in effect. In this case"+
			" the body of the request will be gunzipped before showing it.")
	}

	// TODO: This doesn't work to disable HTTP2
	if ci.DisableHTTP2 {
		t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}

	//// customize the transport if required
	//if customize != nil {
	//	customize(t)
	//}

	// Wrap that req.Transport in our own transport
	return newReqTransport(ci, t)
}

// RoundTrip implements the RoundTripper interface.
func (t *ReqTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
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

// NewReqTransport returns a http3.RoundTripper with the correct timeouts
func NewReqTransport(ctx context.Context) http.RoundTripper {
	(*noTransport).Do(func() {
		transport = NewReqTransportCustom(ctx, nil)
	})
	return transport
}
