// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// MaxInputBytes mirrors the server's request-body bound so oversized input
// fails locally instead of being transmitted.
const MaxInputBytes = 1 << 20

// readDocument reads a JSON document from a file path or "-" for stdin and
// enforces the bounded-size rule.
func (a *App) readDocument(source string) ([]byte, error) {
	var data []byte
	if source == "-" {
		limited, err := io.ReadAll(io.LimitReader(a.stdin, MaxInputBytes+1))
		if err != nil {
			return nil, fmt.Errorf("reading standard input: %w", err)
		}
		data = limited
	} else {
		info, err := os.Stat(source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		if info.Size() > MaxInputBytes {
			return nil, fmt.Errorf("%s exceeds the %d byte input limit", source, MaxInputBytes)
		}
		data, err = os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
	}
	if len(data) > MaxInputBytes {
		return nil, fmt.Errorf("input exceeds the %d byte limit", MaxInputBytes)
	}
	return data, nil
}

// validateSingleJSONDocument checks well-formedness, the exactly-one-document
// rule, and an object root. It never re-encodes: callers splice raw bytes so
// numeric literals such as 20 versus 20.0 survive to admission untouched.
func validateSingleJSONObject(raw []byte, what string) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%s is empty", what)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document json.RawMessage
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("%s is not valid JSON", what)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s must contain exactly one JSON document", what)
	}
	if trimmed := bytes.TrimSpace(document); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%s must be a JSON object", what)
	}
	return nil
}

// parseOwnerRef splits KIND=ID on the first '='; both halves must be
// non-empty.
func parseOwnerRef(value string) (kind, id string, err error) {
	parsedKind, parsedID, found := strings.Cut(value, "=")
	if !found || strings.TrimSpace(parsedKind) == "" || strings.TrimSpace(parsedID) == "" {
		return "", "", errors.New("--owner must have the form KIND=ID")
	}
	return parsedKind, parsedID, nil
}

// newIdempotencyKey mints one cryptographically random key per mutation
// invocation. Keys are identifiers, not credentials; they are never
// persisted.
func newIdempotencyKey() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return "cli-" + hex.EncodeToString(raw)
}

func newCorrelationID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}
