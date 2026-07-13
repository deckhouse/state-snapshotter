#!/usr/bin/env sh
#
# Un-freeze snapshot-controller when it is registered on the gated v0.2.0 handoff build.
#
# snapshot-controller v0.2.0 declares requirements.modules.storage-foundation >= 1.0.0, and Deckhouse
# ignores a ModulePullOverride while a module is DISABLED. So a cluster left with snapshot-controller
# *registered* as v0.2.0 while disabled (a completed run, a failed phase-B run, or a manual pr101
# apply) stays gated on storage-foundation, and the next transition run's phase-B enable is
# webhook-denied ("depends on disabled module(s): storage-foundation").
#
# This script re-registers it on the non-gated legacy tag:
#   - if it is still enabled, retagging the MPO is enough;
#   - if it is disabled, the MPO is ignored, so it briefly enables the dependency chain
#     (state-snapshotter -> storage-foundation) to satisfy the gate, enables snapshot-controller so
#     the MPO re-pulls the legacy image, waits until it re-registers non-gated, then disables again
#     exactly the modules it transiently enabled.
#
# It is a no-op when snapshot-controller is not gated. Safe to run standalone or from `make
# transition-clean`. Honors: KUBECTL, TRANSITION_SNAPC_LEGACY_TAG (default main),
# TRANSITION_CLEAN_TIMEOUT (default 300).

KUBECTL="${KUBECTL:-kubectl}"
LEGACY_TAG="${TRANSITION_SNAPC_LEGACY_TAG:-main}"
TIMEOUT="${TRANSITION_CLEAN_TIMEOUT:-300}"
SNAPC="snapshot-controller"

# gated: snapshot-controller's resolved requirements.modules reference storage-foundation.
gated() {
	$KUBECTL get module "$SNAPC" -o jsonpath='{.properties.requirements.modules}' 2>/dev/null | grep -q storage-foundation
}

# mc_enabled <module>: true when its ModuleConfig spec.enabled is true.
mc_enabled() {
	[ "$($KUBECTL get moduleconfig "$1" -o jsonpath='{.spec.enabled}' 2>/dev/null)" = "true" ]
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

if ! gated; then
	echo "  snapshot-controller is not gated on storage-foundation — nothing to un-freeze"
	exit 0
fi

echo "  snapshot-controller is registered on the gated v0.2.0 handoff build; re-registering on '$LEGACY_TAG'..."

# Force the non-gated legacy image via MPO (ignored while disabled — handled by the enable chain below).
printf 'apiVersion: deckhouse.io/v1alpha2\nkind: ModulePullOverride\nmetadata:\n  name: %s\nspec:\n  imageTag: %s\n  scanInterval: 30s\n' "$SNAPC" "$LEGACY_TAG" | $KUBECTL apply -f - >/dev/null 2>&1 || true

transient=""
if ! mc_enabled "$SNAPC"; then
	echo "  module disabled -> MPO ignored; briefly enabling the dependency chain so it can re-pull..."
	# Order matters: storage-foundation depends on state-snapshotter, snapshot-controller on
	# storage-foundation. Each admission passes once its dependency is enabled.
	for dep in state-snapshotter storage-foundation; do
		if ! mc_enabled "$dep"; then
			enable_mc "$dep"
			transient="$transient $dep"
			echo "    enabled $dep (transient)"
		fi
	done
	enable_mc "$SNAPC"
	transient="$transient $SNAPC"
	echo "    enabled $SNAPC (transient)"
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
for m in $transient; do
	disable_mc "$m"
	echo "  disabled $m (was transient)"
done
