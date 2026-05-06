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
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// Reader is the narrow dependency for generic graph / E5 / aggregated read-path: a concurrent-safe
// view of the current registry snapshot (may be nil before the first successful Refresh).
type Reader interface {
	Current() *snapshot.GVKRegistry
}

// Provider holds the latest graph GVK registry, rebuilt on Refresh (e.g. after DSC reconcile).
// Current() is lock-free and always returns a fully built *GVKRegistry pointer or nil (atomic load).
// Refresh/TryRefresh serialize concurrent rebuilds (singleflight-style mutex); the new registry is
// swapped atomically so readers never observe a half-written registry.
type Provider struct {
	cfg    *config.Options
	mapper meta.RESTMapper
	reader client.Reader
	log    logr.Logger

	refreshMu sync.Mutex
	reg       atomic.Pointer[snapshot.GVKRegistry]
}

// NewProvider constructs a provider. Call Refresh before relying on Current(); until then Current() may be nil.
func NewProvider(cfg *config.Options, mapper meta.RESTMapper, apiReader client.Reader, log logr.Logger) (*Provider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("snapshot graph registry provider: config is nil")
	}
	if mapper == nil {
		return nil, fmt.Errorf("snapshot graph registry provider: RESTMapper is nil")
	}
	if apiReader == nil {
		return nil, fmt.Errorf("snapshot graph registry provider: API reader is nil")
	}
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	return &Provider{
		cfg:    cfg,
		mapper: mapper,
		reader: apiReader,
		log:    log,
	}, nil
}

// Current returns the last successfully built registry, or nil if Refresh has not run or failed.
// Callers must treat nil as "registry not ready" and fail closed when graph resolution is required.
func (p *Provider) Current() *snapshot.GVKRegistry {
	return p.reg.Load()
}

// Refresh rebuilds the registry from bootstrap + eligible DSC + RESTMapper and atomically swaps Current().
func (p *Provider) Refresh(ctx context.Context) error {
	return p.refresh(ctx)
}

// TryRefresh implements LiveReader; same semantics as Refresh (one full rebuild, atomic swap).
func (p *Provider) TryRefresh(ctx context.Context) error {
	return p.refresh(ctx)
}

func (p *Provider) refresh(ctx context.Context) error {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	reg, err := BuildRegistry(ctx, p.mapper, p.reader, p.cfg, p.log)
	if err != nil {
		return err
	}
	p.reg.Store(reg)
	return nil
}

// ReplaceCurrent swaps the registry without rebuilding (integration tests that augment pairs after Refresh).
func (p *Provider) ReplaceCurrent(reg *snapshot.GVKRegistry) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.reg.Store(reg)
}
