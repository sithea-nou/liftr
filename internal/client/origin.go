// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The configured server address is an origin, nothing more. Liftr returns
// API references such as /v1/resources/... and /v1/operations/..., so
// reverse-proxy path prefixes would require an explicit base-path contract;
// the CLI does not guess one.

// ParseOrigin validates a configured server address and normalizes it to a
// bare origin (scheme, host, optional port; no path, query, fragment, or
// userinfo). Plain HTTP is accepted only for syntactic loopback hosts —
// "localhost", IPv4 in 127.0.0.0/8, or the IPv6 loopback ::1 — because a
// bearer credential must never travel over non-loopback plaintext transport.
// Hostnames are never resolved to decide loopback membership.
func ParseOrigin(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("server address is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("server address %q is not a valid URL: %w", raw, err)
	}
	if parsed.Opaque != "" {
		return nil, fmt.Errorf("server address %q must be an absolute http(s) origin", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("server address %q must use http or https", raw)
	}
	if parsed.User != nil {
		return nil, errors.New("server address must not contain userinfo")
	}
	if parsed.RawQuery != "" {
		return nil, errors.New("server address must not contain a query")
	}
	if parsed.Fragment != "" || parsed.ForceQuery {
		return nil, errors.New("server address must not contain a fragment")
	}
	switch parsed.EscapedPath() {
	case "", "/":
	default:
		return nil, errors.New("server address must be an origin without a path; path-prefix hosting is not supported")
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("server address is missing a host")
	}
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("plaintext HTTP is only allowed for loopback addresses (%s, 127.0.0.0/8, [::1]); use HTTPS for %q", "localhost", parsed.Host)
	}
	normalized := &url.URL{Scheme: scheme, Host: parsed.Host}
	return normalized, nil
}

// isLoopbackHost reports whether host is syntactically a loopback address.
// It never performs DNS resolution: a remote name that happens to resolve to
// loopback is still refused over plaintext HTTP.
func isLoopbackHost(host string) bool {
	name := strings.ToLower(host)
	name = strings.TrimSuffix(name, ".")
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin compares scheme, host, and effective port (defaults supplied
// per scheme), so https://host and https://host:443 compare equal while any
// other difference refuses.
func sameOrigin(a, b *url.URL) bool {
	return originKey(a) == originKey(b)
}

func originKey(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "|" + host + "|" + port
}
