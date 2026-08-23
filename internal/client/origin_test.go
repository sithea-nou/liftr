// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/client"
)

func TestParseOriginAcceptsValidOrigins(t *testing.T) {
	cases := []struct {
		raw    string
		scheme string
		host   string
	}{
		{"https://liftr.example.com", "https", "liftr.example.com"},
		{"https://liftr.example.com:8443", "https", "liftr.example.com:8443"},
		{"http://localhost", "http", "localhost"},
		{"http://localhost:8080", "http", "localhost:8080"},
		{"http://127.0.0.1", "http", "127.0.0.1"},
		{"http://127.0.0.1:8080", "http", "127.0.0.1:8080"},
		{"http://[::1]", "http", "[::1]"},
		{"http://[::1]:9000", "http", "[::1]:9000"},
		{"http://127.7.41.9:8080/", "http", "127.7.41.9:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			origin, err := client.ParseOrigin(tc.raw)
			if err != nil {
				t.Fatalf("ParseOrigin(%q) returned error: %v", tc.raw, err)
			}
			if origin.Scheme != tc.scheme || origin.Host != tc.host {
				t.Fatalf("normalized origin = %q, want scheme %q host %q", origin.String(), tc.scheme, tc.host)
			}
			if origin.EscapedPath() != "" || origin.RawQuery != "" || origin.Fragment != "" {
				t.Fatalf("origin is not bare: %q", origin.String())
			}
		})
	}
}

func TestParseOriginRefusesNonLoopbackPlaintext(t *testing.T) {
	cases := []string{
		"http://liftr.example.com",
		"http://liftr.example.com:8080",
		// RFC1918 and other non-loopback literals are refused without any
		// DNS resolution. HTTPS accepts every host; only plaintext HTTP is
		// restricted.
		"http://10.1.2.3",
		"http://172.16.0.5",
		"http://192.168.1.10",
		"http://169.254.1.9",
		"http://0.0.0.0",
		"http://local-host.example",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := client.ParseOrigin(raw); err == nil {
				t.Fatalf("ParseOrigin(%q) accepted a non-loopback plaintext or invalid target", raw)
			}
		})
	}
}

func TestParseOriginIsOriginOnly(t *testing.T) {
	cases := []string{
		"https://liftr.example.com/v1",
		"https://liftr.example.com/api/",
		"https://liftr.example.com?x=1",
		"https://liftr.example.com#frag",
		"https://user:secret@liftr.example.com",
		"ftp://liftr.example.com",
		"localhost:8080",
		"/v1/resources",
		"",
		"   ",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := client.ParseOrigin(raw); err == nil {
				t.Fatalf("ParseOrigin(%q) accepted a non-origin address", raw)
			}
		})
	}
}
