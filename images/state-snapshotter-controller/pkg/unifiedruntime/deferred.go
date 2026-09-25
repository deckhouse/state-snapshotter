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

package unifiedruntime

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// RunWhenMappable waits for gvk to become RESTMappable and then calls wire once.
//
// It exists for the APIs this module wires at boot but does not install itself. The CSI VolumeSnapshot
// CRDs come from storage-foundation, which depends on this module, so on a fresh install this process
// starts first and finds them missing. Every boot-time registration then degrades to "not RESTMappable
// yet", and the only compensating path — Syncer.Sync — runs on CustomSnapshotDefinition reconciles alone,
// which a cluster with no CSDs never has. The pair would stay unwired until the pod restarted: a user's
// VolumeSnapshot is claimed by its domain controller and never gets a SnapshotContent.
//
// wire is retried on the same interval while it returns an error. The function returns when wire
// succeeds or ctx is done; a ctx that ends first is not an error.
func RunWhenMappable(
	ctx context.Context,
	log logr.Logger,
	mapper meta.RESTMapper,
	gvk schema.GroupVersionKind,
	interval time.Duration,
	wire func(context.Context) error,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err == nil {
			werr := wire(ctx)
			if werr == nil {
				log.Info("wired the API that was not served at startup", "gvk", gvk.String())
				return nil
			}
			log.Error(werr, "wiring the API that appeared after startup failed; retrying", "gvk", gvk.String())
		} else if !meta.IsNoMatchError(err) {
			log.V(1).Info("RESTMapping lookup failed; retrying", "gvk", gvk.String(), "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
