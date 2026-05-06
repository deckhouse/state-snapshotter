/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapshotgraphregistry

import (
	"context"
	"errors"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// ErrRefreshNotConfigured is returned by TryRefresh on readers that do not support rebuild (e.g. tests
// using a fixed *GVKRegistry). Generic retry-on-miss logic must not treat this as a fatal refresh error.
var ErrRefreshNotConfigured = errors.New("snapshot graph registry: TryRefresh not configured for this reader")

// ErrGraphRegistryNotReady means Current() is nil or unusable after an attempted rebuild (e.g. first Refresh
// never succeeded). Callers that need heterogeneous graph resolution should map this to 503 / fail-closed.
var ErrGraphRegistryNotReady = errors.New("snapshot graph registry not ready")

// LiveReader is a registry view that can be rebuilt from bootstrap + eligible CSD + RESTMapper.
// Production uses *Provider; tests may use Static with a frozen registry.
type LiveReader interface {
	Reader
	TryRefresh(context.Context) error
}

// Static wraps a fixed registry for tests or callers without dynamic refresh. TryRefresh returns
// ErrRefreshNotConfigured so generic code performs at most one logical attempt without a refresh storm.
type Static struct {
	reg *snapshot.GVKRegistry
}

// NewStatic returns a LiveReader backed by reg (may be nil). TryRefresh is a no-op error.
func NewStatic(reg *snapshot.GVKRegistry) *Static {
	return &Static{reg: reg}
}

func (s *Static) Current() *snapshot.GVKRegistry {
	if s == nil {
		return nil
	}
	return s.reg
}

func (s *Static) TryRefresh(context.Context) error {
	return ErrRefreshNotConfigured
}
