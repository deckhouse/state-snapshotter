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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/restoretransform"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/uuid"
)

// RESTTransformer is the generic, domain-free HTTP client that implements DomainRestoreTransformer by
// delegating to a domain controller's transform endpoint (ADR 2026-06-13 contract, PoC transport). It
// carries the same semantic contract as an in-process transformer over HTTP; it never references any
// domain kind or field name and stays the owner of the restore pipeline by validating every response.
//
// The wire contract (DTOs, endpoint path, version, env) lives in pkg/restoretransform; this client is
// a consumer of that contract, not its owner.
//
// PoC scope: full transformed object (not patch), PVC-only suppress, no auth/TLS/discovery. The
// endpoint is expected to be co-located (loopback) for the demo.
type RESTTransformer struct {
	endpoint   string
	httpClient *http.Client
	// onError is invoked for transport/protocol errors raised on the CoveredPVCNames path, whose
	// interface signature cannot return an error. Defaults to a no-op; wired to a logger in production.
	onError func(error)

	// suppressErr makes the suppress path fail closed: CoveredPVCNames cannot return an error, so a
	// transport/protocol failure there is stashed here and surfaced by the next TransformObject of the
	// same node (called right after CoveredPVCNames in transformNodeObjects), failing the whole
	// restore instead of proceeding with an incomplete suppress set. It is reset at the start of each
	// node's CoveredPVCNames so it never leaks across nodes.
	mu          sync.Mutex
	suppressErr error
}

var _ DomainRestoreTransformer = (*RESTTransformer)(nil)

// NewRESTTransformer returns a RESTTransformer posting to endpoint (a full URL ending in
// restoretransform.EndpointPath).
func NewRESTTransformer(endpoint string) *RESTTransformer {
	return &RESTTransformer{
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		onError:    func(error) {},
	}
}

// WithErrorSink sets a sink for non-propagatable CoveredPVCNames transport errors.
func (t *RESTTransformer) WithErrorSink(sink func(error)) *RESTTransformer {
	if sink != nil {
		t.onError = sink
	}
	return t
}

// CoveredPVCNames asks the domain endpoint which PVCs each node object recreates on restore, and
// returns their names. The interface cannot return an error, so it fails closed: any transport/
// protocol failure is reported to onError and stashed in suppressErr, the partial result is dropped,
// and the next TransformObject of this node surfaces the error and aborts the restore. This prevents
// proceeding with an incomplete suppress set (which could silently emit a dataless PVC).
func (t *RESTTransformer) CoveredPVCNames(node *RestoreNode, objects []unstructured.Unstructured) map[string]struct{} {
	t.setSuppressErr(nil) // start of node: clear any error from a previous node.
	if node == nil {
		t.setSuppressErr(fmt.Errorf("%w: nil restore node in CoveredPVCNames", ErrContractViolation))
		return nil
	}
	covered := map[string]struct{}{}
	for i := range objects {
		obj := &objects[i]
		resp, err := t.call(node, obj, nil)
		if err != nil {
			err = fmt.Errorf("restore transform (suppress) %s/%s %s: %w; refusing to continue with an incomplete suppress set", obj.GetNamespace(), obj.GetName(), obj.GetKind(), err)
			t.onError(err)
			t.setSuppressErr(err)
			return nil
		}
		if !resp.Handled {
			continue
		}
		for _, ref := range resp.SuppressRefs {
			// PoC supports only PVC suppression by name. A suppressRef the generic compiler cannot
			// interpret (non-PVC kind, or PVC without a name) must not be silently ignored: dropping it
			// could keep an object the domain meant to recreate, so fail closed.
			if ref.APIVersion != corePVCVersion || ref.Kind != pvcKind || ref.Name == "" {
				err := fmt.Errorf("%w: unsupported suppressRef %s %s/%s (PoC supports only %s/%s by name)",
					ErrContractViolation, ref.Kind, ref.Namespace, ref.Name, corePVCVersion, pvcKind)
				t.onError(err)
				t.setSuppressErr(err)
				return nil
			}
			covered[ref.Name] = struct{}{}
		}
	}
	return covered
}

// TransformObject sends one sanitized object to the domain endpoint. If the domain handled it, the
// returned object replaces obj in place (after an identity guard). allowed=false is a hard contract
// violation and fails the whole restore.
func (t *RESTTransformer) TransformObject(node *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (bool, error) {
	// Surface a deferred suppress-path failure from this node's CoveredPVCNames (fail closed).
	if err := t.takeSuppressErr(); err != nil {
		return false, err
	}
	resp, err := t.call(node, obj, children)
	if err != nil {
		return false, err
	}
	if !resp.Allowed {
		reason, msg := "Denied", ""
		if resp.Status != nil {
			reason, msg = resp.Status.Reason, resp.Status.Message
		}
		return false, fmt.Errorf("%w: domain rejected %s %s/%s: %s: %s",
			ErrContractViolation, obj.GetKind(), obj.GetNamespace(), obj.GetName(), reason, msg)
	}
	if !resp.Handled {
		return false, nil
	}
	if resp.Object == nil {
		return false, fmt.Errorf("%w: domain handled %s %s/%s but returned no object",
			ErrContractViolation, obj.GetKind(), obj.GetNamespace(), obj.GetName())
	}
	transformed := &unstructured.Unstructured{Object: resp.Object}
	if err := assertSameIdentity(obj, transformed); err != nil {
		return false, err
	}
	obj.Object = transformed.Object
	return true, nil
}

// call builds the envelope, posts it and validates the response uid echo. It never panics on nil
// inputs: a nil node or object is a contract violation, returned as an error.
func (t *RESTTransformer) call(node *RestoreNode, obj *unstructured.Unstructured, children []NodeResult) (*restoretransform.Response, error) {
	if node == nil {
		return nil, fmt.Errorf("%w: nil restore node", ErrContractViolation)
	}
	if obj == nil || obj.Object == nil {
		return nil, fmt.Errorf("%w: nil restore object", ErrContractViolation)
	}
	// Opaque correlation token: the domain must echo it, never parse it. Object identity lives in
	// targetRef, not in the uid, so the contract does not leak an identity encoding.
	uid := string(uuid.NewUUID())
	reqEnvelope := restoretransform.RequestEnvelope{Request: restoretransform.Request{
		UID: uid,
		TargetRef: restoretransform.ObjectRef{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
		},
		Node: restoretransform.NodeRef{SnapshotRef: restoretransform.ObjectRef{
			APIVersion: node.SnapshotRef.APIVersion,
			Kind:       node.SnapshotRef.Kind,
			Namespace:  node.SnapshotRef.Namespace,
			Name:       node.SnapshotRef.Name,
		}},
		RestoreContext: restoretransform.RestoreContext{
			SourceNamespace: node.SnapshotRef.Namespace,
			TargetNamespace: obj.GetNamespace(),
		},
		Object:   obj.Object,
		DataRefs: dataRefsToContract(node.DataBindings),
		Children: childrenToContext(children),
	}}

	body, err := json.Marshal(reqEnvelope)
	if err != nil {
		return nil, fmt.Errorf("marshal transform request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), t.httpClient.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build transform request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post transform request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transform endpoint status %d", httpResp.StatusCode)
	}

	var respEnvelope restoretransform.ResponseEnvelope
	if err := json.NewDecoder(httpResp.Body).Decode(&respEnvelope); err != nil {
		return nil, fmt.Errorf("decode transform response: %w", err)
	}
	resp := respEnvelope.Response
	if resp.UID != uid {
		return nil, fmt.Errorf("%w: transform response uid %q does not match request uid %q",
			ErrContractViolation, resp.UID, uid)
	}
	return &resp, nil
}

func (t *RESTTransformer) setSuppressErr(err error) {
	t.mu.Lock()
	t.suppressErr = err
	t.mu.Unlock()
}

// takeSuppressErr returns and clears the stashed suppress-path error.
func (t *RESTTransformer) takeSuppressErr() error {
	t.mu.Lock()
	err := t.suppressErr
	t.suppressErr = nil
	t.mu.Unlock()
	return err
}

// assertSameIdentity enforces that a domain transform cannot change GVK/name/namespace, so it cannot
// smuggle in a different (or cluster-scoped) object past the generic sanitizer.
func assertSameIdentity(in, out *unstructured.Unstructured) error {
	if in.GetAPIVersion() != out.GetAPIVersion() ||
		in.GetKind() != out.GetKind() ||
		in.GetName() != out.GetName() ||
		in.GetNamespace() != out.GetNamespace() {
		return fmt.Errorf("%w: domain transform changed object identity from %s %s/%s to %s %s/%s",
			ErrContractViolation,
			in.GetAPIVersion()+"/"+in.GetKind(), in.GetNamespace(), in.GetName(),
			out.GetAPIVersion()+"/"+out.GetKind(), out.GetNamespace(), out.GetName())
	}
	return nil
}

func dataRefsToContract(bindings []snapshot.DataBindingRef) []restoretransform.ObjectRef {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]restoretransform.ObjectRef, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, restoretransform.ObjectRef{
			APIVersion: b.Artifact.APIVersion,
			Kind:       b.Artifact.Kind,
			Namespace:  b.Artifact.Namespace,
			Name:       b.Artifact.Name,
		})
	}
	return out
}

func childrenToContext(children []NodeResult) []restoretransform.ChildNode {
	if len(children) == 0 {
		return nil
	}
	out := make([]restoretransform.ChildNode, 0, len(children))
	for i := range children {
		objs := make([]map[string]interface{}, 0, len(children[i].Objects))
		for j := range children[i].Objects {
			objs = append(objs, children[i].Objects[j].Object)
		}
		entry := restoretransform.ChildNode{Objects: objs}
		if children[i].Node != nil {
			entry.SnapshotRef = restoretransform.ObjectRef{
				APIVersion: children[i].Node.SnapshotRef.APIVersion,
				Kind:       children[i].Node.SnapshotRef.Kind,
				Namespace:  children[i].Node.SnapshotRef.Namespace,
				Name:       children[i].Node.SnapshotRef.Name,
			}
		}
		out = append(out, entry)
	}
	return out
}
