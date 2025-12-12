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
	DefaultMaxChunkSizeBytes = 800000          // 800KB (TZ: maxChunkSizeBytes)
	DefaultTTL               = 168 * time.Hour // 7 days (TZ: defaultTTL)
	ConfigMapName            = consts.ConfigMapName
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
	ExcludeKinds       []string
	ExcludeAnnotations []string
	// EnableFiltering controls whether filtering and cleaning should be applied
	// If false, all objects are included as-is (no filtering, no cleaning)
	// Default: false (filtering disabled by default)
	EnableFiltering bool
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

	return &opts
}

// LoadFromConfigMap loads configuration from ConfigMap and updates Options
// This allows runtime configuration updates without restart
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
