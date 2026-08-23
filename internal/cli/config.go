// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/sithea-nou/liftr/internal/client"
)

// DefaultServerURL is the development convenience default. It is loopback
// only: the client refuses plaintext HTTP to any non-loopback host outright,
// regardless of whether a credential is configured, so the default can never
// leak a bearer token off the machine.
const DefaultServerURL = "http://localhost:8080"

// resolveServerRaw applies flag > LIFTR_SERVER > default precedence. Empty
// values at a higher-precedence level never shadow a lower one.
func resolveServerRaw(flagValue string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("LIFTR_SERVER")); value != "" {
		return value
	}
	return DefaultServerURL
}

// resolveToken applies --token-file > LIFTR_TOKEN_FILE > LIFTR_TOKEN
// precedence. There is deliberately no --token flag: command lines appear in
// shell history and process listings. An invocation with no configured
// credential sends no Authorization header and lets the server decide, which
// keeps explicitly insecure development servers usable without ceremony.
func (a *App) resolveToken(flagValue string) error {
	if path := strings.TrimSpace(flagValue); path != "" {
		return a.useTokenFile(path)
	}
	if path := strings.TrimSpace(os.Getenv("LIFTR_TOKEN_FILE")); path != "" {
		return a.useTokenFile(path)
	}
	if token := strings.TrimSpace(os.Getenv("LIFTR_TOKEN")); token != "" {
		return a.buildClient(token)
	}
	return a.buildClient("")
}

func (a *App) useTokenFile(path string) error {
	token, err := loadTokenFile(path, a.stderr)
	if err != nil {
		return err
	}
	return a.buildClient(token)
}

func loadTokenFile(path string, warnings io.Writer) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 && warnings != nil {
		fmt.Fprintf(warnings, "warning: token file %s is readable beyond its owner\n", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if len(token) > client.MaxTokenBytes {
		return "", errors.New("the configured credential exceeds the accepted size")
	}
	return token, nil
}

func (a *App) buildClient(token string) error {
	api, err := client.New(client.Options{
		Origin:        a.origin,
		Token:         token,
		UserAgent:     userAgent(a.version),
		CorrelationID: a.correlationID,
	})
	if err != nil {
		return err
	}
	a.api = api
	return nil
}

func userAgent(version string) string {
	return "liftr/" + version
}

// prepare resolves output mode, server origin, and credentials before any
// network activity. Every failure here is a usage/configuration error.
func (a *App) prepare() error {
	switch a.flagOutput {
	case "", outputText:
		a.output = outputText
	case outputJSON:
		a.output = outputJSON
	default:
		return fmt.Errorf("invalid -o/--output value %q; expected %q or %q", a.flagOutput, outputText, outputJSON)
	}
	origin, err := client.ParseOrigin(resolveServerRaw(a.flagServer))
	if err != nil {
		return err
	}
	a.origin = origin
	return a.resolveToken(a.flagTokenFile)
}
