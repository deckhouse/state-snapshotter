#!/usr/bin/env sh

# Copyright 2026 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Un-freeze snapshot-controller when it is registered on the gated v0.2.0 handoff build.
#
# snapshot-controller v0.2.0 declares requirements.modules.storage-foundation >= 1.0.0, and Deckhouse
# ignores a ModulePullOverride while a module is DISABLED. So a cluster left with snapshot-controller
# *registered* as v0.2.0 while disabled (a completed run, a failed phase-B run, or a manual pr101
# apply) stays gated on storage-foundation, and the next transition run's phase-B enable is
# webhook-denied ("depends on disabled module(s): storage-foundation").
#
# Re-registering it on the non-gated legacy tag:
#   - if it is still ENABLED, retagging the MPO is enough;
#   - if it is DISABLED, the MPO is ignored, so the dependency chain must actually be (re)deployed to
#     satisfy the gate. Deckhouse checks the EFFECTIVE state of a dependency (EnabledByModuleManager),
#     not just ModuleConfig.enabled — and a module with no MPO/ModuleRelease has no version to deploy,
#     so enabling it alone is a no-op. This stages: give each of state-snapshotter -> storage-foundation
#     a ModulePullOverride, enable it, WAIT until it is effectively enabled, then move on; finally
#     enable snapshot-controller so its MPO re-pulls the legacy image, wait until it re-registers
#     non-gated, and disable exactly the modules it transiently enabled.
#
# No-op when snapshot-controller is not gated. Idempotent. Honors:
#   KUBECTL, TRANSITION_SNAPC_LEGACY_TAG (default main), TRANSITION_CLEAN_TIMEOUT (default 300),
#   and for the disabled-frozen case the standard dependency tags
#   STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE, STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE.

KUBECTL="${KUBECTL:-kubectl}"
LEGACY_TAG="${TRANSITION_SNAPC_LEGACY_TAG:-main}"
TIMEOUT="${TRANSITION_CLEAN_TIMEOUT:-300}"
SS_TAG="${STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE:-}"
SF_TAG="${STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE:-}"
SNAPC="snapshot-controller"

# gated: snapshot-controller's resolved requirements.modules reference storage-foundation.
gated() {
	$KUBECTL get module "$SNAPC" -o jsonpath='{.properties.requirements.modules}' 2>/dev/null | grep -q storage-foundation
}

# mc_enabled <module>: true when its ModuleConfig spec.enabled is true.
mc_enabled() {
	[ "$($KUBECTL get moduleconfig "$1" -o jsonpath='{.spec.enabled}' 2>/dev/null)" = "true" ]
}

# effectively_enabled <module>: true when Deckhouse actually turned it on (a release/override
# deployed). This is what a dependent module's admission gate checks, not ModuleConfig.enabled.
effectively_enabled() {
	$KUBECTL get module "$1" -o jsonpath='{range .status.conditions[?(@.type=="EnabledByModuleManager")]}{.status}{end}' 2>/dev/null | grep -q True
}

# set_mpo <module> <tag>: create-or-update its ModulePullOverride so it has a version to deploy.
set_mpo() {
	printf 'apiVersion: deckhouse.io/v1alpha2\nkind: ModulePullOverride\nmetadata:\n  name: %s\nspec:\n  imageTag: %s\n  scanInterval: 30s\n' "$1" "$2" | $KUBECTL apply -f - >/dev/null 2>&1 || true
}

# enable_mc / disable_mc <module>: create-or-update the ModuleConfig enabled flag (best-effort).
enable_mc() {
	if $KUBECTL get moduleconfig "$1" >/dev/null 2>&1; then
		$KUBECTL patch moduleconfig "$1" --type merge -p '{"spec":{"enabled":true}}' >/dev/null 2>&1 || true
	else
		printf 'apiVersion: deckhouse.io/v1alpha1\nkind: ModuleConfig\nmetadata:\n  name: %s\nspec:\n  enabled: true\n  version: 1\n' "$1" | $KUBECTL apply -f - >/dev/null 2>&1 || true
	fi
}
disable_mc() {
	$KUBECTL patch moduleconfig "$1" --type merge -p '{"spec":{"enabled":false}}' >/dev/null 2>&1 || true
}

# wait_effective <module>: block until it is effectively enabled, or TIMEOUT.
wait_effective() {
	deadline=$(( $(date +%s) + TIMEOUT ))
	while ! effectively_enabled "$1"; do
		if [ "$(date +%s)" -ge "$deadline" ]; then
			echo "    WARN: $1 not effectively enabled after ${TIMEOUT}s"
			return 1
		fi
		sleep 5
	done
	echo "    $1 effectively enabled"
}

if ! gated; then
	echo "  snapshot-controller is not gated on storage-foundation — nothing to un-freeze"
	exit 0
fi

echo "  snapshot-controller is registered on the gated v0.2.0 handoff build; re-registering on '$LEGACY_TAG'..."
set_mpo "$SNAPC" "$LEGACY_TAG"

transient=""
if ! mc_enabled "$SNAPC"; then
	# Disabled: the MPO is ignored, so the gate (storage-foundation, and transitively
	# state-snapshotter) must be satisfied by actually redeploying the chain.
	if [ -z "$SS_TAG" ] || [ -z "$SF_TAG" ]; then
		echo "  ERROR: snapshot-controller is DISABLED and frozen on the gated build, so it cannot re-pull"
		echo "         without redeploying its dependency chain (state-snapshotter -> storage-foundation)."
		echo "         Set STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE and STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE"
		echo "         (the same tags the run uses) and re-run, or recreate the cluster. See README."
		exit 1
	fi
	echo "  module disabled -> MPO ignored; redeploying the dependency chain so the gate clears..."
	echo "    state-snapshotter -> $SS_TAG"
	set_mpo state-snapshotter "$SS_TAG"
	if ! mc_enabled state-snapshotter; then enable_mc state-snapshotter; transient="$transient state-snapshotter"; fi
	wait_effective state-snapshotter || true
	echo "    storage-foundation -> $SF_TAG"
	set_mpo storage-foundation "$SF_TAG"
	if ! mc_enabled storage-foundation; then enable_mc storage-foundation; transient="$transient storage-foundation"; fi
	wait_effective storage-foundation || true
	echo "    $SNAPC -> $LEGACY_TAG (re-pull)"
	enable_mc "$SNAPC"
	transient="$transient $SNAPC"
fi

echo "  waiting up to ${TIMEOUT}s for $SNAPC to re-register non-gated..."
deadline=$(( $(date +%s) + TIMEOUT ))
while gated; do
	if [ "$(date +%s)" -ge "$deadline" ]; then
		echo "  WARN: $SNAPC still gated on storage-foundation after ${TIMEOUT}s — a manual reset may be needed"
		break
	fi
	sleep 5
done
gated || echo "  $SNAPC re-registered on '$LEGACY_TAG' (non-gated)"

# Roll back exactly what we transiently enabled (leave everything disabled for the caller/cleanup).
# MPOs are kept: a disabled module's registration is frozen at its last pull, so keeping the MPO
# keeps it on the non-gated/satisfiable tag instead of reverting to the gated source release.
for m in $transient; do
	disable_mc "$m"
	echo "  disabled $m (was transient)"
done
