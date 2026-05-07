// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package kcp

import (
	"testing"

	"k8s.io/client-go/rest"
)

func TestValidateKubeconfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *rest.Config
		wantErr bool
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: true,
		},
		{
			name:    "plain kubernetes URL",
			cfg:     &rest.Config{Host: "https://localhost:6443"},
			wantErr: true,
		},
		{
			name:    "kcp URL without workspace path",
			cfg:     &rest.Config{Host: "https://kcp.example.com"},
			wantErr: true,
		},
		{
			name:    "valid kcp workspace URL",
			cfg:     &rest.Config{Host: "https://kcp.example.com/clusters/root:dep-ctrl"},
			wantErr: false,
		},
		{
			name:    "valid kcp root URL",
			cfg:     &rest.Config{Host: "https://kcp.example.com/clusters/root"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKubeconfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateKubeconfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBaseConfig(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		wantHost string
		wantErr  bool
	}{
		{
			name:    "no clusters path",
			host:    "https://localhost:6443",
			wantErr: true,
		},
		{
			name:     "strips workspace path",
			host:     "https://kcp.example.com/clusters/root:dep-ctrl",
			wantHost: "https://kcp.example.com",
			wantErr:  false,
		},
		{
			name:     "strips root path",
			host:     "https://kcp.example.com/clusters/root",
			wantHost: "https://kcp.example.com",
			wantErr:  false,
		},
		{
			name:     "preserves port",
			host:     "https://localhost:31443/clusters/root:dep-ctrl",
			wantHost: "https://localhost:31443",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &rest.Config{Host: tt.host}
			baseCfg, err := BaseConfig(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("BaseConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if baseCfg.Host != tt.wantHost {
				t.Errorf("BaseConfig() host = %q, want %q", baseCfg.Host, tt.wantHost)
			}
			// Verify original config is not mutated.
			if cfg.Host != tt.host {
				t.Errorf("original config mutated: host = %q, want %q", cfg.Host, tt.host)
			}
		})
	}
}
