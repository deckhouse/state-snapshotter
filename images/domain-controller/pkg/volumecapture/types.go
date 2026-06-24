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

package volumecapture

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// VolumeCaptureRequestGVK is the storage-foundation VolumeCaptureRequest GVK. The domain controller
// talks to it via unstructured objects only, so there is no Go dependency on the foundation API module.
var VolumeCaptureRequestGVK = schema.GroupVersionKind{
	Group:   "storage.deckhouse.io",
	Version: "v1alpha1",
	Kind:    "VolumeCaptureRequest",
}

const (
	VolumeCaptureModeSnapshot = "Snapshot"
)

// Target identifies a PVC capture target (matches foundation VolumeCaptureTarget JSON).
type Target struct {
	UID        string
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}
