// SPDX-License-Identifier: Apache-2.0

// Command liftr is the official Liftr command-line client. It is a pure
// client of the public HTTP API (/v1): it never imports Liftr's application,
// domain, or server implementation packages, so it cannot bypass
// authentication, authorization, idempotency, concurrency semantics, or
// public representation boundaries (ADR-0013).
package main

import (
	"context"
	"os"

	"github.com/sithea-nou/liftr/internal/cli"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Stdin, version))
}
