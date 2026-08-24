// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/sithea-nou/liftr/internal/client"
)

const (
	outputText = "text"
	outputJSON = "json"
)

// sanitize makes server-supplied strings safe for terminal rendering:
// control characters (including escape sequences) become spaces and
// whitespace collapses, so a hostile Problem detail cannot fake terminal
// output.
func sanitize(value string) string {
	replaced := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(replaced), " ")
}

// emitJSON writes exactly one valid JSON document plus newline to the writer.
// The bytes are emitted verbatim: numeric literals such as 20 versus 20.0 are
// never normalized, and JSON payloads are not passed through text
// sanitization (encoding/json escaping already makes them terminal-safe).
func emitJSON(w io.Writer, raw []byte) error {
	if !json.Valid(raw) {
		return fmt.Errorf("internal: refusing to emit invalid JSON")
	}
	if _, err := w.Write(append(json.RawMessage(nil), raw...)); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatOutputValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return sanitize(typed)
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	default:
		return sanitize(fmt.Sprint(typed))
	}
}

func writeIndentedJSON(w io.Writer, raw []byte) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		_, _ = w.Write(raw)
		_, _ = io.WriteString(w, "\n")
		return
	}
	fmt.Fprintln(w, buffer.String())
}

// clean applies credential redaction and terminal sanitization to any
// server- or user-derived string before it reaches the terminal. Even a
// hostile server echoing the bearer credential inside Problem fields cannot
// make the CLI reprint it.
func (a *App) clean(value string) string {
	if a.api != nil {
		value = a.api.Redact(value)
	}
	return sanitize(value)
}

func (a *App) renderResourceText(w io.Writer, resource *client.Resource) {
	c := a.clean
	fmt.Fprintf(w, "ID:                 %s\n", c(resource.ID))
	fmt.Fprintf(w, "Type:               %s/%s\n", c(resource.Type.Name), c(resource.Type.Version))
	fmt.Fprintf(w, "Owner:              %s/%s\n", c(resource.Owner.Kind), c(resource.Owner.ID))
	fmt.Fprintf(w, "State:              %s\n", c(resource.Status.State))
	fmt.Fprintf(w, "Generation:         %d (observed generation %d)\n", resource.Generation, resource.Status.ObservedGeneration)
	fmt.Fprintf(w, "Created:            %s\n", formatTimestamp(resource.CreatedAt))
	fmt.Fprintf(w, "Updated:            %s\n", formatTimestamp(resource.UpdatedAt))
	if len(resource.Status.Conditions) > 0 {
		fmt.Fprintln(w, "\nConditions:")
		for _, condition := range resource.Status.Conditions {
			line := fmt.Sprintf("  %s=%s %s (observed generation %d)",
				c(condition.Type), c(condition.Status), c(condition.Reason), condition.ObservedGeneration)
			if condition.Message != "" {
				line += ": " + c(condition.Message)
			}
			fmt.Fprintln(w, line)
		}
	}
	if resource.LatestOperation != nil {
		latest := resource.LatestOperation
		fmt.Fprintf(w, "\nLatest operation:   %s (%s, %s, target generation %d)\n",
			c(latest.ID), c(latest.Capability), c(latest.State), latest.TargetGeneration)
	}
	switch {
	case resource.Outputs == nil:
		fmt.Fprint(w, "\nOutputs:            none published yet\n")
	default:
		outputs := resource.Outputs
		freshness := "current"
		if outputs.ObservedGeneration < resource.Generation {
			freshness = fmt.Sprintf("STALE — outputs describe generation %d, desired generation is %d",
				outputs.ObservedGeneration, resource.Generation)
		}
		fmt.Fprintf(w, "\nOutputs (generation %d): %s\n", outputs.ObservedGeneration, freshness)
		names := make([]string, 0, len(outputs.Values))
		for name := range outputs.Values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(w, "  %s: %s\n", c(name), formatOutputValue(outputs.Values[name]))
		}
	}
}

func (a *App) renderOperationText(w io.Writer, operation *client.Operation) {
	c := a.clean
	fmt.Fprintf(w, "ID:                 %s\n", c(operation.ID))
	fmt.Fprintf(w, "Resource:           %s\n", c(operation.ResourceID))
	if operation.RetryOf != "" {
		fmt.Fprintf(w, "Retry of:           %s\n", c(operation.RetryOf))
	}
	fmt.Fprintf(w, "Capability:         %s\n", c(operation.Capability))
	fmt.Fprintf(w, "State:              %s\n", c(operation.State))
	fmt.Fprintf(w, "Target generation:  %d\n", operation.TargetGeneration)
	fmt.Fprintf(w, "Requested at:       %s\n", formatTimestamp(operation.RequestedAt))
	if operation.StartedAt != nil {
		fmt.Fprintf(w, "Started at:         %s\n", formatTimestamp(*operation.StartedAt))
	}
	if operation.CompletedAt != nil {
		fmt.Fprintf(w, "Completed at:       %s\n", formatTimestamp(*operation.CompletedAt))
	}
	if operation.Failure != nil {
		fmt.Fprintf(w, "Failure:            %s\n", c(operation.Failure.Reason))
		if operation.Failure.Message != "" {
			fmt.Fprintf(w, "  %s\n", c(operation.Failure.Message))
		}
	}
}

func (a *App) renderOperationListText(w io.Writer, list *client.OperationList) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCAPABILITY\tSTATE\tTARGET GENERATION\tREQUESTED\tCOMPLETED\tRETRY OF")
	for i := range list.Items {
		operation := &list.Items[i]
		completed := "-"
		if operation.CompletedAt != nil {
			completed = formatTimestamp(*operation.CompletedAt)
		}
		retryOf := "-"
		if operation.RetryOf != "" {
			retryOf = a.clean(operation.RetryOf)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			a.clean(operation.ID), a.clean(operation.Capability), a.clean(operation.State),
			operation.TargetGeneration, formatTimestamp(operation.RequestedAt), completed, retryOf)
	}
	_ = tw.Flush()
}

func (a *App) renderResourceTypeListText(w io.Writer, list *client.ResourceTypeList) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tDISPLAY NAME\tCAPABILITIES")
	for _, item := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			a.clean(item.Name), a.clean(item.Version), a.clean(item.DisplayName),
			a.clean(strings.Join(item.Capabilities, " ")))
	}
	tw.Flush()
}

func (a *App) renderResourceTypeDetailText(w io.Writer, detail *client.ResourceTypeDetail) {
	fmt.Fprintf(w, "Name:               %s\n", a.clean(detail.Name))
	fmt.Fprintf(w, "Version:            %s\n", a.clean(detail.Version))
	fmt.Fprintf(w, "Display name:       %s\n", a.clean(detail.DisplayName))
	fmt.Fprintf(w, "Description:        %s\n", a.clean(detail.Description))
	fmt.Fprintf(w, "Capabilities:       %s\n", a.clean(strings.Join(detail.Capabilities, " ")))
	fmt.Fprintln(w, "\nSpec schema:")
	writeIndentedJSON(w, detail.SpecSchema)
	if len(detail.OutputContract) > 0 {
		fmt.Fprintln(w, "\nOutput contract:")
		writeIndentedJSON(w, detail.OutputContract)
	}
}

// renderAdmissionText prints the admitted mutation's Resource snapshot.
func (a *App) renderAdmissionText(w io.Writer, verb string, resource *client.Resource) {
	fmt.Fprintf(w, "%s %s\n", verb, a.clean(resource.ID))
	fmt.Fprintf(w, "type:               %s/%s\n", a.clean(resource.Type.Name), a.clean(resource.Type.Version))
	fmt.Fprintf(w, "generation:         %d\n", resource.Generation)
	fmt.Fprintf(w, "state:              %s\n", a.clean(resource.Status.State))
	if resource.LatestOperation != nil {
		fmt.Fprintf(w, "operation:          %s (%s, %s)\n",
			a.clean(resource.LatestOperation.ID),
			a.clean(resource.LatestOperation.Capability),
			a.clean(resource.LatestOperation.State))
	}
}

// renderProblem writes the decoded RFC 9457 problem with its Liftr
// extensions to stderr. The request ID is always shown for support
// correlation; hidden not-found problems are rendered exactly as served; all
// rendered strings are redacted and sanitized first.
func (a *App) renderProblem(apiErr *client.APIError) {
	w := a.stderr
	title := apiErr.Problem.Title
	if title == "" {
		title = "request failed"
	}
	code := apiErr.Problem.Code
	if code == "" {
		code = "UNKNOWN"
	}
	fmt.Fprintf(w, "error: %s (%s)\n", a.clean(title), code)
	if apiErr.Problem.Detail != "" {
		fmt.Fprintf(w, "  %s\n", a.clean(apiErr.Problem.Detail))
	}
	if apiErr.Problem.CurrentGeneration != nil {
		fmt.Fprintf(w, "  Current generation: %d\n", *apiErr.Problem.CurrentGeneration)
	}
	if len(apiErr.Problem.Violations) > 0 {
		fmt.Fprintln(w, "  Spec violations:")
		for _, violation := range apiErr.Problem.Violations {
			fmt.Fprintf(w, "    - %s %s: %s\n",
				a.clean(violation.Path), a.clean(violation.Keyword), a.clean(violation.Message))
		}
		if apiErr.Problem.Truncated {
			fmt.Fprintln(w, "    (violation list truncated by the server)")
		}
	}
	requestID := apiErr.Problem.RequestID
	if requestID == "" {
		requestID = apiErr.RequestID
	}
	if requestID != "" {
		fmt.Fprintf(w, "  Request ID: %s\n", a.clean(requestID))
	}
}
