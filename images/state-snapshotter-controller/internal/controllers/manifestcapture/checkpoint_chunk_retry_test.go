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

package manifestcapture

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// failChunkCreate returns an interceptor that fails Create on ManifestCheckpointContentChunk objects
// for the first failTimes invocations (with errToReturn), then delegates to the real client. All other
// object kinds always pass through. A failTimes of -1 fails every chunk Create.
func failChunkCreate(errToReturn error, failTimes int) interceptor.Funcs {
	calls := 0
	return interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			if _, ok := obj.(*storagev1alpha1.ManifestCheckpointContentChunk); ok {
				if failTimes < 0 || calls < failTimes {
					calls++
					return errToReturn
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

var _ = Describe("ManifestCaptureRequest chunk creation resilience", func() {
	var (
		ctx        context.Context
		scheme     *runtime.Scheme
		cfg        *config.Options
		testLogger logger.LoggerInterface
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			DefaultTTL:        10 * time.Minute,
			DefaultTTLStr:     "10m",
			MaxChunkSizeBytes: 800000,
		}

		var err error
		testLogger, err = logger.NewLogger("info")
		Expect(err).ToNot(HaveOccurred())
	})

	newController := func(c ctrlclient.Client) *ManifestCheckpointController {
		ctrl, err := NewManifestCheckpointController(c, c, scheme, testLogger, cfg)
		Expect(err).ToNot(HaveOccurred())
		return ctrl
	}

	newMCRWithConfigMap := func(c ctrlclient.Client, name string) *storagev1alpha1.ManifestCaptureRequest {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "target-cm", Namespace: "default"},
			Data:       map[string]string{"k": "v"},
		}
		Expect(c.Create(ctx, cm)).To(Succeed())

		mcr := &storagev1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name + "-uid"),
			},
			Spec: storagev1alpha1.ManifestCaptureRequestSpec{
				Targets: []storagev1alpha1.ManifestTarget{
					{APIVersion: "v1", Kind: "ConfigMap", Name: "target-cm"},
				},
			},
		}
		Expect(c.Create(ctx, mcr)).To(Succeed())
		return mcr
	}

	reconcile := func(ctrl *ManifestCheckpointController, mcr *storagev1alpha1.ManifestCaptureRequest) (controllerruntime.Result, error) {
		return ctrl.Reconcile(ctx, controllerruntime.Request{
			NamespacedName: types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace},
		})
	}

	It("requeues without finalizing when chunk creation hits a transient error", func() {
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}, &storagev1alpha1.ManifestCheckpoint{}).
			WithInterceptorFuncs(failChunkCreate(apierrors.NewServiceUnavailable("apiserver blip"), -1)).
			Build()
		ctrl := newController(c)
		mcr := newMCRWithConfigMap(c, "mcr-transient")

		_, err := reconcile(ctrl, mcr)
		Expect(err).To(HaveOccurred(), "transient chunk error must surface as a reconcile error (requeue)")

		// MCR must remain non-terminal (Processing), not Failed.
		updated := &storagev1alpha1.ManifestCaptureRequest{}
		Expect(c.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())
		cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing))
		Expect(updated.Status.CompletionTimestamp).To(BeNil())

		// The checkpoint exists but must NOT be marked Failed.
		checkpointName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
		mcp := &storagev1alpha1.ManifestCheckpoint{}
		Expect(c.Get(ctx, ctrlclient.ObjectKey{Name: checkpointName}, mcp)).To(Succeed())
		mcpReady := meta.FindStatusCondition(mcp.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
		Expect(mcpReady).ToNot(BeNil())
		Expect(mcpReady.Reason).ToNot(Equal(storagev1alpha1.ManifestCheckpointConditionReasonFailed))
	})

	It("resumes chunk creation on a later reconcile after a transient failure", func() {
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}, &storagev1alpha1.ManifestCheckpoint{}).
			WithInterceptorFuncs(failChunkCreate(apierrors.NewServiceUnavailable("apiserver blip"), 1)).
			Build()
		ctrl := newController(c)
		mcr := newMCRWithConfigMap(c, "mcr-resume")

		// First reconcile: chunk Create fails once -> transient requeue.
		_, err := reconcile(ctrl, mcr)
		Expect(err).To(HaveOccurred())

		// Second reconcile: chunk Create now succeeds -> checkpoint completes.
		_, err = reconcile(ctrl, mcr)
		Expect(err).ToNot(HaveOccurred())

		checkpointName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
		mcp := &storagev1alpha1.ManifestCheckpoint{}
		Expect(c.Get(ctx, ctrlclient.ObjectKey{Name: checkpointName}, mcp)).To(Succeed())
		mcpReady := meta.FindStatusCondition(mcp.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
		Expect(mcpReady).ToNot(BeNil())
		Expect(mcpReady.Status).To(Equal(metav1.ConditionTrue))
		Expect(mcpReady.Reason).To(Equal(storagev1alpha1.ManifestCheckpointConditionReasonCompleted))
		Expect(mcp.Status.Chunks).ToNot(BeEmpty())

		// The chunk resource must now exist.
		chunkList := &storagev1alpha1.ManifestCheckpointContentChunkList{}
		Expect(c.List(ctx, chunkList)).To(Succeed())
		Expect(chunkList.Items).ToNot(BeEmpty())
	})

	It("finalizes the MCR as Failed on a terminal chunk error", func() {
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}, &storagev1alpha1.ManifestCheckpoint{}).
			WithInterceptorFuncs(failChunkCreate(apierrors.NewBadRequest("permanently malformed chunk"), -1)).
			Build()
		ctrl := newController(c)
		mcr := newMCRWithConfigMap(c, "mcr-terminal")

		_, err := reconcile(ctrl, mcr)
		Expect(err).ToNot(HaveOccurred(), "terminal failures are recorded on status, not returned as reconcile errors")

		updated := &storagev1alpha1.ManifestCaptureRequest{}
		Expect(c.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())
		cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
		Expect(updated.Status.CompletionTimestamp).ToNot(BeNil())
	})

	It("resumes chunk creation when a checkpoint already exists but is still Processing", func() {
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}, &storagev1alpha1.ManifestCheckpoint{}).
			Build()
		ctrl := newController(c)
		mcr := newMCRWithConfigMap(c, "mcr-existing-mcp")

		// Pre-create the (deterministically named) checkpoint in a Processing state with no chunks,
		// simulating a prior reconcile that created the MCP but requeued mid chunk-creation.
		checkpointName := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcr.UID)
		mcp := &storagev1alpha1.ManifestCheckpoint{
			ObjectMeta: metav1.ObjectMeta{Name: checkpointName},
			Spec: storagev1alpha1.ManifestCheckpointSpec{
				SourceNamespace: mcr.Namespace,
			},
		}
		Expect(c.Create(ctx, mcp)).To(Succeed())
		mcp.Status.Conditions = []metav1.Condition{{
			Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.ManifestCheckpointConditionReasonProcessing,
			Message:            "Checkpoint created, creating chunks...",
			LastTransitionTime: metav1.Now(),
		}}
		Expect(c.Status().Update(ctx, mcp)).To(Succeed())

		_, err := reconcile(ctrl, mcr)
		Expect(err).ToNot(HaveOccurred())

		// The existing checkpoint must be driven to completion (not short-circuited to handoff).
		updatedMCP := &storagev1alpha1.ManifestCheckpoint{}
		Expect(c.Get(ctx, ctrlclient.ObjectKey{Name: checkpointName}, updatedMCP)).To(Succeed())
		mcpReady := meta.FindStatusCondition(updatedMCP.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
		Expect(mcpReady).ToNot(BeNil())
		Expect(mcpReady.Status).To(Equal(metav1.ConditionTrue))
		Expect(mcpReady.Reason).To(Equal(storagev1alpha1.ManifestCheckpointConditionReasonCompleted))
		Expect(updatedMCP.Status.Chunks).ToNot(BeEmpty())
	})
})

func TestChunkCreateErrorIsTerminal(t *testing.T) {
	gk := schema.GroupKind{Group: "state-snapshotter.deckhouse.io", Kind: "ManifestCheckpointContentChunk"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"invalid", apierrors.NewInvalid(gk, "chunk", nil), true},
		{"bad-request", apierrors.NewBadRequest("bad"), true},
		{"entity-too-large", apierrors.NewRequestEntityTooLargeError("too big"), true},
		{"server-timeout", apierrors.NewServerTimeout(schema.GroupResource{Resource: "x"}, "create", 1), false},
		{"service-unavailable", apierrors.NewServiceUnavailable("down"), false},
		{"too-many-requests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, "chunk", stderrors.New("rbac")), false},
		{"generic", stderrors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkCreateErrorIsTerminal(tc.err); got != tc.want {
				t.Fatalf("chunkCreateErrorIsTerminal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsTerminalCaptureError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", fmt.Errorf("transient"), false},
		{"terminal", terminalCapturef("bad chunk"), true},
		{"terminal-wrapped", fmt.Errorf("context: %w", terminalCapturef("bad chunk")), true},
		{"api-error-not-wrapped", apierrors.NewBadRequest("bad"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalCaptureError(tc.err); got != tc.want {
				t.Fatalf("isTerminalCaptureError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
