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
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// SnapshotContentVCRName returns the deterministic VolumeCaptureRequest name for a logical SnapshotContent (N5 PR-4).
func SnapshotContentVCRName(contentUID types.UID) string {
	return fmt.Sprintf("snap-vcr-%s", contentUID)
}

// SnapshotOwnedVCRName returns the deterministic data-leg VolumeCaptureRequest name owned by a domain
// snapshot, keyed by the snapshot UID. Used by domain (demo) controllers so the request name is derivable
// from the snapshot alone, without reading SnapshotContent (commit 2 content-ownership decoupling, D3).
func SnapshotOwnedVCRName(snapshotUID types.UID) string {
	return fmt.Sprintf("snap-owned-vcr-%s", snapshotUID)
}
