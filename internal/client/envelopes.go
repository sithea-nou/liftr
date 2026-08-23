// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"encoding/json"
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

// WrapUpdateSpec assembles the full-replacement envelope {"spec":...}.
func WrapUpdateSpec(spec []byte) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(`{"spec":`)
	buffer.Write(spec)
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
