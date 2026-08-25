// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package opentofu

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOSRunnerKillsDescendantAfterParentHandlesInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	script := `trap 'exit 0' INT
(trap '' INT; exec sleep 30) >/dev/null 2>&1 &
child=$!
echo "$child"
wait`
	result, err := (OSCommandRunner{}).Run(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}, MaxOutputBytes: 1024})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(result.Stdout)))
	if parseErr != nil || pid <= 0 {
		t.Fatalf("descendant pid output=%q error=%v", result.Stdout, parseErr)
	}
	process, findErr := os.FindProcess(pid)
	if findErr != nil {
		t.Fatal(findErr)
	}
	deadline := time.Now().Add(time.Second)
	for process.Signal(syscall.Signal(0)) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d survived process-group cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
