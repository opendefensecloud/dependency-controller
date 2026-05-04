// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

// RuleRegistry is a thread-safe registry of active DependencyRules and their
// metadata. It maintains a reverse index from dependency target GVRs to rule
// keys, enabling the webhook to quickly find which rules protect a given
// resource type.
type RuleRegistry struct {
	mu       sync.RWMutex
	rules    map[string]*RuleState
	byTarget map[schema.GroupVersionResource][]string // GVR -> rule keys
}

// RuleState holds the metadata for a single DependencyRule.
type RuleState struct {
	Rule         v1alpha1.DependencyRuleSpec
	DependentGVK schema.GroupVersionKind
	DependentGVR schema.GroupVersionResource
	IndexFields  []IndexedField
}

// IndexedField maps a field path on the dependent resource to the dependency
// target GVR it resolves references for.
type IndexedField struct {
	FieldPath string
	TargetGVR schema.GroupVersionResource
}

// RuleEntry is returned by FindByTargetGVR and pairs a rule state with the
// specific indexed fields that match the queried GVR.
type RuleEntry struct {
	Key          string
	State        *RuleState
	MatchedField IndexedField
}

func NewRuleRegistry() *RuleRegistry {
	return &RuleRegistry{
		rules:    make(map[string]*RuleState),
		byTarget: make(map[schema.GroupVersionResource][]string),
	}
}

// Register adds a rule to the registry and rebuilds the reverse index.
// If a state was already registered under this key, it is replaced and
// the old state is returned so the caller can detect duplicates.
func (r *RuleRegistry) Register(key string, state *RuleState) *RuleState {
	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.rules[key]
	r.rules[key] = state
	r.rebuildTargetIndex()

	return old
}

// Unregister removes a rule from the registry and rebuilds the reverse index.
func (r *RuleRegistry) Unregister(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.rules[key]; !exists {
		return
	}

	delete(r.rules, key)
	r.rebuildTargetIndex()
}

// Exists returns true if the given rule key is registered.
func (r *RuleRegistry) Exists(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.rules[key]

	return exists
}

// Get returns the rule state for the given key, or nil if not found.
func (r *RuleRegistry) Get(key string) *RuleState {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.rules[key]
}

// FindByTargetGVR returns all rules that have a dependency target matching
// the given GVR, along with the specific indexed field that matched.
func (r *RuleRegistry) FindByTargetGVR(gvr schema.GroupVersionResource) []RuleEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := r.byTarget[gvr]
	if len(keys) == 0 {
		return nil
	}

	var entries []RuleEntry
	for _, key := range keys {
		state := r.rules[key]
		if state == nil {
			continue
		}
		for _, f := range state.IndexFields {
			if f.TargetGVR == gvr {
				entries = append(entries, RuleEntry{
					Key:          key,
					State:        state,
					MatchedField: f,
				})
			}
		}
	}

	return entries
}

// AllTargetGVRs returns the deduplicated set of all dependency target GVRs
// across all registered rules.
func (r *RuleRegistry) AllTargetGVRs() []schema.GroupVersionResource {
	r.mu.RLock()
	defer r.mu.RUnlock()

	gvrs := make([]schema.GroupVersionResource, 0, len(r.byTarget))
	for gvr := range r.byTarget {
		gvrs = append(gvrs, gvr)
	}

	return gvrs
}

// rebuildTargetIndex rebuilds the byTarget reverse index from scratch.
// Must be called with the write lock held.
func (r *RuleRegistry) rebuildTargetIndex() {
	r.byTarget = make(map[schema.GroupVersionResource][]string)
	for key, state := range r.rules {
		for _, f := range state.IndexFields {
			r.byTarget[f.TargetGVR] = append(r.byTarget[f.TargetGVR], key)
		}
	}
}
