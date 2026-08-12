/*
Copyright 2024 Flant JSC

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

package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/consts"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/logger"
)

const (
	LogLevelEnvName                      = "LOG_LEVEL"
	ControllerNamespaceEnv               = "CONTROLLER_NAMESPACE"
	HardcodedControllerNS                = consts.ModuleNamespace
	ControllerName                       = "controller"
	DefaultHealthProbeBindAddressEnvName = "HEALTH_PROBE_BIND_ADDRESS"
	DefaultHealthProbeBindAddress        = ":8081"
	DefaultRequeueStorageClassInterval   = 10
	DefaultRequeueNodeSelectorInterval   = 10
	// Manifest capture defaults (TZ section 7)
	DefaultMaxChunkSizeBytes = 800000           // 800KB (TZ: maxChunkSizeBytes)
	DefaultTTL               = 10 * time.Minute // 10 minutes (TZ: defaultTTL)
	DefaultTTLStr            = "10m"            // String representation for annotation
	ConfigMapName            = consts.ConfigMapName

	// DefaultSnapshotTTLAfterDelete is spec.ttl on the root ObjectKeeper (unified naming scheme: the root
	// ObjectKeeper is nss-ok-*, retaining the nss-* snapshot content tree)
	// when STATE_SNAPSHOTTER_SNAPSHOT_TTL_AFTER_DELETE is not set.
	//
	// Production default: 30 days (720h). This is the "recycle bin" retention window (wave4B) — how long the
	// durable cluster-scoped SnapshotContent tree survives after its namespaced Snapshot is deleted, during
	// which the captured data remains recoverable. Override per install with the snapshotTtlAfterDelete
	// module parameter (env STATE_SNAPSHOTTER_SNAPSHOT_TTL_AFTER_DELETE). Keep this comfortably long: retained
	// root content disappears once the window elapses, so a short value silently shrinks the recovery window.
	DefaultSnapshotTTLAfterDelete = 30 * 24 * time.Hour // 720h
	// EnvSnapshotTTLAfterDelete: optional override (Go duration, must be >0). Empty or invalid → default.
	EnvSnapshotTTLAfterDelete = "STATE_SNAPSHOTTER_SNAPSHOT_TTL_AFTER_DELETE"
)

type Options struct {
	Loglevel                            logger.Verbosity
	RequeueStorageClassInterval         time.Duration
	RequeueNodeLabelsReconcilerInterval time.Duration
	HealthProbeBindAddress              string
	ControllerNamespace                 string
	// Manifest capture config (TZ section 7)
	MaxChunkSizeBytes int64
	DefaultTTL        time.Duration
	DefaultTTLStr     string // String representation for annotation (e.g., "168h", "7d")

	// SnapshotTTLAfterDelete: how long the durable root SnapshotContent tree is retained after its
	// namespaced Snapshot is deleted (spec.ttl on the root ObjectKeeper, FollowObjectWithTTL mode).
	// Resolved at startup: env override if >0, else built-in DefaultSnapshotTTLAfterDelete.
	SnapshotTTLAfterDelete time.Duration
}

func NewConfig() *Options {
	var opts Options

	loglevel := os.Getenv(LogLevelEnvName)
	if loglevel == "" {
		opts.Loglevel = logger.DebugLevel
	} else {
		opts.Loglevel = logger.Verbosity(loglevel)
	}

	opts.HealthProbeBindAddress = os.Getenv(DefaultHealthProbeBindAddressEnvName)
	if opts.HealthProbeBindAddress == "" {
		opts.HealthProbeBindAddress = DefaultHealthProbeBindAddress
	}

	opts.ControllerNamespace = os.Getenv(ControllerNamespaceEnv)
	if opts.ControllerNamespace == "" {
		namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			log.Printf("Failed to get namespace from filesystem: %v", err)
			log.Printf("Using hardcoded namespace: %s", HardcodedControllerNS)
			opts.ControllerNamespace = HardcodedControllerNS
		} else {
			log.Printf("Got namespace from filesystem: %s", string(namespace))
			opts.ControllerNamespace = string(namespace)
		}
	}

	opts.RequeueStorageClassInterval = DefaultRequeueStorageClassInterval
	opts.RequeueNodeLabelsReconcilerInterval = DefaultRequeueNodeSelectorInterval

	// Manifest capture defaults (TZ section 7)
	opts.MaxChunkSizeBytes = DefaultMaxChunkSizeBytes
	opts.DefaultTTL = DefaultTTL
	opts.DefaultTTLStr = formatDurationForAnnotation(DefaultTTL)

	opts.SnapshotTTLAfterDelete = resolveSnapshotTTLAfterDelete()
	return &opts
}

func resolveSnapshotTTLAfterDelete() time.Duration {
	if d, ok := positiveDurationFromEnv(EnvSnapshotTTLAfterDelete); ok {
		return d
	}
	return DefaultSnapshotTTLAfterDelete
}

func positiveDurationFromEnv(key string) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("Invalid %s (%q): %v", key, v, err)
		return 0, false
	}
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// LoadFromConfigMap loads controller configuration from ConfigMap data and updates Options
// This allows runtime configuration updates without restart
// ConfigMap fields:
//   - maxChunkSizeBytes: maximum chunk size in bytes (e.g., "800000")
//   - defaultTTL: default TTL duration (e.g., "10m", "1h", "168h")
func (opts *Options) LoadFromConfigMap(configMapData map[string]string) {
	// maxChunkSizeBytes
	if val, ok := configMapData["maxChunkSizeBytes"]; ok {
		if size, err := strconv.ParseInt(val, 10, 64); err == nil && size > 0 {
			opts.MaxChunkSizeBytes = size
		}
	}

	// defaultTTL
	if val, ok := configMapData["defaultTTL"]; ok {
		if duration, err := time.ParseDuration(val); err == nil && duration > 0 {
			opts.DefaultTTL = duration
			opts.DefaultTTLStr = formatDurationForAnnotation(duration)
		}
	}
}

// formatDurationForAnnotation formats duration as a readable string for annotation
// Examples: 10m, 1h, 168h, 7d
func formatDurationForAnnotation(d time.Duration) string {
	// Round to nearest minute for readability
	minutes := int(d.Round(time.Minute).Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	remainingMinutes := minutes % 60
	if remainingMinutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, remainingMinutes)
}
