// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package kcp

import (
	"context"
	"fmt"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FindEndpointSlice finds the APIExportEndpointSlice whose spec.export.name
// matches the given APIExport name. The slice name is not guaranteed to match
// the APIExport name — service providers can create custom slices with
// different names.
func FindEndpointSlice(ctx context.Context, c client.Reader, apiExportName string) (*apisv1alpha1.APIExportEndpointSlice, error) {
	var list apisv1alpha1.APIExportEndpointSliceList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing APIExportEndpointSlices: %w", err)
	}

	for i := range list.Items {
		if list.Items[i].Spec.APIExport.Name == apiExportName {
			return &list.Items[i], nil
		}
	}

	return nil, fmt.Errorf("no APIExportEndpointSlice found for APIExport %q", apiExportName)
}
