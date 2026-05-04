// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package kcp

import (
	"errors"
	"strings"

	"k8s.io/client-go/rest"
)

// ValidateKubeconfig checks that the given rest.Config points to a kcp
// workspace (i.e. the host URL contains /clusters/). This catches the most
// common misconfiguration — pointing at a plain Kubernetes cluster or at kcp
// without a workspace path.
func ValidateKubeconfig(cfg *rest.Config) error {
	if cfg == nil {
		return errors.New("kubeconfig is nil")
	}

	if !strings.Contains(cfg.Host, "/clusters/") {
		return errors.New("kubeconfig host does not contain a /clusters/ workspace path — ensure the kubeconfig points to a kcp workspace (e.g. https://host/clusters/root:my-workspace)")
	}

	return nil
}

// BaseConfig returns a copy of the given rest.Config with the /clusters/...
// workspace path stripped from the host URL, leaving only the front-proxy
// base URL. This base URL can be used to construct workspace-scoped clients
// by appending /clusters/<logicalClusterName>.
//
// Returns an error if the host URL does not contain a /clusters/ path.
func BaseConfig(cfg *rest.Config) (*rest.Config, error) {
	idx := strings.Index(cfg.Host, "/clusters/")
	if idx == -1 {
		return nil, errors.New("kubeconfig host does not contain a /clusters/ workspace path — cannot derive front-proxy base URL")
	}

	baseCfg := rest.CopyConfig(cfg)
	baseCfg.Host = cfg.Host[:idx]

	return baseCfg, nil
}
