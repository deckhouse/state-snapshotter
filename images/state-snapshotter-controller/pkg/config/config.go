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
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/unifiedbootstrap"
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

	// DefaultSnapshotRootOKTTL is spec.ttl on root ObjectKeeper (ret-nssnap-* and unified ret-* snapshot OK)
	// when neither STATE_SNAPSHOTTER_SNAPSHOT_ROOT_OK_TTL nor STATE_SNAPSHOTTER_NS_ROOT_OK_TTL is set.
	//
	// DEBUG ONLY (explicit team choice for TTL/smoke iteration): currently 1m so strict cluster smoke
	// (PR4_SMOKE_REQUIRE_TTL=1) finishes quickly. This MUST NOT ship as the long-term default: before merge
	// to a production-oriented branch, restore a safe built-in default (e.g. 168*time.Hour) or rely solely
	// on env in chart/values — otherwise retained root content may disappear far faster than operators expect.
	DefaultSnapshotRootOKTTL = 1 * time.Minute
	// EnvSnapshotRootOKTTL: optional override (Go duration, must be >0). Empty or invalid → try EnvNamespaceSnapshotRootOKTTLAlt, then default.
	EnvSnapshotRootOKTTL = "STATE_SNAPSHOTTER_SNAPSHOT_ROOT_OK_TTL"
	// EnvNamespaceSnapshotRootOKTTLAlt: second env var name for the same duration; read when EnvSnapshotRootOKTTL is unset or non-positive.
	EnvNamespaceSnapshotRootOKTTLAlt = "STATE_SNAPSHOTTER_NS_ROOT_OK_TTL"
)

type Options struct {
	Loglevel                            logger.Verbosity
	RequeueStorageClassInterval         time.Duration
	RequeueNodeLabelsReconcilerInterval time.Duration
	HealthProbeBindAddress              string
	ControllerNamespace                 string
	// Manifest capture config (TZ section 7)
	MaxChunkSizeBytes  int64
	DefaultTTL         time.Duration
	DefaultTTLStr      string // String representation for annotation (e.g., "168h", "7d")
	ExcludeKinds       []string
	ExcludeAnnotations []string
	// EnableFiltering controls whether filtering and cleaning should be applied
	// If false, all objects are included as-is (no filtering, no cleaning)
	// Default: false (filtering disabled by default)
	EnableFiltering bool

	// UnifiedBootstrapMode + UnifiedBootstrapCustomPairs: static bootstrap before merge with eligible DSC (R5).
	// See EffectiveUnifiedBootstrapPairs().
	UnifiedBootstrapMode        UnifiedBootstrapMode
	UnifiedBootstrapCustomPairs []unifiedbootstrap.UnifiedGVKPair

	// SnapshotRootOKTTL: duration for root snapshot ObjectKeeper FollowObjectWithTTL (NamespaceSnapshot + unified XxxxSnapshot).
	// Resolved at startup: env override if >0, else built-in DefaultSnapshotRootOKTTL.
	SnapshotRootOKTTL time.Duration
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
	opts.ExcludeKinds = []string{
		"Pod", "Event", "Endpoints", "EndpointSlice", "Lease", "Node", "ControllerRevision",
		"VolumeSnapshot", "VolumeSnapshotContent", "*Snapshot", "*SnapshotContent",
	}
	opts.ExcludeAnnotations = []string{
		"kubectl.kubernetes.io/last-applied-configuration",
		"deployment.kubernetes.io/*",
		"autoscaling.alpha.kubernetes.io/*",
		"checksum/*",
		"helm.sh/*",
	}
	// Filtering disabled by default - all objects included as-is
	opts.EnableFiltering = false

	mode, pairs, perr := ParseUnifiedBootstrapPairsEnv(os.Getenv(EnvUnifiedBootstrapPairs))
	if perr != nil {
		log.Printf("Invalid %s (%v); using default bootstrap list", EnvUnifiedBootstrapPairs, perr)
		mode = UnifiedBootstrapDefault
		pairs = nil
	}
	opts.UnifiedBootstrapMode = mode
	opts.UnifiedBootstrapCustomPairs = pairs

	opts.SnapshotRootOKTTL = resolveSnapshotRootOKTTL()
	return &opts
}

func resolveSnapshotRootOKTTL() time.Duration {
	if d, ok := positiveDurationFromEnv(EnvSnapshotRootOKTTL); ok {
		return d
	}
	if d, ok := positiveDurationFromEnv(EnvNamespaceSnapshotRootOKTTLAlt); ok {
		return d
	}
	return DefaultSnapshotRootOKTTL
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
//   - excludeKinds: comma-separated list of kinds to exclude (e.g., "Pod,Event")
//   - excludeAnnotations: comma-separated list of annotation patterns to exclude
//   - enableFiltering: enable object filtering/cleaning ("true"/"false"/"1"/"yes")
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

	// excludeKinds
	if val, ok := configMapData["excludeKinds"]; ok && val != "" {
		kinds := strings.Split(val, ",")
		opts.ExcludeKinds = make([]string, 0, len(kinds))
		for _, kind := range kinds {
			kind = strings.TrimSpace(kind)
			if kind != "" {
				opts.ExcludeKinds = append(opts.ExcludeKinds, kind)
			}
		}
	}

	// excludeAnnotations
	if val, ok := configMapData["excludeAnnotations"]; ok && val != "" {
		anns := strings.Split(val, ",")
		opts.ExcludeAnnotations = make([]string, 0, len(anns))
		for _, ann := range anns {
			ann = strings.TrimSpace(ann)
			if ann != "" {
				opts.ExcludeAnnotations = append(opts.ExcludeAnnotations, ann)
			}
		}
	}

	// enableFiltering
	if val, ok := configMapData["enableFiltering"]; ok {
		// Accept: "true", "True", "TRUE", "1", "yes", "Yes", "YES"
		valLower := strings.ToLower(strings.TrimSpace(val))
		opts.EnableFiltering = valLower == "true" || valLower == "1" || valLower == "yes"
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
