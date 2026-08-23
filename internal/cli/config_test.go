// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/client"
)

func TestResolveServerRawPrecedence(t *testing.T) {
	t.Run("flag beats environment beats default", func(t *testing.T) {
		t.Setenv("LIFTR_SERVER", "http://localhost:1")
		if got := resolveServerRaw("https://flag.example.com"); got != "https://flag.example.com" {
			t.Fatalf("flag precedence: %q", got)
		}
		if got := resolveServerRaw(""); got != "http://localhost:1" {
			t.Fatalf("environment precedence: %q", got)
		}
		if got := resolveServerRaw("   "); got != "http://localhost:1" {
			t.Fatalf("whitespace flag must not shadow environment: %q", got)
		}
	})

	t.Run("empty environment falls through to default", func(t *testing.T) {
		t.Setenv("LIFTR_SERVER", "   ")
		if got := resolveServerRaw(""); got != DefaultServerURL {
			t.Fatalf("default: %q", got)
		}
	})
}

func TestLoadTokenFileRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics are not exercised on Windows")
	}
	dir := t.TempDir()

	t.Run("trims and accepts", func(t *testing.T) {
		path := filepath.Join(dir, "token")
		os.WriteFile(path, []byte("  token-value \n"), 0o600)
		got, err := loadTokenFile(path, nil)
		if err != nil || got != "token-value" {
			t.Fatalf("load = %q, err %v", got, err)
		}
	})

	t.Run("empty file refused", func(t *testing.T) {
		path := filepath.Join(dir, "empty")
		os.WriteFile(path, []byte("\n\n"), 0o600)
		if _, err := loadTokenFile(path, nil); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("oversized credential refused without revealing length", func(t *testing.T) {
		path := filepath.Join(dir, "huge")
		os.WriteFile(path, []byte(strings.Repeat("x", client.MaxTokenBytes+1)), 0o600)
		_, err := loadTokenFile(path, nil)
		if err == nil || !strings.Contains(err.Error(), "accepted size") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "8193") {
			t.Fatalf("error reveals credential size: %v", err)
		}
	})

	t.Run("directory is not a regular file", func(t *testing.T) {
		if _, err := loadTokenFile(dir, nil); err == nil {
			t.Fatal("directory accepted as token file")
		}
	})
}
