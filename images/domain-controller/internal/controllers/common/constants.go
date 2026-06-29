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

package common //nolint:revive // package name matches internal/controllers/common directory

// Kind constants used by the demo domain controllers. KindSnapshot is the generic core snapshot kind
// (referenced when resolving parent ownerRefs); the rest are demo-domain kinds. This package is
// self-contained so images/domain-controller depends only on api/ and k8s.
const (
	KindSnapshot                   = "Snapshot"
	KindDemoVirtualDiskSnapshot    = "DemoVirtualDiskSnapshot"
	KindDemoVirtualMachineSnapshot = "DemoVirtualMachineSnapshot"
	KindDemoVirtualDisk            = "DemoVirtualDisk"
	KindDemoVirtualMachine         = "DemoVirtualMachine"
)
