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

// Package patch applies optimistic-locked status patches with conflict retry, generically over any
// client.Object via caller-supplied accessors.
package patch

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Condition applies a single status condition to obj under optimistic lock, retrying on conflict. On each
// attempt it re-reads the live object, merges the condition (preserving co-owned conditions via getConds/
// setConds), stamps observedGeneration from the refreshed object, and skips the patch when already equal.
func Condition(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	getConds func() []metav1.Condition,
	setConds func([]metav1.Condition),
	merge func(conds []metav1.Condition, observedGeneration int64) (out []metav1.Condition, changed bool),
) error {
	key := client.ObjectKeyFromObject(obj)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
		base := obj.DeepCopyObject().(client.Object)
		out, changed := merge(getConds(), obj.GetGeneration())
		if !changed {
			return nil
		}
		setConds(out)
		return c.Status().Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// Status applies a domain-status mutation to obj under a plain merge patch, retrying on conflict. mutate
// reports whether it changed anything; when it does not, no patch is sent.
func Status(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	mutate func() (changed bool),
) error {
	key := client.ObjectKeyFromObject(obj)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
		base := obj.DeepCopyObject().(client.Object)
		if !mutate() {
			return nil
		}
		return c.Status().Patch(ctx, obj, client.MergeFrom(base))
	})
}
