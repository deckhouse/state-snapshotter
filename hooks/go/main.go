/*
Copyright 2025 Flant JSC

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

package main

import (
	"github.com/deckhouse/module-sdk/pkg/app"

	_ "hook/020-apiserver-certs"
	// Front-proxy CA is read directly by controller from extension-apiserver-authentication ConfigMap
	// No hook needed - controller handles mTLS client certificate verification internally
)

func main() {
	app.Run()
}
