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

// Package crdtests checks what kube-apiserver admits and rejects once the
// generated crds/ and crds/internal/ are installed. It is a module of its own so
// that envtest, the controller-runtime client and sds-common-lib stay out of the
// requirements of the api module, which other modules import.
package crdtests

import (
	"context"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/sds-common-lib/crdtest"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// testNamespace holds the namespaced objects; kube-apiserver creates it itself.
const testNamespace = "default"

var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(crdtest.Main(m, crdtest.Options{
		CRDPaths:    []string{"../../crds", "../../crds/internal"},
		AddToScheme: []func(*runtime.Scheme) error{storagev1alpha1.AddToScheme, ssv1alpha1.AddToScheme},
	}, func(env *crdtest.Env) { k8sClient = env.Client }))
}

// objectName turns a subtest name into an object name the apiserver accepts.
func objectName(prefix, name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(prefix + "-" + name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if !strings.HasSuffix(b.String(), "-") {
			b.WriteRune('-')
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// expectAdmission checks that err is nil when no messages are given, and
// otherwise that the apiserver rejected the object as invalid with every one of
// messages in the error.
func expectAdmission(t *testing.T, err error, messages ...string) {
	t.Helper()
	if len(messages) == 0 {
		if err != nil {
			t.Fatalf("want the object admitted, got %v", err)
		}
		return
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("want an Invalid error with %q, got %v", messages, err)
	}
	for _, message := range messages {
		if !strings.Contains(err.Error(), message) {
			t.Fatalf("want %q in the error, got %v", message, err)
		}
	}
}

// create creates obj, fails the test when the apiserver refuses it and deletes
// it when the test ends.
func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %s: %v", obj.GetName(), err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
}

// writer is how a change reaches the apiserver: the object itself or its status
// subresource, where the status rules are checked.
type writer func(context.Context, client.Object) error

func writeObject(ctx context.Context, obj client.Object) error { return k8sClient.Update(ctx, obj) }

func writeStatus(ctx context.Context, obj client.Object) error {
	return k8sClient.Status().Update(ctx, obj)
}

// change writes a copy of obj changed by mutate and checks the verdict like
// expectAdmission. It returns the stored object when the change is admitted
// and obj otherwise, so that a test can go on from the last admitted state.
func change[T client.Object](t *testing.T, obj T, write writer, mutate func(T), messages ...string) T {
	t.Helper()
	changed, ok := obj.DeepCopyObject().(T)
	if !ok {
		t.Fatalf("copy of %T is %T", obj, obj.DeepCopyObject())
	}
	mutate(changed)
	err := write(context.Background(), changed)
	expectAdmission(t, err, messages...)
	if err != nil {
		return obj
	}
	return changed
}
