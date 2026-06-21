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

package restore

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DomainSubtreeRestorer restores a domain snapshot subtree out-of-process. When the restore compiler
// reaches a domain snapshot node (its kind is owned by a separate domain controller), it does not
// compile that subtree in-process; it delegates the whole subtree to the domain's aggregated apiserver
// (manifests-with-data-restoration) through the kube-apiserver aggregation layer and splices the
// returned apply-ready manifests into the result.
//
// This replaces the in-process DomainRestoreTransformer registration in core: core stays domain-free
// and owns no domain restore logic; the domain controller owns the mutation of its own kinds.
type DomainSubtreeRestorer interface {
	// RestoreDomainSubtree returns the apply-ready manifests for the whole subtree rooted at the
	// domain snapshot identified by (gvk, namespace, name). targetNamespace is the namespace the
	// caller wants the objects applied into (empty means the source namespace). The implementation
	// must return objects already sanitized and rewritten for restore (the same contract the core
	// compiler produces for generic nodes), so the spliced output is uniformly apply-ready.
	//
	// The implementation must uphold the same fail-closed contract as the core compiler: it must
	// enforce readiness of the addressed subtree and must never return a data-less object (e.g. a PVC
	// with no data binding). Any such condition must be returned as an error so the whole restore
	// aborts rather than silently producing an empty/partial result.
	RestoreDomainSubtree(ctx context.Context, gvk schema.GroupVersionKind, namespace, name, targetNamespace string) ([]unstructured.Unstructured, error)
}
