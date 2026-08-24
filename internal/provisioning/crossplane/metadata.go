// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"strconv"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// Private correlation metadata carried on every Liftr-owned XR. Labels hold
// only label-legal digest values; annotations carry the full opaque
// identifiers. Ownership metadata is stable across updates and retries;
// execution correlation changes with each admitted Operation. No
// user-editable ResourceSpec field participates.
const (
	labelManagedBy                = "app.kubernetes.io/managed-by"
	labelPlatform                 = "liftr.io/platform"
	annotationResourceID          = "liftr.io/resource-id"
	annotationResourceType        = "liftr.io/resource-type"
	annotationTargetGenerationKey = "liftr.io/target-generation"
	annotationOperationID         = "liftr.io/operation-id"
	managedByValue                = "liftr"
)

type identityMetadata struct {
	platformDigest string
	resourceID     domain.ResourceID
	resourceType   domain.ResourceTypeRef
}

func (i identityMetadata) stamp(object map[string]any) {
	metadata, _ := object["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	labels, _ := metadata["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels[labelManagedBy] = managedByValue
	labels[labelPlatform] = i.platformDigest
	metadata["labels"] = labels

	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations[annotationResourceID] = string(i.resourceID)
	annotations[annotationResourceType] = resourceTypeAnnotation(i.resourceType)
	metadata["annotations"] = annotations
}

func (i identityMetadata) verify(object *kube.Object) bool {
	if value, ok := object.LabelString(labelManagedBy); !ok || value != managedByValue {
		return false
	}
	if value, ok := object.LabelString(labelPlatform); !ok || value != i.platformDigest {
		return false
	}
	if value, ok := object.AnnotationString(annotationResourceID); !ok || value != string(i.resourceID) {
		return false
	}
	value, ok := object.AnnotationString(annotationResourceType)
	return ok && value == resourceTypeAnnotation(i.resourceType)
}

// operationCorrelation is the per-execution half of the metadata: it may be
// absent on foreign or stale objects without saying anything about physical
// presence or Liftr ownership.
func stampOperationCorrelation(object map[string]any, operationID domain.OperationID, targetGeneration uint64) {
	metadata, _ := object["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		object["metadata"] = metadata
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations[annotationOperationID] = string(operationID)
	annotations[annotationTargetGenerationKey] = strconv.FormatUint(targetGeneration, 10)
}

// operationCorrelated reports whether the live object carries exactly the
// requesting Operation's execution correlation.
func operationCorrelated(object *kube.Object, operationID domain.OperationID, targetGeneration uint64) bool {
	id, ok := object.AnnotationString(annotationOperationID)
	if !ok || id != string(operationID) {
		return false
	}
	generation, ok := object.AnnotationString(annotationTargetGenerationKey)
	if !ok {
		return false
	}
	parsed, err := strconv.ParseUint(generation, 10, 64)
	return err == nil && parsed == targetGeneration
}

// annotationTargetGeneration reads the stamped Liftr generation, which both
// execution and passive observation require to be current before any Ready
// fact may be reported.
func annotationTargetGeneration(object *kube.Object) (uint64, bool) {
	value, ok := object.AnnotationString(annotationTargetGenerationKey)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
