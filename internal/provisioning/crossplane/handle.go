// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"encoding/base64"
	"encoding/json"

	"github.com/sithea-nou/liftr/internal/provisioning"
)

// The execution handle is provider-private. It encodes the deterministic
// logical target plus, once observed, the physical Kubernetes UID. Liftr
// never inspects the token; only this adapter decodes it. UID participation
// is deliberately additive: before the first confirmed sighting the handle
// carries no UID and correlation works from identity alone, so restarts are
// safe; after a sighting, a UID change is an identity conflict and never a
// silent adoption.
const handlePrefix = "xc1."

type handlePayload struct {
	Group     string `json:"g,omitempty"`
	Version   string `json:"v"`
	Kind      string `json:"k"`
	Namespace string `json:"ns"`
	Name      string `json:"n"`
	UID       string `json:"uid,omitempty"`
}

func encodeHandle(binding *resolvedBinding, name, uid string) provisioning.ExecutionHandle {
	payload := handlePayload{
		Group:     binding.gvr.Group,
		Version:   binding.gvr.Version,
		Kind:      binding.kind,
		Namespace: binding.namespace,
		Name:      name,
		UID:       uid,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return provisioning.ExecutionHandle{}
	}
	handle, _ := provisioning.NewExecutionHandle(handlePrefix + base64RawURL(encoded))
	return handle
}

func decodeHandle(handle *provisioning.ExecutionHandle) (handlePayload, bool) {
	if handle == nil {
		return handlePayload{}, false
	}
	token := handle.String()
	if len(token) <= len(handlePrefix) || token[:len(handlePrefix)] != handlePrefix {
		return handlePayload{}, false
	}
	raw, err := base64RawURLDecode(token[len(handlePrefix):])
	if err != nil {
		return handlePayload{}, false
	}
	var payload handlePayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Name == "" || payload.Kind == "" || payload.Version == "" {
		return handlePayload{}, false
	}
	return payload, true
}

// handleUID returns the UID recorded on the request handle, if any.
func handleUID(request operationRequest) string {
	payload, ok := decodeHandle(request.handle())
	if !ok {
		return ""
	}
	return payload.UID
}

func base64RawURL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func base64RawURLDecode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}
