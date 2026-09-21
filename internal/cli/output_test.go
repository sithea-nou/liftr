// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/client"
)

var errOutputUnavailable = errors.New("output unavailable")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errOutputUnavailable }

func TestVersionReturnsFailureWhenStdoutCannotBeWritten(t *testing.T) {
	var stderr strings.Builder
	code := Execute(context.Background(), []string{"version"}, failingWriter{}, &stderr, strings.NewReader(""), "test")
	if code != ExitFailure {
		t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), errOutputUnavailable.Error()) {
		t.Fatalf("stderr = %q, want output error", stderr.String())
	}
}

func TestTextRenderersReturnWriterFailures(t *testing.T) {
	app := &App{}
	tests := []struct {
		name   string
		render func(io.Writer) error
	}{
		{"resource", func(w io.Writer) error { return app.renderResourceText(w, &client.Resource{}) }},
		{"operation", func(w io.Writer) error { return app.renderOperationText(w, &client.Operation{}) }},
		{"resource list", func(w io.Writer) error { return app.renderResourceListText(w, &client.ResourceList{}) }},
		{"operation list", func(w io.Writer) error { return app.renderOperationListText(w, &client.OperationList{}) }},
		{"resource type list", func(w io.Writer) error { return app.renderResourceTypeListText(w, &client.ResourceTypeList{}) }},
		{"resource type detail", func(w io.Writer) error {
			return app.renderResourceTypeDetailText(w, &client.ResourceTypeDetail{SpecSchema: []byte(`{}`)})
		}},
		{"admission", func(w io.Writer) error { return app.renderAdmissionText(w, "created", &client.Resource{}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.render(failingWriter{}); !errors.Is(err, errOutputUnavailable) {
				t.Fatalf("error = %v, want %v", err, errOutputUnavailable)
			}
		})
	}
}
