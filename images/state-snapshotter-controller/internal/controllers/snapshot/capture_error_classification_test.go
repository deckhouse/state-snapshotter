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

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "dial tcp: i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

func TestIsTransientCaptureTargetError(t *testing.T) {
	gr := schema.GroupResource{Group: "kafka.example.com", Resource: "kafkas"}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"server-timeout", apierrors.NewServerTimeout(gr, "list", 1), true},
		{"too-many-requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"service-unavailable", apierrors.NewServiceUnavailable("down"), true},
		{"internal-error", apierrors.NewInternalError(errors.New("boom")), true},
		{"deadline-exceeded", fmt.Errorf("list: %w", context.DeadlineExceeded), true},
		{"eof", fmt.Errorf("read: %w", io.EOF), true},
		{"net-timeout", fmt.Errorf("list: %w", timeoutNetError{}), true},
		{"informer-sync", errors.New("failed to wait for Informer to sync"), true},
		{"connection-refused", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), true},
		// Forbidden is NOT transient here — it is collected as unreadable (fail-closed) separately.
		{"forbidden", apierrors.NewForbidden(gr, "", errors.New("rbac")), false},
		{"structural", errors.New("json: cannot unmarshal"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientCaptureTargetError(tc.err); got != tc.want {
				t.Fatalf("isTransientCaptureTargetError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
