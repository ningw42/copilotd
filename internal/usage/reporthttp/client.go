package reporthttp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
)

type Client struct {
	endpoint *url.URL
	http     *http.Client
}

// Result retains the original bounded bytes alongside the validated report.
type Result struct {
	Report report.Report
	JSON   []byte
}

func NewClient(endpoint string) (*Client, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Opaque != "" || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.ForceQuery || base.RawQuery != "" || strings.Contains(endpoint, "#") {
		return nil, fmt.Errorf("report endpoint must be an absolute HTTP(S) base URL without userinfo, query, or fragment")
	}
	if strings.HasSuffix(base.Host, ":") {
		return nil, fmt.Errorf("invalid report endpoint port")
	}
	if port := base.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid report endpoint port")
		}
	}
	base.RawPath = strings.TrimRight(base.EscapedPath(), "/") + Path
	base.Path, _ = url.PathUnescape(base.RawPath)
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, DisableKeepAlives: true, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	// Fresh HTTP/1 connections avoid net/http's stale-connection retry. Disable
	// HTTP/2 negotiation too: REFUSED_STREAM transparently retries even on fresh
	// connections. Ordinary verified TLS/proxies remain enabled. No CookieJar,
	// credentials, redirect following, or automatic request replay.
	return &Client{endpoint: base, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Query(ctx context.Context, q report.Query) (Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	endpoint := *c.endpoint
	values := url.Values{}
	for name, value := range map[string]string{"period": q.Period, "since": q.Since, "until": q.Until, "timezone": q.Timezone, "surface": q.Surface} {
		if value != "" {
			values.Set(name, value)
		}
	}
	if q.Model != nil {
		values.Set("model", *q.Model)
	}
	endpoint.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(ctx, "GET", endpoint.String(), nil)
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("report request failed: %s", diagnostic(err.Error()))
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return Result{}, formatResponseFailure(response, "report response read failed or was canceled")
		}
		return Result{}, fmt.Errorf("report response read failed or was canceled")
	}
	if len(body) > MaxBodyBytes {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return Result{}, formatResponseFailure(response, "report response exceeds 8 MiB")
		}
		return Result{}, fmt.Errorf("report response exceeds 8 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, responseError(ctx, response, body)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return Result{}, errProtocol
	}
	result, err := decodeReport(ctx, body, q)
	if err != nil {
		return Result{}, err
	}
	return Result{Report: result, JSON: body}, nil
}

func diagnostic(value string) string {
	if len(value) > 256 {
		value = value[:256] + "..."
	}
	return strconv.QuoteToASCII(value)
}
func responseError(ctx context.Context, response *http.Response, body []byte) error {
	trustedDetail := ""
	if unambiguousJSON(ctx, body) == nil {
		var root map[string]json.RawMessage
		if json.Unmarshal(body, &root) == nil {
			d := wireDecoder{ctx: ctx}
			version := d.integer(root, "schema_version")
			problem := d.object(d.member(root, "error"))
			code := d.text(problem, "code")
			detail := d.text(problem, "message")
			if d.err == nil && version == 1 {
				trustedDetail = diagnostic(code) + " " + diagnostic(detail)
			}
		}
	}
	return formatResponseFailure(response, trustedDetail)
}

func formatResponseFailure(response *http.Response, trustedDetail string) error {
	message := fmt.Sprintf("report HTTP status %d", response.StatusCode)
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		message += " (redirect refused; check the endpoint base URL or proxy)"
	}
	if trustedDetail != "" {
		message += ": " + trustedDetail
	}
	if id := response.Header.Get("X-Request-Id"); id != "" {
		message += " (request ID " + diagnostic(id) + ")"
	}
	return fmt.Errorf("%s", message)
}
