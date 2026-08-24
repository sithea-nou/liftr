// SPDX-License-Identifier: Apache-2.0

// Package fakeapi provides a deterministic in-process Kubernetes-style API
// server for the Crossplane adapter tests. It implements just enough REST
// semantics — namespaced CRUD, UID delete preconditions, resourceVersion
// preconditions on apply patches, finalizer-aware deletion, generation
// bumping on spec changes, and scripted controller reactions — so adapter
// tests exercise true asynchronous declarative behavior without a real
// cluster or credentials.
package fakeapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// Object is one stored resource with the runtime physical identity the fake
// tracks for precondition enforcement.
type Object struct {
	Raw             map[string]any
	UID             string
	ResourceVersion uint64
	Finalizers      []string
	polls           int
}

func (o *Object) clone() *Object {
	raw := rawDeepCopy(o.Raw)
	metadata, _ := raw["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["uid"] = o.UID
	metadata["resourceVersion"] = strconv.FormatUint(o.ResourceVersion, 10)
	if len(o.Finalizers) > 0 {
		finalizers := make([]string, len(o.Finalizers))
		copy(finalizers, o.Finalizers)
		metadata["finalizers"] = finalizers
	} else {
		delete(metadata, "finalizers")
	}
	return &Object{Raw: raw, UID: o.UID, ResourceVersion: o.ResourceVersion, Finalizers: o.Finalizers}
}

// Generation returns the live metadata.generation value.
func (o *Object) Generation() uint64 {
	metadata, _ := o.Raw["metadata"].(map[string]any)
	generation, _ := metadata["generation"].(float64)
	return uint64(generation)
}

// AnnotationValue reads one metadata annotation as a string.
func (o *Object) AnnotationValue(key string) string {
	metadata, _ := o.Raw["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	value, _ := annotations[key].(string)
	return value
}

// SetFinalizer installs a finalizer, modelling a webhook.
func (o *Object) SetFinalizer(name string) {
	o.Finalizers = append(o.Finalizers, name)
}

func rawDeepCopy(value map[string]any) map[string]any {
	target := make(map[string]any, len(value))
	for key, entry := range value {
		switch typed := entry.(type) {
		case map[string]any:
			target[key] = rawDeepCopy(typed)
		case []any:
			copied := make([]any, len(typed))
			for index, item := range typed {
				if asMap, ok := item.(map[string]any); ok {
					copied[index] = rawDeepCopy(asMap)
				} else {
					copied[index] = item
				}
			}
			target[key] = copied
		default:
			target[key] = entry
		}
	}
	return target
}

// Controller is the deterministic reconciliation hook. It runs under the
// server lock after every successful GET of a live object; poll counts GETs
// of that object since it last (re)appeared, starting at 1.
type Controller func(poll int, object *Object)

// FailureInjector can return an API error instead of executing a verb.
type FailureInjector func(method string) *kube.APIError // Server is the fake control plane.
type Server struct {
	server *httptest.Server

	mu          sync.Mutex
	nextRV      uint64
	nextUID     uint64
	writeCount  int
	objects     map[string]*Object // key: namespace/plural/name
	controller  Controller
	failures    FailureInjector
	getFailures int

	// armedWriteFailure and commitFirst model transport loss on create:
	// with commitFirst the write lands and only the response is lost, which
	// is exactly the ambiguity window Liftr must survive.
	armedWriteFailure *kube.APIError
	writeFailuresLeft int
	commitFirst       bool

	// beforeNextWrite runs once under the server lock immediately before the
	// next mutating verb. It deterministically simulates out-of-band races,
	// such as a foreign actor replacing a verified object.
	beforeNextWrite func(s *Server)

	// Discovery state: families are learned from successful object traffic;
	// retired plurals are definitively unserved; discovery failures model
	// authorization denials, server faults, and transport uncertainty.
	familyGV           map[string]string // plural -> group/version at first sight
	knownGroupVersions map[string]struct{}
	retiredPlurals     map[string]struct{}
	discoverFailures   int
	discoverErr        *kube.APIError

	// updateFailureArmed routes the armed write failure to PUT instead of
	// POST.
	updateFailureArmed bool
}

// New starts a plain-HTTP fake server and returns its base URL.
func New(t *testing.T) (*Server, string) {
	t.Helper()
	s := &Server{
		objects:            map[string]*Object{},
		familyGV:           map[string]string{},
		knownGroupVersions: map[string]struct{}{},
		retiredPlurals:     map[string]struct{}{},
		nextRV:             100,
		nextUID:            1000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s, s.server.URL
}

// SetController installs the per-poll reconciliation hook.
func (s *Server) SetController(controller Controller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controller = controller
}

// FailNextGets injects count API failures for GETs before succeeding again.
// Use it to model control-plane uncertainty.
func (s *Server) FailNextGets(err *kube.APIError, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = func(string) *kube.APIError { return err }
	s.getFailures = count
}

// FailNextCreates injects count create failures. When commitFirst is true
// the create is fully applied before the failure response, modelling a
// dropped connection after persistence; otherwise nothing is stored.
func (s *Server) FailNextCreates(err *kube.APIError, count int, commitFirst bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armedWriteFailure = err
	s.writeFailuresLeft = count
	s.commitFirst = commitFirst
}

// ArmBeforeNextWrite schedules one mutation to run under the server lock
// immediately before the next POST/PATCH/DELETE. The hook must use the
// exported lock-free primitives of the server (Put/Mutate are not safe here;
// use putLocked/mutateLocked via the passed receiver).
func (s *Server) ArmBeforeNextWrite(mutation func(s *Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeNextWrite = mutation
}

// SetDiscoveryServed marks one plural as served (true, the default after
// first sight) or retired from discovery. Retirement models CRD removal:
// the API kind stops being served even if instance objects linger.
func (s *Server) SetDiscoveryServed(plural string, served bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if served {
		delete(s.retiredPlurals, plural)
		return
	}
	s.retiredPlurals[plural] = struct{}{}
}

// RetireCRD removes the API kind from discovery and wipes every stored
// instance of it, modelling what actually happens when a CRD disappears.
func (s *Server) RetireCRD(namespace, plural string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retiredPlurals[plural] = struct{}{}
	for key := range s.objects {
		if strings.HasPrefix(key, namespace+"/"+plural+"/") {
			delete(s.objects, key)
		}
	}
}

// FailNextDiscoveries injects count discovery failures. Any injected error
// yields ServedUnknown: authorization denials and server faults must never
// become definitive answers.
func (s *Server) FailNextDiscoveries(err *kube.APIError, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoverErr = err
	s.discoverFailures = count
}

// FailNextUpdates injects update failures. With commitFirst the update
// lands before the failure response, modelling a dropped connection after
// persistence.
func (s *Server) FailNextUpdates(err *kube.APIError, count int, commitFirst bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armedWriteFailure = err
	s.writeFailuresLeft = count
	s.commitFirst = commitFirst
	s.updateFailureArmed = true
}

// MutateLocked applies fn to a stored object. For use inside
// beforeNextWrite hooks only.
func (s *Server) MutateLocked(namespace, plural, name string, fn func(object *Object)) bool {
	key := s.key(namespace, plural, name)
	stored, ok := s.objects[key]
	if !ok {
		return false
	}
	fn(stored)
	s.refreshMetadataLocked(stored)
	return true
}

// PutLocked installs an object with fresh runtime identity. For use inside
// beforeNextWrite hooks only.
func (s *Server) PutLocked(namespace, plural, name string, raw map[string]any) {
	stored := &Object{Raw: rawDeepCopy(raw), UID: fmt.Sprintf("uid-%d", s.nextUID), ResourceVersion: s.nextRV}
	s.nextUID++
	s.nextRV++
	s.materialize(stored, namespace, name)
	s.objects[s.key(namespace, plural, name)] = stored
}

// Put installs or replaces an object wholesale, assigning fresh runtime
// identity. It models out-of-band writes such as a foreign actor recreating
// an object under a known name.
func (s *Server) Put(namespace, plural, name string, raw map[string]any) *Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := &Object{Raw: rawDeepCopy(raw), UID: fmt.Sprintf("uid-%d", s.nextUID), ResourceVersion: s.nextRV}
	s.nextUID++
	s.nextRV++
	s.materialize(stored, namespace, name)
	s.objects[s.key(namespace, plural, name)] = stored
	return stored.clone()
}

// Get returns a snapshot of one stored object.
func (s *Server) Get(namespace, plural, name string) (*Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.objects[s.key(namespace, plural, name)]
	if !ok {
		return nil, false
	}
	return stored.clone(), true
}

// AllNames lists the stored object names in one namespace/resource family.
func (s *Server) AllNames(namespace, plural string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := namespace + "/" + plural + "/"
	names := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			names = append(names, strings.TrimPrefix(key, prefix))
		}
	}
	sort.Strings(names)
	return names
}

// Mutate applies fn to the stored object under the server lock. The hook
// receives live internals; it must not retain references.
func (s *Server) Mutate(namespace, plural, name string, fn func(object *Object)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(namespace, plural, name)
	stored, ok := s.objects[key]
	if !ok {
		return false
	}
	fn(stored)
	s.refreshMetadataLocked(stored)
	if s.terminatingWithoutFinalizers(stored) {
		delete(s.objects, key)
	}
	return true
}

// RemoveFinalizer clears every finalizer from the named object so a pending
// deletion can complete on the next DELETE.
func (s *Server) RemoveFinalizers(namespace, plural, name string) bool {
	return s.Mutate(namespace, plural, name, func(object *Object) { object.Finalizers = nil })
}

// WriteCount reports how many mutating calls (POST/PATCH/DELETE) reached the
// server. Output-recovery observations must leave this unchanged.
func (s *Server) WriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCount
}

func (s *Server) key(namespace, plural, name string) string {
	return namespace + "/" + plural + "/" + name
}

// recordFamily learns the group/version serving one plural from successful
// object traffic, and remembers the group/version as existing so a fully
// retired family still answers discovery with an empty definitive list.
func (s *Server) recordFamily(gv, plural string) {
	if gv == "" {
		return
	}
	s.knownGroupVersions[gv] = struct{}{}
	if _, exists := s.familyGV[plural]; !exists {
		s.familyGV[plural] = gv
	}
}

// RegisterFamily explicitly declares one API resource as served by the
// given group/version, independent of any object traffic.
func (s *Server) RegisterFamily(group, version, plural string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gv := group + "/" + version
	s.knownGroupVersions[gv] = struct{}{}
	if _, exists := s.familyGV[plural]; !exists {
		s.familyGV[plural] = gv
	}
}

func (s *Server) materialize(stored *Object, namespace, name string) {
	metadata, _ := stored.Raw["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["name"] = name
	metadata["namespace"] = namespace
	metadata["uid"] = stored.UID
	metadata["resourceVersion"] = strconv.FormatUint(stored.ResourceVersion, 10)
	generation, _ := metadata["generation"].(float64)
	if generation == 0 {
		metadata["generation"] = float64(1)
	}
	if _, exists := metadata["creationTimestamp"]; !exists {
		metadata["creationTimestamp"] = "2026-01-01T00:00:00Z"
	}
	stored.Raw["metadata"] = metadata
}

func (s *Server) refreshMetadataLocked(stored *Object) {
	metadata, _ := stored.Raw["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["uid"] = stored.UID
	metadata["resourceVersion"] = strconv.FormatUint(stored.ResourceVersion, 10)
	stored.Raw["metadata"] = metadata
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.Method == http.MethodGet && s.failures != nil && s.getFailures > 0 {
		err := s.failures(r.Method)
		s.getFailures--
		if s.getFailures == 0 {
			s.failures = nil
		}
		writeStatus(w, err.Code, err.Reason, err.Message)
		return
	}

	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/") {
		segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/"), "/")
		if len(segments) == 2 {
			s.handleDiscovery(w, segments[0], segments[1])
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/apis/")
	segments := strings.Split(path, "/")
	// .../{group}/{version}/namespaces/{ns}/{plural}[/{name}]
	if len(segments) < 5 || segments[2] != "namespaces" {
		writeStatus(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource")
		return
	}
	namespace, plural := segments[3], segments[4]
	name := ""
	if len(segments) >= 6 {
		name = segments[5]
	}

	gv := segments[0] + "/" + segments[1]

	if r.Method != http.MethodGet {
		s.writeCount++
		if s.beforeNextWrite != nil {
			hook := s.beforeNextWrite
			s.beforeNextWrite = nil
			if hook != nil {
				hook(s)
			}
		}
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, gv, namespace, plural, name)
	case http.MethodPost:
		s.handleCreate(w, r, gv, namespace, plural)
	case http.MethodPut:
		s.handleUpdate(w, r, gv, namespace, plural, name)
	case http.MethodDelete:
		s.handleDelete(w, r, gv, namespace, plural, name)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method")
	}
}

// handleDiscovery serves the structured group-version discovery document.
// Families are learned from successful object traffic; retired plurals are
// omitted; an unknown group/version is a definitive 404. Injected failures
// model authorization denials and control-plane faults as uncertainty.
func (s *Server) handleDiscovery(w http.ResponseWriter, group, version string) {
	gv := group + "/" + version
	plurals := make([]string, 0, len(s.familyGV))
	for plural, family := range s.familyGV {
		if family != gv {
			continue
		}
		if _, retired := s.retiredPlurals[plural]; retired {
			continue
		}
		plurals = append(plurals, plural)
	}
	if len(plurals) == 0 {
		_, known := s.knownGroupVersions[gv]
		if !known {
			writeStatus(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource")
			return
		}
	}
	if s.discoverFailures > 0 {
		s.discoverFailures--
		err := s.discoverErr
		if s.discoverFailures == 0 {
			s.discoverErr = nil
		}
		writeStatus(w, err.Code, err.Reason, err.Message)
		return
	}
	sort.Strings(plurals)
	resources := make([]any, 0, len(plurals))
	for _, plural := range plurals {
		resources = append(resources, map[string]any{"name": plural, "namespaced": true})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": gv,
		"resources":    resources,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, gv, namespace, plural, name string) {
	key := s.key(namespace, plural, name)
	stored, ok := s.objects[key]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource")
		return
	}
	s.recordFamily(gv, plural)
	stored.polls++
	if s.controller != nil {
		s.controller(stored.polls, stored)
		s.refreshMetadataLocked(stored)
	}
	if s.terminatingWithoutFinalizers(stored) {
		// Foreground garbage collection: once nothing holds the object back,
		// it disappears for subsequent readers.
		defer delete(s.objects, key)
	}
	writeObject(w, stored.clone())
}

func (s *Server) terminatingWithoutFinalizers(stored *Object) bool {
	metadata, _ := stored.Raw["metadata"].(map[string]any)
	if metadata == nil {
		return false
	}
	if _, terminating := metadata["deletionTimestamp"]; !terminating {
		return false
	}
	return len(stored.Finalizers) == 0
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, gv, namespace, plural string) {
	document, ok := readBody(r, w)
	if !ok {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(document, &raw); err != nil {
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "the request body was not a valid object")
		return
	}
	metadata, _ := raw["metadata"].(map[string]any)
	objectName, _ := metadata["name"].(string)
	if objectName == "" {
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "object metadata.name is required")
		return
	}
	key := s.key(namespace, plural, objectName)
	if _, exists := s.objects[key]; exists {
		writeStatus(w, http.StatusConflict, "AlreadyExists", "the object already exists")
		return
	}
	failNow := s.writeFailuresLeft > 0 && !s.updateFailureArmed
	var injected *kube.APIError
	commitFirst := false
	if failNow {
		injected = s.armedWriteFailure
		commitFirst = s.commitFirst
		s.consumeWriteFailure()
	}
	if failNow && !commitFirst {
		// Transport loss before persistence: nothing is stored.
		writeStatus(w, injected.Code, injected.Reason, injected.Message)
		return
	}
	stored := &Object{Raw: rawDeepCopy(raw), UID: fmt.Sprintf("uid-%d", s.nextUID), ResourceVersion: s.nextRV}
	s.nextUID++
	s.nextRV++
	s.materialize(stored, namespace, objectName)
	s.objects[key] = stored
	s.recordFamily(gv, plural)
	if failNow {
		// Transport loss after persistence: the write landed but Liftr never
		// saw success.
		writeStatus(w, injected.Code, injected.Reason, injected.Message)
		return
	}
	writeObject(w, stored.clone())
}

func (s *Server) consumeWriteFailure() {
	s.writeFailuresLeft--
	if s.writeFailuresLeft == 0 {
		s.armedWriteFailure = nil
		s.commitFirst = false
		s.updateFailureArmed = false
	}
}

// handleUpdate implements conditional full-object updates: the request must
// carry metadata.resourceVersion equal to the live object's version or the
// write is rejected with a conflict. The spec is replaced wholesale; server-
// owned identity fields survive.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, gv, namespace, plural, name string) {
	key := s.key(namespace, plural, name)
	stored, ok := s.objects[key]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource")
		return
	}
	s.recordFamily(gv, plural)
	document, ok := readBody(r, w)
	if !ok {
		return
	}
	var replacement map[string]any
	if err := json.Unmarshal(document, &replacement); err != nil {
		writeStatus(w, http.StatusUnprocessableEntity, "Invalid", "the update body was not valid")
		return
	}
	metadata, _ := replacement["metadata"].(map[string]any)
	requiredRV, _ := metadata["resourceVersion"].(string)
	delete(metadata, "resourceVersion")
	delete(metadata, "uid")
	if requiredRV != "" && requiredRV != strconv.FormatUint(stored.ResourceVersion, 10) {
		writeStatus(w, http.StatusConflict, "Conflict",
			fmt.Sprintf("the object has been modified; please apply your changes to the latest version (%s)", requiredRV))
		return
	}
	failNow := s.writeFailuresLeft > 0 && s.updateFailureArmed
	var injected *kube.APIError
	if failNow {
		injected = s.armedWriteFailure
		s.consumeWriteFailure()
	}
	specBefore := rawDeepCopy(specOf(stored.Raw))
	replacementMetadata := stored.Raw["metadata"].(map[string]any)
	liveLabels, _ := replacementMetadata["labels"].(map[string]any)
	newLabels, _ := metadata["labels"].(map[string]any)
	for keyLabel, value := range newLabels {
		liveLabels[keyLabel] = rawDeepCopyValue(value)
	}
	liveAnnotations, _ := replacementMetadata["annotations"].(map[string]any)
	newAnnotations, _ := metadata["annotations"].(map[string]any)
	for keyAnnotation, value := range newAnnotations {
		liveAnnotations[keyAnnotation] = rawDeepCopyValue(value)
	}
	if desiredSpec, ok := replacement["spec"].(map[string]any); ok {
		stored.Raw["spec"] = rawDeepCopy(desiredSpec)
	}
	specChanged := !equalJSON(specBefore, specOf(stored.Raw))
	stored.ResourceVersion = s.nextRV
	s.nextRV++
	if specChanged {
		generation, _ := replacementMetadata["generation"].(float64)
		replacementMetadata["generation"] = generation + 1
	}
	s.refreshMetadataLocked(stored)
	if failNow {
		// With commitFirst the update landed above and only the response is
		// lost; without it nothing changed because the rejection preceded
		// the mutation block.
		writeStatus(w, injected.Code, injected.Reason, injected.Message)
		return
	}
	writeObject(w, stored.clone())
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, gv, namespace, plural, name string) {
	key := s.key(namespace, plural, name)
	stored, ok := s.objects[key]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", "the server reported the target as absent")
		return
	}
	s.recordFamily(gv, plural)
	var options struct {
		Preconditions struct {
			UID string `json:"uid"`
		} `json:"preconditions"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &options); err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest", "delete options were not valid")
			return
		}
	}
	if options.Preconditions.UID != "" && options.Preconditions.UID != stored.UID {
		writeStatus(w, http.StatusConflict, "Conflict",
			fmt.Sprintf("the UID in the precondition does not match the existing object %q", name))
		return
	}
	if len(stored.Finalizers) > 0 {
		metadata, _ := stored.Raw["metadata"].(map[string]any)
		metadata["deletionTimestamp"] = "2026-01-01T00:00:05Z"
		stored.ResourceVersion = s.nextRV
		s.nextRV++
		s.refreshMetadataLocked(stored)
		writeObject(w, stored.clone())
		return
	}
	delete(s.objects, key)
	writeObject(w, stored.clone())
}

func specOf(raw map[string]any) map[string]any {
	typed, _ := raw["spec"].(map[string]any)
	if typed == nil {
		return map[string]any{}
	}
	return typed
}

func mergeInto(target map[string]any, patch map[string]any) {
	for key, value := range patch {
		if asMap, ok := value.(map[string]any); ok {
			existing, ok := target[key].(map[string]any)
			if !ok {
				existing = map[string]any{}
			}
			mergeInto(existing, asMap)
			target[key] = existing
			continue
		}
		target[key] = rawDeepCopyValue(value)
	}
}

func rawDeepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return rawDeepCopy(typed)
	case []any:
		copied := make([]any, len(typed))
		for index, entry := range typed {
			if asMap, ok := entry.(map[string]any); ok {
				copied[index] = rawDeepCopy(asMap)
			} else {
				copied[index] = entry
			}
		}
		return copied
	default:
		return typed
	}
}

func equalJSON(left, right map[string]any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return string(leftBytes) == string(rightBytes)
}

func readBody(r *http.Request, w http.ResponseWriter) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "could not read request body")
		return nil, false
	}
	return body, true
}

func writeObject(w http.ResponseWriter, object *Object) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(object.Raw)
}

func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    message,
		"reason":     reason,
		"code":       code,
	})
}
