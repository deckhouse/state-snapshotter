//go:build integration
// +build integration

// Package pocdynwatch is a throwaway proof-of-concept that validates the wiring fix for the
// duplicate-reconciliation finding: a snapshot-status wake can be attached to the SINGLE existing
// SnapshotContent controller via controller.Controller.Watch(...) AFTER the manager has started,
// instead of building a second controller-runtime Controller per Snapshot GVK.
//
// It uses only core types (ConfigMap as the "For" object, Secret as the dynamically-watched "wake"
// source), so no CRDs are required. Run with envtest assets:
//
//	KUBEBUILDER_ASSETS=$HOME/.envtest-bin/k8s/1.33.0-darwin-arm64 \
//	  go test -tags integration ./test/pocdynwatch/... -count=1 -v
package pocdynwatch

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// countingReconciler records how many times each object name was reconciled, so the PoC can prove
// that a dynamically-added Watch delivers events into the SAME reconciler (== same controller).
type countingReconciler struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *countingReconciler) Reconcile(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	r.mu.Lock()
	r.counts[req.Name]++
	r.mu.Unlock()
	return reconcile.Result{}, nil
}

func (r *countingReconciler) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, msg)
}

func TestDynamicWatchOnRunningController(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start (is KUBEBUILDER_ASSETS set?): %v", err)
	}
	defer func() { _ = env.Stop() }()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, HealthProbeBindAddress: "0", LeaderElection: false})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	r := &countingReconciler{counts: map[string]int{}}

	// A distinctive RateLimiter + MaxConcurrentReconciles so we can reason that both are properties of
	// the ONE controller we build; a dynamically-added Watch cannot introduce a second queue/worker pool.
	rl := workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, 1*time.Second)
	primary, err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Named("poc-primary").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, RateLimiter: rl}).
		Build(r) // Build(r) == Complete(r) + returns the controller handle (verified in controller-runtime source).
	if err != nil {
		t.Fatalf("build primary controller: %v", err)
	}
	if primary == nil {
		t.Fatal("Build returned a nil controller handle")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
	// Give the manager a beat to flip the controller's Started flag before the dynamic Watch call.
	time.Sleep(500 * time.Millisecond)

	const cmName = "poc-cm"
	ns := "default"

	// Sanity: the primary For(ConfigMap) path reconciles.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns}}
	if err := mgr.GetClient().Create(ctx, cm); err != nil {
		t.Fatalf("create configmap: %v", err)
	}
	waitUntil(t, 10*time.Second, "primary For(ConfigMap) reconcile", func() bool { return r.count(cmName) > 0 })
	baseline := r.count(cmName)
	t.Logf("primary For(ConfigMap) reconciled %d time(s) before dynamic watch", baseline)

	// THE CLAIM UNDER TEST: attach a NEW event source (Secret) to the ALREADY-STARTED controller via
	// Controller.Watch(...). A Secret change maps to the poc-cm ConfigMap request. No second Controller
	// is constructed anywhere.
	mapSecretToCM := func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ns, Name: cmName}}}
	}
	src := source.Kind(mgr.GetCache(), client.Object(&corev1.Secret{}),
		handler.EnqueueRequestsFromMapFunc(mapSecretToCM))
	if err := primary.Watch(src); err != nil {
		t.Fatalf("Controller.Watch on running controller failed: %v", err)
	}
	t.Log("Controller.Watch(...) on a running controller returned nil (source started dynamically)")

	// Trigger the dynamic source: a Secret event must wake the poc-cm reconcile through the SAME controller.
	before := r.count(cmName)
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "poc-secret", Namespace: ns}}
	if err := mgr.GetClient().Create(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	waitUntil(t, 10*time.Second, "dynamic Secret watch wakes poc-cm reconcile", func() bool {
		return r.count(cmName) > before
	})
	t.Logf("after dynamic Secret watch: poc-cm reconcile count grew %d -> %d (event delivered to the single controller)",
		before, r.count(cmName))

	// Clean shutdown: cancelling the manager ctx must terminate cleanly (dynamic sources are tied to the
	// controller ctx, not leaked).
	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("manager.Start returned error on shutdown: %v", err)
		}
		t.Log("manager shut down cleanly with the dynamically-added watch active")
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not shut down within 10s after cancel")
	}

	fmt.Println("PoC OK: dynamic Controller.Watch after start delivers events to the single controller")
}
