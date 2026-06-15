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

package demo

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/restoretransform"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// AddRestoreTransformServerToManager registers the demo domain's restore-transform HTTP endpoint as a
// manager Runnable (ADR 2026-06-13, PoC transport). It is a no-op unless RESTORE_TRANSFORM_ENDPOINT is
// set (i.e. the generic restore client is configured to delegate over REST).
//
// Architectural boundary (read this before copying the pattern): the transform *endpoint* belongs to
// the *domain* controller, not to generic core. The generic restore compiler holds only the
// transport-agnostic DomainRestoreTransformer interface and a REST *client*. This endpoint lives in
// the same binary in the PoC ONLY because the demo domain itself is bundled into this binary; it is
// wired through the demo package's own manager registration, never started by generic main/core. In
// production a real domain controller serves this endpoint from its own module/binary, and generic
// keeps just the client.
//
// PoC shortcut: the listen host:port is derived from the same URL the generic client calls, so a
// single env wires both sides for the demo. A production domain controller would own its own listen
// configuration independently of the client's endpoint URL.
func AddRestoreTransformServerToManager(mgr ctrl.Manager, log logger.LoggerInterface) error {
	endpoint := os.Getenv(restoretransform.EnvEndpoint)
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Do not abort the whole controller for a demo endpoint misconfiguration: log clearly and skip.
		// The generic REST client then fails restore calls loudly (fail-whole / fail-closed) rather
		// than producing a partial restore.
		log.Error(err, "[demo] invalid RESTORE_TRANSFORM_ENDPOINT; demo restore-transform endpoint not started", "endpoint", endpoint)
		return nil
	}

	mux := http.NewServeMux()
	NewRestoreTransformHandler().SetupRoutes(mux)
	srv := &http.Server{Addr: u.Host, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		log.Info("[demo] serving domain restore-transform endpoint (PoC transport; owned by demo domain, not generic core)", "addr", u.Host, "path", u.Path)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}))
}
