// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

// RuleRegistry is a thread-safe registry of active DependencyRules and their
// associated multicluster managers. It maintains a reverse index from dependency
// target GVRs to rule keys, enabling the webhook to quickly find which rules
// protect a given resource type.
type RuleRegistry struct {
	mu       sync.RWMutex
	rules    map[string]*RuleState
	byTarget map[schema.GroupVersionResource][]string // GVR -> rule keys
}

// RuleState holds the runtime state for a single DependencyRule.
type RuleState struct {
	Manager      mcmanager.Manager
	Cancel       func()
	Rule         v1alpha1.DependencyRuleSpec
	DependentGVK schema.GroupVersionKind
	IndexFields  []IndexedField
	Ready        bool

	mu            sync.Mutex
	knownClusters map[string]struct{}
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
func (r *RuleRegistry) Register(key string, state *RuleState) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.rules[key] = state
	r.rebuildTargetIndex()
}

// Unregister removes a rule from the registry, cancels its manager, and
// rebuilds the reverse index.
func (r *RuleRegistry) Unregister(key string) {
	r.mu.Lock()
	state, exists := r.rules[key]
	if !exists {
		r.mu.Unlock()
		return
	}
	delete(r.rules, key)
	r.rebuildTargetIndex()
	r.mu.Unlock()

	// Cancel outside the lock to avoid holding it during teardown.
	state.Cancel()
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

// AllDependentGVRs returns the deduplicated set of all dependent resource GVRs
// across all registered rules.
func (r *RuleRegistry) AllDependentGVRs() []schema.GroupVersionResource {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[schema.GroupVersionResource]struct{})
	var gvrs []schema.GroupVersionResource
	for _, state := range r.rules {
		dep := state.Rule.Dependent
		gvr := schema.GroupVersionResource{
			Group:    dep.Group,
			Version:  dep.Version,
			Resource: dep.Resource,
		}
		if _, ok := seen[gvr]; !ok {
			seen[gvr] = struct{}{}
			gvrs = append(gvrs, gvr)
		}
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

// TrackCluster records that the given rule has seen a resource in the given cluster.
func (r *RuleRegistry) TrackCluster(key, clusterName string) {
	r.mu.RLock()
	state := r.rules[key]
	r.mu.RUnlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.knownClusters == nil {
		state.knownClusters = make(map[string]struct{})
	}
	state.knownClusters[clusterName] = struct{}{}
}

// MarkReady marks a rule's cache as synced and ready for queries.
func (r *RuleRegistry) MarkReady(key string) {
	r.mu.RLock()
	state := r.rules[key]
	r.mu.RUnlock()
	if state != nil {
		state.Ready = true
	}
}
