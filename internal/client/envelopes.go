// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Wire-envelope assembly for mutations. Specs are spliced verbatim so
// numeric literals keep their exact lexical form end to end; callers must
// validate that spec is exactly one JSON object beforehand.

// BuildCreateEnvelope assembles {"id","type","owner","spec"}.
func BuildCreateEnvelope(id, typeName, version, ownerKind, ownerID string, spec []byte) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(`{"id":`)
	writeJSONString(&buffer, id)
	buffer.WriteString(`,"type":{"name":`)
	writeJSONString(&buffer, typeName)
	buffer.WriteString(`,"version":`)
	writeJSONString(&buffer, version)
	buffer.WriteString(`},"owner":{"kind":`)
	writeJSONString(&buffer, ownerKind)
	buffer.WriteString(`,"id":`)
	writeJSONString(&buffer, ownerID)
	buffer.WriteString(`},"spec":`)
	buffer.Write(spec)
	buffer.WriteString(`}`)
	return buffer.Bytes()
}

// ReferenceBinding is one canonical reference slot for envelope assembly.
type ReferenceBinding struct {
	Slot    string
	Targets []string
}

// BuildCreateEnvelopeWithReferences assembles the create document with an
// explicit desired references binding. Slots and target IDs are emitted in
// sorted order; the server canonicalizes independently, so this ordering is a
// byte-stability courtesy for idempotent retries rather than a requirement.
func BuildCreateEnvelopeWithReferences(id, typeName, version, ownerKind, ownerID string, spec []byte, references []ReferenceBinding) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(`{"id":`)
	writeJSONString(&buffer, id)
	buffer.WriteString(`,"type":{"name":`)
	writeJSONString(&buffer, typeName)
	buffer.WriteString(`,"version":`)
	writeJSONString(&buffer, version)
	buffer.WriteString(`},"owner":{"kind":`)
	writeJSONString(&buffer, ownerKind)
	buffer.WriteString(`,"id":`)
	writeJSONString(&buffer, ownerID)
	buffer.WriteString(`},"spec":`)
	buffer.Write(spec)
	if len(references) > 0 {
		sorted := append([]ReferenceBinding(nil), references...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].Slot < sorted[b].Slot })
		buffer.WriteString(`,"references":{`)
		for i, binding := range sorted {
			if i > 0 {
				buffer.WriteByte(',')
			}
			writeJSONString(&buffer, binding.Slot)
			buffer.WriteString(`:[`)
			targets := append([]string(nil), binding.Targets...)
			sort.Strings(targets)
			for j, target := range targets {
				if j > 0 {
					buffer.WriteByte(',')
				}
				writeJSONString(&buffer, target)
			}
			buffer.WriteString(`]`)
		}
		buffer.WriteString(`}`)
	}
	buffer.WriteString(`}`)
	return buffer.Bytes()
}

// WrapUpdateSpec assembles the full-replacement envelope {"spec":...}.
func WrapUpdateSpec(spec []byte) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(`{"spec":`)
	buffer.Write(spec)
	buffer.WriteString(`}`)
	return buffer.Bytes()
}

// WrapUpdateReferences assembles an update envelope whose references field is
// EXPLICITLY present. present=false omits the field entirely (the server then
// preserves stored relationships); present=true with zero bindings emits {} to
// clear every optional slot. This distinction is deliberate: absence must stay
// meaningful so pre-M21 clients that send only spec can never destroy
// relationships.
func WrapUpdateReferences(spec []byte, present bool, references []ReferenceBinding) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(`{"spec":`)
	buffer.Write(spec)
	if present {
		buffer.WriteString(`,"references":{`)
		sorted := append([]ReferenceBinding(nil), references...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].Slot < sorted[b].Slot })
		for i, binding := range sorted {
			if i > 0 {
				buffer.WriteByte(',')
			}
			writeJSONString(&buffer, binding.Slot)
			buffer.WriteString(`:[`)
			targets := append([]string(nil), binding.Targets...)
			sort.Strings(targets)
			for j, target := range targets {
				if j > 0 {
					buffer.WriteByte(',')
				}
				writeJSONString(&buffer, target)
			}
			buffer.WriteString(`]`)
		}
		buffer.WriteString(`}`)
	}
	buffer.WriteString(`}`)
	return buffer.Bytes()
}

func writeJSONString(buffer *bytes.Buffer, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		// json.Marshal of a string cannot fail.
		panic(err)
	}
	buffer.Write(encoded)
}
