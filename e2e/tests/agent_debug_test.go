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

package tests

// #region agent log
// Temporary debug-session bridge (session f965e6): harvests controller-pod stdout lines tagged
// "DBGCAP" and appends them as NDJSON to the local debug log file so they can be analyzed alongside
// the e2e timeline. The controller runs in the remote nested cluster and cannot write to the Mac
// filesystem directly, so the e2e process (running on the Mac) acts as the bridge. Remove once the
// root-MCR stall is confirmed fixed.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const agentDebugLogPath = "/Users/azimin/Documents/Flant/deckhouse/state-snapshotter/.cursor/debug-f965e6.log"
const agentDebugSessionID = "f965e6"

func agentDebugDumpControllerLogsToFile(ctx context.Context, moduleNS string) {
	pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	f, ferr := os.OpenFile(agentDebugLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if ferr != nil {
		return
	}
	defer f.Close()

	tail := int64(8000)
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Spec.Containers {
			data, lerr := suiteClientset.CoreV1().Pods(moduleNS).
				GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &tail}).
				DoRaw(ctx)
			if lerr != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, "DBGCAP") {
					continue
				}
				entry := map[string]any{
					"sessionId": agentDebugSessionID,
					"location":  pod.Name + "/" + container.Name,
					"message":   strings.TrimSpace(line),
					"timestamp": time.Now().UnixMilli(),
				}
				b, mErr := json.Marshal(entry)
				if mErr != nil {
					continue
				}
				_, _ = f.Write(append(b, '\n'))
			}
		}
	}
}

// #endregion
