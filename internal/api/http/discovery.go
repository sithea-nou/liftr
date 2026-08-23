// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// resourceTypeSummaryDTO is one entry of the discovery list. Summaries never
// carry the schema document; they expose only developer-contract metadata.
type resourceTypeSummaryDTO struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Href         string   `json:"href"`
}

// resourceTypeOutputFieldDTO describes one declared output field of a
// ResourceType. jsonType is one of the flat scalar types; requiredWhenReady
// means a successfully reconciled generation must publish this field.
type resourceTypeOutputFieldDTO struct {
	Name              string `json:"name"`
	JSONType          string `json:"jsonType"`
	RequiredWhenReady bool   `json:"requiredWhenReady"`
}

// resourceTypeOutputContractDTO is the developer-consumable output contract of
// one ResourceType version. It declares exact names and types; it never
// describes provider implementation data, and secret material has no
// representation in M10.
type resourceTypeOutputContractDTO struct {
	Fields []resourceTypeOutputFieldDTO `json:"fields"`
}

// resourceTypeDTO is the detailed discovery representation. specSchema embeds
// the registered JSON Schema 2020-12 document verbatim; its keywords are
// governed by the JSON Schema specification, not by this API's envelope.
// outputContract appears only for ResourceTypes that declare realized values;
// list summaries never embed either document.
type resourceTypeDetailDTO struct {
	Name           string                         `json:"name"`
	Version        string                         `json:"version"`
	DisplayName    string                         `json:"displayName"`
	Description    string                         `json:"description"`
	Capabilities   []string                       `json:"capabilities"`
	Href           string                         `json:"href"`
	SpecSchema     json.RawMessage                `json:"specSchema"`
	OutputContract *resourceTypeOutputContractDTO `json:"outputContract,omitempty"`
}

type resourceTypeListDTO struct {
	Items []resourceTypeSummaryDTO `json:"items"`
}

func resourceTypeHref(ref domain.ResourceTypeRef) string {
	return "/v1/resource-types/" + ref.Name + "/" + ref.Version
}

// newListDTO renders summaries in the catalog's deterministic order. Items is
// never nil so an empty catalog serializes as an empty array.
func newListDTO(contracts []application.ResourceContract) resourceTypeListDTO {
	items := make([]resourceTypeSummaryDTO, 0, len(contracts))
	for _, contract := range contracts {
		ref := contract.Ref()
		items = append(items, resourceTypeSummaryDTO{
			Name:         ref.Name,
			Version:      ref.Version,
			DisplayName:  contract.DisplayName(),
			Description:  contract.Description(),
			Capabilities: capabilityNames(contract),
			Href:         resourceTypeHref(ref),
		})
	}
	return resourceTypeListDTO{Items: items}
}

func newResourceTypeDetailDTO(contract application.ResourceContract) resourceTypeDetailDTO {
	ref := contract.Ref()
	detail := resourceTypeDetailDTO{
		Name:         ref.Name,
		Version:      ref.Version,
		DisplayName:  contract.DisplayName(),
		Description:  contract.Description(),
		Capabilities: capabilityNames(contract),
		Href:         resourceTypeHref(ref),
		SpecSchema:   append(json.RawMessage(nil), contract.SpecSchema()...),
	}
	if declared := contract.OutputContract(); declared != nil {
		outputContract := &resourceTypeOutputContractDTO{Fields: []resourceTypeOutputFieldDTO{}}
		for _, field := range declared.Fields() {
			outputContract.Fields = append(outputContract.Fields, resourceTypeOutputFieldDTO{
				Name:              field.Name,
				JSONType:          string(field.JSONType),
				RequiredWhenReady: field.RequiredWhenReady,
			})
		}
		detail.OutputContract = outputContract
	}
	return detail
}

func capabilityNames(contract application.ResourceContract) []string {
	capabilities := contract.Capabilities()
	names := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		names = append(names, string(capability))
	}
	return names
}

// listResourceTypes answers "what ResourceTypes can I create?" with contract
// summaries only, for any authenticated principal. Discovery is authenticated
// but globally readable in M11 (resourceType:read for every principal);
// unauthenticated callers never reach this handler. Capabilities are
// ResourceType-contract capabilities, not guarantees about current backend
// availability.
func (h *handler) listResourceTypes(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	contracts, err := h.service.ListResourceTypes(r.Context(), principal)
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			writeProblem(w, r, CodeForbidden, "you are not authorized to read ResourceType discovery", nil)
			return
		}
		h.mapReadError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newListDTO(contracts))
}

// getResourceType returns one developer contract including its embedded
// ResourceSpec schema to an authenticated principal. A missing name/version
// pair is a 404 because the request addresses a ResourceType entity directly;
// an authorization denial is 403 because discovery hides no per-type
// existence in M11.
func (h *handler) getResourceType(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	version := r.PathValue("version")
	if strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" ||
		strings.ContainsRune(name, '/') || strings.ContainsRune(version, '/') {
		writeProblem(w, r, CodeResourceTypeNotFound, "no ResourceType is registered with this name and version", nil)
		return
	}
	contract, err := h.service.GetResourceType(r.Context(), principal, domain.ResourceTypeRef{Name: name, Version: version})
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			writeProblem(w, r, CodeForbidden, "you are not authorized to read ResourceType discovery", nil)
			return
		}
		h.mapReadError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newResourceTypeDetailDTO(contract))
}

// writeJSON renders one success representation under the M7 no-store policy.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
