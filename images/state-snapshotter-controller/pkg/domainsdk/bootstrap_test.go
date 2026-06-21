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

package domainsdk

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseAllowedCNs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty string yields empty (non-nil) slice", in: "", want: []string{}},
		{name: "single", in: "system:kube-apiserver", want: []string{"system:kube-apiserver"}},
		{name: "multiple", in: "a,b,c", want: []string{"a", "b", "c"}},
		{name: "trims whitespace", in: " a , b ,c ", want: []string{"a", "b", "c"}},
		{name: "drops empty entries", in: "a,,b,,,", want: []string{"a", "b"}},
		{name: "only separators yields empty slice", in: ",, ,", want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseAllowedCNs(tc.in)
			if got == nil {
				t.Fatal("ParseAllowedCNs must never return nil")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseAllowedCNs(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	return scheme
}

func frontProxyConfigMap(data map[string]string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: frontProxyCANamespace, Name: frontProxyCAConfigMap},
		Data:       data,
	}
}

func TestLoadFrontProxyCAFromReader_Success(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(frontProxyConfigMap(map[string]string{frontProxyCAKey: ca})).
		Build()

	got, err := LoadFrontProxyCAFromReader(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != ca {
		t.Fatalf("CA mismatch: got %q want %q", got, ca)
	}
}

func TestLoadFrontProxyCAFromReader_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		objects []client.Object
	}{
		{name: "missing ConfigMap", objects: nil},
		{name: "missing requestheader key", objects: []client.Object{frontProxyConfigMap(map[string]string{"other": "x"})}},
		{name: "empty requestheader value", objects: []client.Object{frontProxyConfigMap(map[string]string{frontProxyCAKey: ""})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.objects...).Build()
			got, err := LoadFrontProxyCAFromReader(context.Background(), c)
			if err == nil {
				t.Fatalf("expected fail-closed error, got CA %q", got)
			}
			if got != nil {
				t.Fatalf("expected nil CA on error, got %q", got)
			}
		})
	}
}
