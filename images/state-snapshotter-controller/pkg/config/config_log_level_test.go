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

package config

import (
	"strings"
	"testing"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/sds-common-lib/monitoring"
)

// The chart emits the fleet's numeric scale 0..4, the one lib-helm's
// helm_lib_module_controller_log_level produces. This module used to hand that
// number to logrus, whose own scale is 0..6, so "2" -- what `logLevel: INFO`
// renders to -- selected logrus's Error and dropped every Info, Debug and Trace
// record the module wrote. Nothing failed and nothing said so; the logs were
// simply empty. These cases are what keeps that from coming back.
func TestNewConfigReadsTheFleetLogLevelScale(t *testing.T) {
	cases := []struct {
		env  string
		want logger.Verbosity
	}{
		{env: "0", want: logger.ErrorLevel},
		{env: "1", want: logger.WarningLevel},
		{env: "2", want: logger.InfoLevel},
		{env: "3", want: logger.DebugLevel},
		{env: "4", want: logger.TraceLevel},
	}

	for _, tc := range cases {
		t.Run("LOG_LEVEL="+tc.env, func(t *testing.T) {
			t.Setenv(logger.EnvLogLevel, tc.env)

			got := NewConfig().Loglevel
			if got != tc.want {
				t.Fatalf("got verbosity %q, want %q", got, tc.want)
			}
			if _, err := got.Level(); err != nil {
				t.Fatalf("verbosity %q does not map to a level: %v", got, err)
			}
		})
	}
}

func TestNewConfigLogLevelDefaultsToInfo(t *testing.T) {
	t.Setenv(logger.EnvLogLevel, "")

	if got := NewConfig().Loglevel; got != logger.InfoLevel {
		t.Fatalf("with no LOG_LEVEL: got verbosity %q, want %q", got, logger.InfoLevel)
	}
}

// The chart points METRICS_BIND_ADDRESS at a loopback address, because the
// kube-rbac-proxy of the same pod is what publishes the endpoint. The fallback has
// to be loopback as well: the fleet-wide ":8080" is a wildcard, which would both
// answer unauthenticated on the pod IP and collide with the proxy, since the two
// carry the same port number.
func TestNewConfigMetricsBindAddress(t *testing.T) {
	t.Run("taken from the environment", func(t *testing.T) {
		t.Setenv(monitoring.EnvMetricsBindAddress, "127.0.0.1:9101")

		if got := NewConfig().MetricsBindAddress; got != "127.0.0.1:9101" {
			t.Fatalf("got %q, want the value the chart set", got)
		}
	})

	t.Run("fallback stays on loopback", func(t *testing.T) {
		t.Setenv(monitoring.EnvMetricsBindAddress, "")

		got := NewConfig().MetricsBindAddress
		if got != defaultMetricsBindAddress {
			t.Fatalf("got %q, want %q", got, defaultMetricsBindAddress)
		}
		if !strings.HasPrefix(got, "127.0.0.1:") {
			t.Fatalf("fallback %q is not on loopback", got)
		}
	})
}
