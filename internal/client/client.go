// SPDX-License-Identifier: Apache-2.0

// Package client is the reusable Liftr public HTTP API client. It speaks
// only the versioned /v1 contract documented in docs/openapi/v1/openapi.yaml:
// bearer credentials from the caller, mandatory idempotency keys and concrete
// generation preconditions on mutations, RFC 9457 problems, bounded response
// bodies, refused redirects, and same-origin enforcement of every
// server-supplied reference. It knows nothing about terminals, flags, or exit
// codes, and it imports nothing from the Liftr server implementation.
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxTokenBytes mirrors the server's explicit ceiling on one bearer
// credential so oversized credentials fail locally before any transmission.
const MaxTokenBytes = 8 * 1024

// maxResponseBytes bounds every response body read by the client.
const maxResponseBytes = 4 << 20

const (
	readAttempts   = 3
	mutateAttempts = 3
	backoffBase    = 250 * time.Millisecond
	backoffMax     = 2 * time.Second
)

var retryableStatus = map[int]bool{
	http.StatusBadGateway:         true,
	http.StatusServiceUnavailable: true,
	http.StatusGatewayTimeout:     true,
}

// Options configures one Client.
type Options struct {
	// Origin is the validated Liftr server origin (ParseOrigin).
	Origin *url.URL
	// Token is an already-issued bearer access token; empty sends no
	// Authorization header at all.
	Token string
	// UserAgent is sent as the User-Agent header.
	UserAgent string
	// CorrelationID is sent as X-Correlation-ID on every request.
	CorrelationID string
	// HTTPClient replaces the default transport in tests. Redirect refusal
	// is enforced regardless.
	HTTPClient *http.Client
}

// Client performs authenticated calls against exactly one Liftr origin.
type Client struct {
	origin        *url.URL
	http          *http.Client
	token         string
	userAgent     string
	correlationID string
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("liftr client refuses redirects")
}

// New constructs a Client. The origin must have been validated with
// ParseOrigin.
func New(opts Options) (*Client, error) {
	if opts.Origin == nil {
		return nil, errors.New("server origin is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	// Redirect refusal is an unconditional security property of this
	// client, so it is enforced even over an injected transport.
	clone := *hc
	clone.CheckRedirect = refuseRedirect
	hc = &clone
	return &Client{
		origin:        opts.Origin,
		http:          hc,
		token:         opts.Token,
		userAgent:     opts.UserAgent,
		correlationID: opts.CorrelationID,
	}, nil
}

// Origin returns the configured server origin.
func (c *Client) Origin() *url.URL { return c.origin }

// Token exposes whether (not what) a credential is configured so the CLI can
// redact it from rendered diagnostics. It returns a boolean, never the value.
func (c *Client) HasToken() bool { return c.token != "" }

// Redact replaces every occurrence of the configured credential in s with a
// placeholder. It is a defense-in-depth filter for terminal output: even a
// hostile server that echoes the bearer credential inside Problem fields
// cannot make the CLI reprint it.
func (c *Client) Redact(s string) string {
	if c.token == "" || !strings.Contains(s, c.token) {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[redacted]")
}

// TransportError reports a failure that produced no usable response. For a
// mutation, OutcomeUnknown records that the server may still have processed
// the request, so only a replay with the identical idempotency key resolves
// the outcome safely.
type TransportError struct {
	Method         string
	Err            error
	OutcomeUnknown bool
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("%s request failed: %v", e.Method, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

type response struct {
	status int
	header http.Header
	raw    []byte
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, extraHeaders map[string]string, mutation bool) (*response, error) {
	attempts := readAttempts
	if mutation {
		attempts = mutateAttempts
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.origin.String()+path, reader)
		if err != nil {
			return nil, &TransportError{Method: method, Err: err}
		}
		req.Header.Set("Accept", "application/json")
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}
		if c.correlationID != "" {
			req.Header.Set("X-Correlation-ID", c.correlationID)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				lastErr = &TransportError{Method: method, Err: ctxErr}
				break
			}
			lastErr = &TransportError{Method: method, Err: err, OutcomeUnknown: mutation}
			if attempt >= attempts {
				break
			}
			if !sleepBackoff(ctx, attempt, 0) {
				return nil, lastErr
			}
			continue
		}

		raw, truncated, readErr := readBounded(resp.Body, maxResponseBytes)
		resp.Body.Close()
		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				lastErr = &TransportError{Method: method, Err: ctxErr}
				break
			}
			lastErr = &TransportError{Method: method, Err: fmt.Errorf("reading response: %w", readErr), OutcomeUnknown: mutation}
			if attempt >= attempts {
				break
			}
			if !sleepBackoff(ctx, attempt, 0) {
				return nil, lastErr
			}
			continue
		}
		if truncated {
			return nil, &TransportError{Method: method, Err: fmt.Errorf("response exceeds the %d byte limit", maxResponseBytes)}
		}

		outcomeUnknown := mutation && retryableStatus[resp.StatusCode]
		if retryableStatus[resp.StatusCode] && attempt < attempts {
			delay := retryDelay(resp.Header.Get("Retry-After"), attempt)
			if !sleepBackoff(ctx, 0, delay) {
				return nil, &TransportError{Method: method, Err: ctx.Err(), OutcomeUnknown: outcomeUnknown}
			}
			lastErr = &TransportError{Method: method, Err: fmt.Errorf("server returned %d", resp.StatusCode), OutcomeUnknown: outcomeUnknown}
			continue
		}
		return &response{status: resp.StatusCode, header: resp.Header, raw: raw}, nil
	}
	var terr *TransportError
	if errors.As(lastErr, &terr) {
		return nil, terr
	}
	return nil, lastErr
}

func sleepBackoff(ctx context.Context, attempt int, minimum time.Duration) bool {
	delay := minimum
	if delay <= 0 {
		shift := attempt - 1
		if shift < 0 {
			shift = 0
		}
		if shift > 4 {
			shift = 4
		}
		delay = backoffBase << shift
		if delay > backoffMax {
			delay = backoffMax
		}
		delay = jitter(delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func retryDelay(retryAfter string, attempt int) time.Duration {
	if retryAfter != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 && seconds <= 60 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

func jitter(d time.Duration) time.Duration {
	spread := d / 5
	if spread <= 0 {
		return d
	}
	return d - spread + time.Duration(rand.Int64N(int64(2*spread)))
}

func readBounded(r io.Reader, limit int64) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > limit {
		return nil, true, nil
	}
	return raw, false, nil
}

func mediaType(contentType string) (string, bool) {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", false
	}
	return parsed, true
}
