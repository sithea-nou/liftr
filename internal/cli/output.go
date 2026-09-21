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

type textWriter struct {
	w   io.Writer
	err error
}

func (out *textWriter) printf(format string, args ...any) {
	if out.err == nil {
		_, out.err = fmt.Fprintf(out.w, format, args...)
	}
}

func (out *textWriter) println(args ...any) {
	if out.err == nil {
		_, out.err = fmt.Fprintln(out.w, args...)
	}
}

func (out *textWriter) print(args ...any) {
	if out.err == nil {
		_, out.err = fmt.Fprint(out.w, args...)
	}
}

func writeIndentedJSON(w io.Writer, raw []byte) error {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		if _, writeErr := w.Write(raw); writeErr != nil {
			return writeErr
		}
		_, writeErr := io.WriteString(w, "\n")
		return writeErr
	}
	_, err := fmt.Fprintln(w, buffer.String())
	return err
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

func (a *App) renderResourceText(w io.Writer, resource *client.Resource) error {
	out := &textWriter{w: w}
	c := a.clean
	out.printf("ID:                 %s\n", c(resource.ID))
	out.printf("Type:               %s/%s\n", c(resource.Type.Name), c(resource.Type.Version))
	out.printf("Owner:              %s/%s\n", c(resource.Owner.Kind), c(resource.Owner.ID))
	out.printf("State:              %s\n", c(resource.Status.State))
	out.printf("Generation:         %d (observed generation %d)\n", resource.Generation, resource.Status.ObservedGeneration)
	out.printf("Created:            %s\n", formatTimestamp(resource.CreatedAt))
	out.printf("Updated:            %s\n", formatTimestamp(resource.UpdatedAt))
	if len(resource.Status.Conditions) > 0 {
		out.println("\nConditions:")
		for _, condition := range resource.Status.Conditions {
			line := fmt.Sprintf("  %s=%s %s (observed generation %d)",
				c(condition.Type), c(condition.Status), c(condition.Reason), condition.ObservedGeneration)
			if condition.Message != "" {
				line += ": " + c(condition.Message)
			}
			out.println(line)
		}
	}
	if len(resource.References) > 0 {
		out.println("\nReferences (desired):")
		slots := make([]string, 0, len(resource.References))
		for slot := range resource.References {
			slots = append(slots, slot)
		}
		sort.Strings(slots)
		for _, slot := range slots {
			targets := append([]string(nil), resource.References[slot]...)
			sort.Strings(targets)
			out.printf("  %s: %s\n", c(slot), c(strings.Join(targets, ", ")))
		}
	}
	if resource.LatestOperation != nil {
		latest := resource.LatestOperation
		out.printf("\nLatest operation:   %s (%s, %s, target generation %d)\n",
			c(latest.ID), c(latest.Capability), c(latest.State), latest.TargetGeneration)
	}
	switch resource.Outputs {
	case nil:
		out.print("\nOutputs:            none published yet\n")
	default:
		outputs := resource.Outputs
		freshness := "current"
		if outputs.ObservedGeneration < resource.Generation {
			freshness = fmt.Sprintf("STALE — outputs describe generation %d, desired generation is %d",
				outputs.ObservedGeneration, resource.Generation)
		}
		out.printf("\nOutputs (generation %d): %s\n", outputs.ObservedGeneration, freshness)
		names := make([]string, 0, len(outputs.Values))
		for name := range outputs.Values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out.printf("  %s: %s\n", c(name), formatOutputValue(outputs.Values[name]))
		}
	}
	return out.err
}

func (a *App) renderOperationText(w io.Writer, operation *client.Operation) error {
	out := &textWriter{w: w}
	c := a.clean
	out.printf("ID:                 %s\n", c(operation.ID))
	out.printf("Resource:           %s\n", c(operation.ResourceID))
	if operation.RetryOf != "" {
		out.printf("Retry of:           %s\n", c(operation.RetryOf))
	}
	out.printf("Capability:         %s\n", c(operation.Capability))
	out.printf("State:              %s\n", c(operation.State))
	out.printf("Target generation:  %d\n", operation.TargetGeneration)
	out.printf("Requested at:       %s\n", formatTimestamp(operation.RequestedAt))
	if operation.StartedAt != nil {
		out.printf("Started at:         %s\n", formatTimestamp(*operation.StartedAt))
	}
	if operation.CompletedAt != nil {
		out.printf("Completed at:       %s\n", formatTimestamp(*operation.CompletedAt))
	}
	if operation.Failure != nil {
		out.printf("Failure:            %s\n", c(operation.Failure.Reason))
		if operation.Failure.Message != "" {
			out.printf("  %s\n", c(operation.Failure.Message))
		}
	}
	return out.err
}

func (a *App) renderOperationListText(w io.Writer, list *client.OperationList) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	out := &textWriter{w: tw}
	out.println("ID\tCAPABILITY\tSTATE\tTARGET GENERATION\tREQUESTED\tCOMPLETED\tRETRY OF")
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
		out.printf("%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			a.clean(operation.ID), a.clean(operation.Capability), a.clean(operation.State),
			operation.TargetGeneration, formatTimestamp(operation.RequestedAt), completed, retryOf)
	}
	if out.err != nil {
		return out.err
	}
	return tw.Flush()
}

// renderResourceListText renders one inventory page. Summaries carry no
// spec, outputs, or conditions by contract, so none are printed; detail
// lives in `liftr resource get`.
func (a *App) renderResourceListText(w io.Writer, list *client.ResourceList) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	out := &textWriter{w: tw}
	out.println("ID\tTYPE\tOWNER\tSTATE\tGENERATION\tOBSERVED\tLATEST OPERATION")
	for i := range list.Items {
		summary := &list.Items[i]
		latest := "-"
		if summary.LatestOperation != nil {
			latest = a.clean(summary.LatestOperation.ID) + "/" + a.clean(summary.LatestOperation.State)
		}
		out.printf("%s\t%s/%s\t%s/%s\t%s\t%d\t%d\t%s\n",
			a.clean(summary.ID),
			a.clean(summary.Type.Name), a.clean(summary.Type.Version),
			a.clean(summary.Owner.Kind), a.clean(summary.Owner.ID),
			a.clean(summary.Status.State), summary.Generation,
			summary.Status.ObservedGeneration, latest)
	}
	if out.err != nil {
		return out.err
	}
	return tw.Flush()
}

func (a *App) renderResourceTypeListText(w io.Writer, list *client.ResourceTypeList) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	out := &textWriter{w: tw}
	out.println("NAME\tVERSION\tDISPLAY NAME\tCAPABILITIES")
	for _, item := range list.Items {
		out.printf("%s\t%s\t%s\t%s\n",
			a.clean(item.Name), a.clean(item.Version), a.clean(item.DisplayName),
			a.clean(strings.Join(item.Capabilities, " ")))
	}
	if out.err != nil {
		return out.err
	}
	return tw.Flush()
}

func (a *App) renderResourceTypeDetailText(w io.Writer, detail *client.ResourceTypeDetail) error {
	out := &textWriter{w: w}
	out.printf("Name:               %s\n", a.clean(detail.Name))
	out.printf("Version:            %s\n", a.clean(detail.Version))
	out.printf("Display name:       %s\n", a.clean(detail.DisplayName))
	out.printf("Description:        %s\n", a.clean(detail.Description))
	out.printf("Capabilities:       %s\n", a.clean(strings.Join(detail.Capabilities, " ")))
	out.println("\nSpec schema:")
	if out.err != nil {
		return out.err
	}
	if err := writeIndentedJSON(w, detail.SpecSchema); err != nil {
		return err
	}
	if len(detail.OutputContract) > 0 {
		out.println("\nOutput contract:")
		if out.err != nil {
			return out.err
		}
		return writeIndentedJSON(w, detail.OutputContract)
	}
	return nil
}

// renderAdmissionText prints the admitted mutation's Resource snapshot.
func (a *App) renderAdmissionText(w io.Writer, verb string, resource *client.Resource) error {
	out := &textWriter{w: w}
	out.printf("%s %s\n", verb, a.clean(resource.ID))
	out.printf("type:               %s/%s\n", a.clean(resource.Type.Name), a.clean(resource.Type.Version))
	out.printf("generation:         %d\n", resource.Generation)
	out.printf("state:              %s\n", a.clean(resource.Status.State))
	if resource.LatestOperation != nil {
		out.printf("operation:          %s (%s, %s)\n",
			a.clean(resource.LatestOperation.ID),
			a.clean(resource.LatestOperation.Capability),
			a.clean(resource.LatestOperation.State))
	}
	return out.err
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
	_, _ = fmt.Fprintf(w, "error: %s (%s)\n", a.clean(title), code)
	if apiErr.Problem.Detail != "" {
		_, _ = fmt.Fprintf(w, "  %s\n", a.clean(apiErr.Problem.Detail))
	}
	if apiErr.Problem.CurrentGeneration != nil {
		_, _ = fmt.Fprintf(w, "  Current generation: %d\n", *apiErr.Problem.CurrentGeneration)
	}
	if len(apiErr.Problem.Violations) > 0 {
		_, _ = fmt.Fprintln(w, "  Spec violations:")
		for _, violation := range apiErr.Problem.Violations {
			_, _ = fmt.Fprintf(w, "    - %s %s: %s\n",
				a.clean(violation.Path), a.clean(violation.Keyword), a.clean(violation.Message))
		}
		if apiErr.Problem.Truncated {
			_, _ = fmt.Fprintln(w, "    (violation list truncated by the server)")
		}
	}
	requestID := apiErr.Problem.RequestID
	if requestID == "" {
		requestID = apiErr.RequestID
	}
	if requestID != "" {
		_, _ = fmt.Fprintf(w, "  Request ID: %s\n", a.clean(requestID))
	}
}
