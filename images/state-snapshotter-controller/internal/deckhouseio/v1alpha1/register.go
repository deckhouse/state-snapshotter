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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the deckhouse.io API group (owned by deckhouse-controller).
	GroupName = "deckhouse.io"
	// Version is the API version mirrored here.
	Version = "v1alpha1"
)

// SchemeGroupVersion is the group version used to register these objects.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

var (
	// SchemeBuilder collects the functions that add the mirrored types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme registers the mirrored ObjectKeeper types with a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers ONLY ObjectKeeper and ObjectKeeperList — this is the whole
// point of the local mirror. Upstream addKnownTypes registers dozens of unrelated
// deckhouse.io types (ModuleConfig, Module, DeckhouseRelease, …), which is what made
// importing the upstream package so heavy.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&ObjectKeeper{},
		&ObjectKeeperList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
