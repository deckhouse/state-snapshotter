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

package handlers

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/slok/kubewebhook/v2/pkg/model"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/deckhouse/sds-common-lib/logger"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestMCRValidateLogsThroughSetLogger(t *testing.T) {
	const message = "User test-user does not have GET permission for Namespace/default"

	tests := []struct {
		level   logger.Verbosity
		wantLog bool
	}{
		{level: logger.InfoLevel, wantLog: true},
		{level: logger.WarningLevel, wantLog: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			var out bytes.Buffer
			l, err := logger.NewLoggerToWriter(&out, tt.level)
			if err != nil {
				t.Fatal(err)
			}
			SetLogger(l)
			t.Cleanup(func() { SetLogger(logger.NewNop()) })

			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "subjectaccessreviews", func(_ ktesting.Action) (bool, runtime.Object, error) {
				return true, &authorizationv1.SubjectAccessReview{
					Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false},
				}, nil
			})
			SetKubernetesClient(client)

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "mcr", Namespace: "default"},
				Spec: storagev1alpha1.ManifestCaptureRequestSpec{
					Targets: []storagev1alpha1.ManifestTarget{{APIVersion: "v1", Kind: "Namespace", Name: "default"}},
				},
			}
			ar := &model.AdmissionReview{
				Operation: model.OperationCreate,
				UserInfo:  authenticationv1.UserInfo{Username: "test-user"},
			}

			res, err := MCRValidate(context.Background(), ar, mcr)
			if err != nil {
				t.Fatal(err)
			}
			if res.Valid {
				t.Fatal("the request is admitted, want it rejected")
			}

			if got := strings.Contains(out.String(), message); got != tt.wantLog {
				t.Fatalf("message logged = %t, want %t; output: %q", got, tt.wantLog, out.String())
			}
		})
	}
}
