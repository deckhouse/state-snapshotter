#!/bin/bash
#
# Smoke test for Unified Snapshots Controller
# Tests basic Snapshot → SnapshotContent → Ready flow
#
# Usage:
#   ./test-smoke.sh [namespace] [snapshot-kind] [backup-class-name]
#
# Example:
#   ./test-smoke.sh default Snapshot my-local-class
#   ./test-smoke.sh d8-backup Snapshot my-local-class
#   ./test-smoke.sh default Snapshot  # Uses default: my-local-class
#
# Note: backupClassName is required and must reference an existing BackupClass
#       BackupClass binds Snapshot to a BackupRepository
#       Check available BackupClasses: kubectl get backupclass.storage.deckhouse.io

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration (updated for storage.deckhouse.io cluster)
NAMESPACE="${1:-default}"
SNAPSHOT_KIND="${2:-Snapshot}"
SNAPSHOT_NAME="test-smoke-$(date +%s)"
SNAPSHOT_API_GROUP="storage.deckhouse.io"
SNAPSHOT_API_VERSION="v1alpha1"
CONTENT_KIND="${SNAPSHOT_KIND}Content"
CONTROLLER_NAMESPACE="d8-state-snapshotter"
BACKUP_CLASS_NAME="${3:-my-local-class}"  # Optional: backupClassName (default: "my-local-class")

# Short names for kubectl (from api-resources)
SNAPSHOT_SHORT="snap"
CONTENT_SHORT="snapcontent"

# Test counters
TESTS_PASSED=0
TESTS_FAILED=0

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[✓]${NC} $*"
    ((TESTS_PASSED++)) || true
}

log_error() {
    echo -e "${RED}[✗]${NC} $*"
    ((TESTS_FAILED++)) || true
}

log_warn() {
    echo -e "${YELLOW}[!]${NC} $*"
}

wait_for_condition() {
    local resource_type=$1
    local name=$2
    local condition_type=$3
    local expected_status=$4
    local namespace="${5:-}"
    local timeout="${6:-30}"
    local interval="${7:-1}"
    
    local ns_arg=""
    if [[ -n "$namespace" ]]; then
        ns_arg="-n $namespace"
    fi
    
    log_info "Waiting for $resource_type/$name condition $condition_type=$expected_status (timeout: ${timeout}s)..."
    
    local elapsed=0
    while [[ $elapsed -lt $timeout ]]; do
        local status=$(kubectl get $resource_type $name $ns_arg -o jsonpath="{.status.conditions[?(@.type=='$condition_type')].status}" 2>/dev/null || echo "")
        
        if [[ "$status" == "$expected_status" ]]; then
            log_success "Condition $condition_type=$expected_status reached"
            return 0
        fi
        
        sleep $interval
        elapsed=$((elapsed + interval))
    done
    
    log_error "Timeout waiting for $condition_type=$expected_status (current: $status)"
    return 1
}

wait_for_field() {
    local resource_type=$1
    local name=$2
    local jsonpath=$3
    local expected_value=$4
    local namespace="${5:-}"
    local timeout="${6:-30}"
    local interval="${7:-1}"
    
    local ns_arg=""
    if [[ -n "$namespace" ]]; then
        ns_arg="-n $namespace"
    fi
    
    log_info "Waiting for $resource_type/$name field $jsonpath=$expected_value (timeout: ${timeout}s)..."
    
    local elapsed=0
    while [[ $elapsed -lt $timeout ]]; do
        local value=$(kubectl get $resource_type $name $ns_arg -o jsonpath="$jsonpath" 2>/dev/null || echo "")
        
        if [[ "$value" == "$expected_value" ]]; then
            log_success "Field $jsonpath=$expected_value reached"
            return 0
        fi
        
        sleep $interval
        elapsed=$((elapsed + interval))
    done
    
    log_error "Timeout waiting for $jsonpath=$expected_value (current: $value)"
    return 1
}

check_resource_exists() {
    local resource_type=$1
    local name=$2
    local namespace="${3:-}"
    
    local ns_arg=""
    if [[ -n "$namespace" ]]; then
        ns_arg="-n $namespace"
    fi
    
    if kubectl get $resource_type $name $ns_arg &>/dev/null; then
        log_success "Resource $resource_type/$name exists"
        return 0
    else
        log_error "Resource $resource_type/$name does not exist"
        return 1
    fi
}

check_resource_not_exists() {
    local resource_type=$1
    local name=$2
    local namespace="${3:-}"
    
    local ns_arg=""
    if [[ -n "$namespace" ]]; then
        ns_arg="-n $namespace"
    fi
    
    if kubectl get $resource_type $name $ns_arg &>/dev/null; then
        log_error "Resource $resource_type/$name still exists (should be deleted)"
        return 1
    else
        log_success "Resource $resource_type/$name does not exist (correctly deleted)"
        return 0
    fi
}

# Cleanup function
cleanup() {
    log_info "Cleaning up test resources..."
    
    # Use dedicated cleanup script if available, otherwise fallback to inline cleanup
    local cleanup_script="$(dirname "$0")/test-cleanup.sh"
    if [[ -f "$cleanup_script" ]] && [[ -x "$cleanup_script" ]]; then
        log_info "Using dedicated cleanup script: $cleanup_script"
        "$cleanup_script" --snapshot-name "$SNAPSHOT_NAME" --namespace "$NAMESPACE" --snapshot-kind "$SNAPSHOT_KIND" --force || true
    else
        log_warn "Cleanup script not found, using inline cleanup"
        log_warn "For better cleanup, use: ./test-cleanup.sh --snapshot-name $SNAPSHOT_NAME --namespace $NAMESPACE --force"
        
        # Fallback inline cleanup
        local snapshot_resource="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
        local content_resource="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"
        
        if kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE &>/dev/null; then
            kubectl delete "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE --wait=false || true
        fi
        
        # Wait a bit for reconciliation
        sleep 5
        
        # Force delete SnapshotContent if still exists (remove finalizers)
        local content_name=$(kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE -o jsonpath='{.status.boundSnapshotContentName}' 2>/dev/null || echo "")
        if [[ -z "$content_name" ]] && [[ -n "${CONTENT_NAME:-}" ]]; then
            content_name="$CONTENT_NAME"
        fi
        
        if [[ -n "$content_name" ]]; then
            if kubectl get "$content_resource" "$content_name" &>/dev/null; then
                log_warn "Removing finalizers from $CONTENT_KIND/$content_name..."
                kubectl patch "$content_resource" "$content_name" -p '{"metadata":{"finalizers":[]}}' --type=merge || true
                kubectl delete "$content_resource" "$content_name" --wait=false || true
            fi
        fi
        
        log_info "Cleanup completed"
    fi
}

# Trap to ensure cleanup on exit
trap cleanup EXIT

# Main test flow
main() {
    echo "═══════════════════════════════════════════════════════════════"
    echo "🧪 Unified Snapshots Controller - Smoke Test"
    echo "═══════════════════════════════════════════════════════════════"
    echo ""
    echo "Configuration:"
    echo "  Namespace: $NAMESPACE"
    echo "  Snapshot Kind: $SNAPSHOT_KIND"
    echo "  Snapshot Name: $SNAPSHOT_NAME"
    echo "  API Group: $SNAPSHOT_API_GROUP"
    echo "  BackupClass Name: $BACKUP_CLASS_NAME"
    echo ""
    
    # Pre-flight checks
    log_info "Pre-flight checks..."
    
    # Check CRDs exist (using lowercase plural form)
    # Note: Kubernetes CRDs always use plural form for resource names
    local snapshot_crd="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    local content_crd="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"  # Note: plural form (snapshotcontents, not snapshotcontent)
    
    if ! kubectl get crd "$snapshot_crd" &>/dev/null; then
        log_error "CRD $snapshot_crd not found"
        exit 1
    fi
    
    if ! kubectl get crd "$content_crd" &>/dev/null; then
        log_error "CRD $content_crd not found"
        exit 1
    fi
    
    log_info "Using CRDs: $snapshot_crd, $content_crd"
    
    # Check if conditions field exists in Snapshot CRD status schema
    # If not, try to patch it (best-effort, for local testing only)
    # NOTE: In production Deckhouse clusters, CRDs are managed by the platform
    # This is only for smoke testing in development/test clusters
    local status_properties=$(kubectl get crd "$snapshot_crd" -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties}' 2>/dev/null || echo "")
    local has_conditions=""
    if [[ -n "$status_properties" ]]; then
        has_conditions=$(kubectl get crd "$snapshot_crd" -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.conditions}' 2>/dev/null || echo "")
    fi
    
    SKIP_DOMAIN_SIMULATION=false
    if [[ -z "$has_conditions" ]]; then
        log_warn "CRD $snapshot_crd does not have 'conditions' field in status schema"
        log_warn "This is required for unified snapshots. Attempting to patch CRD (best-effort)..."
        
        # Ensure status.properties exists first
        if [[ -z "$status_properties" ]]; then
            log_info "Adding status.properties to CRD schema..."
            kubectl patch crd "$snapshot_crd" --type=json -p '[{
                "op": "add",
                "path": "/spec/versions/0/schema/openAPIV3Schema/properties/status/properties",
                "value": {}
            }]' 2>/dev/null || true
            sleep 1
        fi
        
        # Try to patch CRD to add conditions field (best-effort, don't fail if it doesn't work)
        log_info "Adding conditions field to CRD schema..."
        if kubectl patch crd "$snapshot_crd" --type=json -p '[{
            "op": "add",
            "path": "/spec/versions/0/schema/openAPIV3Schema/properties/status/properties/conditions",
            "value": {
                "description": "Conditions represent the latest available observations of the snapshot state",
                "items": {
                    "description": "Condition contains details for one aspect of the current state of this API Resource.",
                    "properties": {
                        "lastTransitionTime": {"description": "lastTransitionTime is the last time the condition transitioned from one status to another.", "format": "date-time", "type": "string"},
                        "message": {"description": "message is a human readable message indicating details about the transition.", "maxLength": 32768, "type": "string"},
                        "observedGeneration": {"description": "observedGeneration represents the .metadata.generation that the condition was set based upon.", "format": "int64", "minimum": 0, "type": "integer"},
                        "reason": {"description": "reason contains a programmatic identifier indicating the reason for the condition'\''s last transition.", "maxLength": 1024, "minLength": 1, "pattern": "^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$", "type": "string"},
                        "status": {"description": "status of the condition, one of True, False, Unknown.", "enum": ["True", "False", "Unknown"], "type": "string"},
                        "type": {"description": "type of condition in CamelCase or in foo.example.com/CamelCase.", "maxLength": 316, "pattern": "^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$", "type": "string"}
                    },
                    "required": ["lastTransitionTime", "message", "reason", "status", "type"],
                    "type": "object"
                },
                "type": "array"
            }
        }]' 2>&1; then
            log_success "CRD patched successfully. Waiting for CRD to be established..."
            kubectl wait --for=condition=Established crd "$snapshot_crd" --timeout=30s 2>/dev/null || true
            sleep 2
            # Verify conditions field was added
            local verify_conditions=$(kubectl get crd "$snapshot_crd" -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.conditions.type}' 2>/dev/null || echo "")
            if [[ "$verify_conditions" == "array" ]]; then
                log_success "CRD schema updated and verified - conditions field is now available"
            else
                log_warn "CRD patched but verification failed. Schema may still be updating..."
                log_warn "Domain controller simulation will be skipped"
                SKIP_DOMAIN_SIMULATION=true
            fi
        else
            log_warn "Failed to patch CRD (this is OK in production clusters)"
            log_warn "Domain controller simulation will be skipped"
            log_warn "SnapshotContent will NOT be created in this cluster"
            SKIP_DOMAIN_SIMULATION=true
        fi
    else
        log_success "CRD $snapshot_crd has 'conditions' field in status schema"
    fi
    
    # Check BackupClass exists (if BackupClass CRD exists)
    if kubectl get crd backupclasses.storage.deckhouse.io &>/dev/null; then
        if ! kubectl get backupclass.storage.deckhouse.io "$BACKUP_CLASS_NAME" &>/dev/null; then
            log_error "BackupClass '$BACKUP_CLASS_NAME' not found"
            log_info "Available BackupClasses:"
            kubectl get backupclass.storage.deckhouse.io 2>/dev/null | head -10 || log_warn "Could not list BackupClasses"
            log_error "Snapshot creation will fail - BackupClass does not exist"
            log_info "Usage: $0 $NAMESPACE $SNAPSHOT_KIND <backup-class-name>"
            exit 1
        else
            log_success "BackupClass '$BACKUP_CLASS_NAME' found"
            # Show BackupClass details
            local repo_name=$(kubectl get backupclass.storage.deckhouse.io "$BACKUP_CLASS_NAME" -o jsonpath='{.spec.backupRepositoryName}' 2>/dev/null || echo "")
            if [[ -n "$repo_name" ]]; then
                log_info "  BackupRepository: $repo_name"
            fi
        fi
    else
        log_warn "BackupClass CRD not found (may be OK if not using BackupClass)"
    fi
    
    if ! kubectl get pods -n $CONTROLLER_NAMESPACE &>/dev/null; then
        log_warn "Controller namespace $CONTROLLER_NAMESPACE not found (may be OK)"
    fi
    
    log_success "Pre-flight checks passed"
    echo ""
    
    # Test 1: Create Snapshot
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 1: Create Snapshot"
    log_info "═══════════════════════════════════════════════════════════════"
    
    # Create Snapshot using short name or full resource name
    # backupClassName: Name of the BackupClass to use (binds us to a BackupRepository)
    # Required field - must reference an existing BackupClass
    cat <<EOF | kubectl apply -f -
apiVersion: $SNAPSHOT_API_GROUP/$SNAPSHOT_API_VERSION
kind: $SNAPSHOT_KIND
metadata:
  name: $SNAPSHOT_NAME
  namespace: $NAMESPACE
spec:
  backupClassName: $BACKUP_CLASS_NAME
EOF
    
    # Alternative: using short name
    # kubectl create $SNAPSHOT_SHORT $SNAPSHOT_NAME -n $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
    
    # Check using full resource name (e.g., snapshots.storage.deckhouse.io)
    local snapshot_resource="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    if check_resource_exists "$snapshot_resource" $SNAPSHOT_NAME $NAMESPACE; then
        log_success "Snapshot created"
    else
        log_error "Failed to create Snapshot"
        exit 1
    fi
    
    # Test 2: Simulate custom snapshot controller (set HandledByCustomSnapshotController condition)
    # IMPORTANT: SnapshotController waits for this condition before creating SnapshotContent
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 2: Simulate custom snapshot controller"
    log_info "═══════════════════════════════════════════════════════════════"
    
    local snapshot_resource="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    
    if [[ "${SKIP_DOMAIN_SIMULATION:-false}" == "true" ]]; then
        log_warn "Skipping custom snapshot controller simulation (CRD does not support conditions)"
        log_warn "SnapshotContent will NOT be created - this is expected"
    else
        log_info "Setting HandledByCustomSnapshotController=True condition..."
        
        # CRITICAL: Must use --subresource=status to patch status subresource
        # Without --subresource=status, Kubernetes will reject status.conditions
        # because status is declared as a subresource in the CRD
        local transition_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
        
        if command -v jq &>/dev/null; then
            # Build patch payload with conditions array
            # CRITICAL: When using --patch="...", payload must be wrapped in {"status": {...}}
            # kubectl does NOT automatically wrap payload for --subresource=status when using --patch="..."
            local patch_payload=$(jq -n --arg time "$transition_time" '{
                "status": {
                    "conditions": [{
                        "type": "HandledByCustomSnapshotController",
                        "status": "True",
                        "reason": "Processed",
                        "message": "Custom snapshot controller processed snapshot",
                        "lastTransitionTime": $time
                    }]
                }
            }')
            
            # Patch status subresource (CRITICAL: --subresource=status is required)
            kubectl patch "$snapshot_resource" "$SNAPSHOT_NAME" -n "$NAMESPACE" \
                --subresource=status \
                --type=merge \
                --patch="$patch_payload" || {
                log_error "Failed to set condition via kubectl patch --subresource=status"
                log_info "Current snapshot status:"
                kubectl get "$snapshot_resource" "$SNAPSHOT_NAME" -n "$NAMESPACE" -o jsonpath='{.status}' | jq '.' 2>/dev/null || echo "Status is empty"
                exit 1
            }
            
            # Verify condition was set
            sleep 1  # Small delay for API to update
            local condition_status=$(kubectl get "$snapshot_resource" "$SNAPSHOT_NAME" -n "$NAMESPACE" \
                -o jsonpath='{.status.conditions[?(@.type=="HandledByCustomSnapshotController")].status}' 2>/dev/null || echo "")
            if [[ "$condition_status" == "True" ]]; then
                log_success "Custom snapshot controller condition set and verified"
            else
                log_error "Condition was not set correctly (status: $condition_status)"
                log_info "Snapshot status:"
                kubectl get "$snapshot_resource" "$SNAPSHOT_NAME" -n "$NAMESPACE" -o jsonpath='{.status}' | jq '.' 2>/dev/null || kubectl get "$snapshot_resource" "$SNAPSHOT_NAME" -n "$NAMESPACE" -o yaml
                exit 1
            fi
            
            # Wait a bit for controller to receive the update event
            sleep 2
        else
            log_error "jq is required for setting conditions. Please install jq."
            exit 1
        fi
    fi
    
    # Test 3: Wait for SnapshotContent creation
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 3: Wait for SnapshotContent creation"
    log_info "═══════════════════════════════════════════════════════════════"
    
    # Get Snapshot UID to generate deterministic SnapshotContent name
    # Format: {snapshot-name}-content-{first-8-chars-of-uid}
    # Note: GenerateSnapshotContentName uses first 8 characters of UID (without dashes)
    local snapshot_uid=$(kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
    if [[ -z "$snapshot_uid" ]]; then
        log_error "Failed to get Snapshot UID"
        exit 1
    fi
    
    # Generate expected SnapshotContent name (deterministic pattern)
    # Remove dashes and take first 8 characters, convert to lowercase
    local uid_suffix=$(echo "$snapshot_uid" | tr -d '-' | cut -c1-8 | tr '[:upper:]' '[:lower:]')
    local expected_content_name="${SNAPSHOT_NAME}-content-${uid_suffix}"
    local content_resource="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    
    log_info "Waiting for SnapshotContent creation (expected name: $expected_content_name)..."
    
    # Initialize CONTENT_NAME as empty string
    CONTENT_NAME=""
    
    local elapsed=0
    while [[ $elapsed -lt 60 ]]; do  # Increased timeout to 60s
        # Check if SnapshotContent exists directly (more reliable than status.boundSnapshotContentName)
        if kubectl get "$content_resource" "$expected_content_name" &>/dev/null; then
            CONTENT_NAME="$expected_content_name"
            log_success "SnapshotContent created: $CONTENT_NAME"
            break
        fi
        # Also try to get from status.boundSnapshotContentName (as per CRD schema)
        local content_name=$(kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE -o jsonpath='{.status.boundSnapshotContentName}' 2>/dev/null || echo "")
        if [[ -n "$content_name" ]]; then
            CONTENT_NAME="$content_name"
            log_success "SnapshotContent created: $CONTENT_NAME (from status.boundSnapshotContentName)"
            break
        fi
        sleep 2  # Check every 2 seconds
        elapsed=$((elapsed + 2))
    done
    
    if [[ -z "$CONTENT_NAME" ]]; then
        log_error "SnapshotContent was not created (timeout after 60s)"
        log_info "Snapshot status:"
        kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE -o yaml
        log_info "Controller logs (last 20 lines):"
        kubectl logs -n $CONTROLLER_NAMESPACE -l app=controller --tail=20 2>/dev/null || true
        exit 1
    fi
    
    # Test 4: Check SnapshotContent exists
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 4: Check SnapshotContent exists"
    log_info "═══════════════════════════════════════════════════════════════"
    
    local content_resource="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    if check_resource_exists "$content_resource" $CONTENT_NAME; then
        log_success "SnapshotContent resource exists"
        
        # Check for legacy format (snapshotRef.kind is empty)
        local legacy_kind=$(kubectl get "$content_resource" "$CONTENT_NAME" \
            -o jsonpath='{.spec.snapshotRef.kind}' 2>/dev/null || echo "")
        if [[ -z "$legacy_kind" ]]; then
            log_warn "SnapshotContent uses legacy format: spec.snapshotRef.kind is empty (fallback logic will be used in controller)"
            log_warn "Recommendation: Run migration or recreate SnapshotContent to set snapshotRef.kind explicitly"
        else
            log_success "SnapshotContent uses modern format: spec.snapshotRef.kind=$legacy_kind"
        fi
    else
        log_error "SnapshotContent resource does not exist"
        exit 1
    fi
    
    # Test 5: Check finalizer on SnapshotContent
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 5: Check finalizer on SnapshotContent"
    log_info "═══════════════════════════════════════════════════════════════"
    
    local content_resource="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    local finalizers=$(kubectl get "$content_resource" $CONTENT_NAME -o jsonpath='{.metadata.finalizers[*]}' 2>/dev/null || echo "")
    if echo "$finalizers" | grep -q "snapshot.deckhouse.io/parent-protect"; then
        log_success "Finalizer 'snapshot.deckhouse.io/parent-protect' found"
    else
        log_error "Finalizer 'snapshot.deckhouse.io/parent-protect' not found (finalizers: $finalizers)"
    fi
    
    # Test 6: Simulate SnapshotContent Ready (set Ready=True on SnapshotContent)
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 6: Simulate SnapshotContent Ready"
    log_info "═══════════════════════════════════════════════════════════════"
    
    local content_resource="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    # Use kubectl patch with merge patch directly to status subresource
    local transition_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    
    # Get existing conditions to merge properly
    # jsonpath returns empty string if field doesn't exist, so we need to handle that
    local existing_conditions_raw=$(kubectl get "$content_resource" $CONTENT_NAME -o jsonpath='{.status.conditions}' 2>/dev/null || echo "")
    local existing_conditions="[]"
    if [[ -n "$existing_conditions_raw" ]]; then
        # Try to parse as JSON to validate
        if echo "$existing_conditions_raw" | jq . >/dev/null 2>&1; then
            existing_conditions="$existing_conditions_raw"
        fi
    fi
    
    if command -v jq &>/dev/null; then
        # Build new conditions
        local ready_condition=$(jq -n --arg time "$transition_time" '{
            "type": "Ready",
            "status": "True",
            "reason": "AllArtifactsReady",
            "message": "All artifacts are ready",
            "lastTransitionTime": $time
        }')
        local inprogress_condition=$(jq -n --arg time "$transition_time" '{
            "type": "InProgress",
            "status": "False",
            "reason": "Completed",
            "message": "Snapshot completed",
            "lastTransitionTime": $time
        }')
        
        # Merge with existing conditions (remove old Ready and InProgress if exist)
        local updated_conditions=$(echo "$existing_conditions" | jq --argjson ready "$ready_condition" --argjson inprogress "$inprogress_condition" '
            (. // []) | map(select(.type != "Ready" and .type != "InProgress")) + [$ready, $inprogress]
        ')
        
        # Use merge patch to update status.conditions
        # CRITICAL: When using --patch-file=/dev/stdin, payload should be wrapped in {"status": {...}}
        # for consistency, even though --patch-file may handle it differently
        local merge_patch=$(jq -n --argjson conditions "$updated_conditions" '{
            "status": {
                "conditions": $conditions
            }
        }')
        
        # Patch status subresource with merge patch
        echo "$merge_patch" | kubectl patch "$content_resource" $CONTENT_NAME --subresource=status --type=merge --patch-file=/dev/stdin || {
            log_error "Failed to set SnapshotContent Ready condition via kubectl patch --subresource=status"
            log_info "Trying to get current status:"
            kubectl get "$content_resource" $CONTENT_NAME -o jsonpath='{.status}' | jq '.' 2>/dev/null || echo "Status is empty"
            exit 1
        }
        
        # Verify condition was set
        sleep 1  # Small delay for API to update
        local ready_status=$(kubectl get "$content_resource" $CONTENT_NAME -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
        if [[ "$ready_status" == "True" ]]; then
            log_success "SnapshotContent Ready condition set and verified"
        else
            log_error "Ready condition was not set correctly (status: $ready_status)"
            kubectl get "$content_resource" $CONTENT_NAME -o yaml
            exit 1
        fi
    else
        log_error "jq is required for setting conditions. Please install jq."
        exit 1
    fi
    
    # Wait a bit for controller to receive the update event
    sleep 2
    
    # Test 7: Wait for Snapshot Ready (propagation from SnapshotContent)
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 7: Wait for Snapshot Ready"
    log_info "═══════════════════════════════════════════════════════════════"
    
    local snapshot_resource="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    if wait_for_condition "$snapshot_resource" $SNAPSHOT_NAME "Ready" "True" $NAMESPACE 30; then
        log_success "Snapshot reached Ready=True"
    else
        log_error "Snapshot did not reach Ready=True"
        kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE -o yaml
        exit 1
    fi
    
    # Test 8: Check ObjectKeeper (for root snapshots, best-effort)
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 8: Check ObjectKeeper (best-effort)"
    log_info "═══════════════════════════════════════════════════════════════"
    
    if kubectl get crd objectkeepers.deckhouse.io &>/dev/null; then
        local ok_name="ret-${SNAPSHOT_KIND,,}-${SNAPSHOT_NAME}"
        if kubectl get objectkeeper.deckhouse.io $ok_name &>/dev/null; then
            log_success "ObjectKeeper created: $ok_name"
        else
            log_warn "ObjectKeeper not found (may be OK for child snapshots)"
        fi
    else
        log_warn "ObjectKeeper CRD not found (skipping check)"
    fi
    
    # Test 9: Delete Snapshot and check deletion behavior (finalizer/GC)
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 9: Delete Snapshot and check deletion behavior"
    log_info "═══════════════════════════════════════════════════════════════"
    
    local snapshot_resource="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    local content_resource="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"
    
    kubectl delete "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE --wait=false
    
    # Wait for Snapshot deletion
    log_info "Waiting for Snapshot deletion..."
    local elapsed=0
    local snapshot_deleted=false
    while [[ $elapsed -lt 30 ]]; do
        if ! kubectl get "$snapshot_resource" $SNAPSHOT_NAME -n $NAMESPACE &>/dev/null; then
            log_success "Snapshot deleted"
            snapshot_deleted=true
            break
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    
    if [[ "$snapshot_deleted" == "false" ]]; then
        log_error "Snapshot deletion timeout (still exists after 30s)"
    fi
    
    # Wait for SnapshotContent to be deleted by GC or for finalizer to be removed
    log_info "Waiting for SnapshotContent to be deleted or finalizer removed..."
    local content_deleted=false
    local finalizer_removed=false
    local logged_finalizer_removed=false
    local elapsed_gc=0
    local gc_wait_timeout=120
    while [[ $elapsed_gc -lt $gc_wait_timeout ]]; do
        if ! kubectl get "$content_resource" $CONTENT_NAME &>/dev/null; then
            content_deleted=true
            break
        fi
        if [[ "$finalizer_removed" != "true" ]]; then
            local finalizers_after=$(kubectl get "$content_resource" $CONTENT_NAME -o jsonpath='{.metadata.finalizers[*]}' 2>/dev/null || echo "")
            if ! echo "$finalizers_after" | grep -q "snapshot.deckhouse.io/parent-protect"; then
                finalizer_removed=true
            fi
        fi
        if [[ "$finalizer_removed" == "true" && "$logged_finalizer_removed" != "true" ]]; then
            log_info "SnapshotContent finalizer removed; waiting for GC deletion..."
            logged_finalizer_removed=true
        fi
        sleep 1
        elapsed_gc=$((elapsed_gc + 1))
    done

    if [[ "$content_deleted" == "true" ]]; then
        log_success "SnapshotContent deleted by GC after Snapshot removal"
    else
        log_error "SnapshotContent was not deleted within ${gc_wait_timeout}s after Snapshot deletion"
    fi
    
    # Test 10: (Optional) Check reconcile count in logs
    log_info ""
    log_info "═══════════════════════════════════════════════════════════════"
    log_info "Test 10: Check reconcile count (optional)"
    log_info "═══════════════════════════════════════════════════════════════"
    
    if kubectl get pods -n $CONTROLLER_NAMESPACE &>/dev/null; then
        local reconcile_count=$(kubectl logs -n $CONTROLLER_NAMESPACE -l app=controller --tail=1000 2>/dev/null | \
            grep "SnapshotContent reconciliation completed" | \
            grep "$CONTENT_NAME" | wc -l | tr -d ' ')
        
        if [[ -n "$reconcile_count" ]] && [[ "$reconcile_count" -gt 0 ]]; then
            if [[ $reconcile_count -le 10 ]]; then
                log_success "Reconcile count: $reconcile_count (normal range)"
            else
                log_warn "Reconcile count: $reconcile_count (higher than expected, may indicate issues)"
            fi
        else
            log_warn "Could not determine reconcile count from logs"
        fi
    else
        log_warn "Controller namespace not found, skipping reconcile count check"
    fi
    
    # Summary
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "📊 Test Summary"
    echo "═══════════════════════════════════════════════════════════════"
    echo "  Tests passed: $TESTS_PASSED"
    echo "  Tests failed: $TESTS_FAILED"
    echo ""
    
    if [[ $TESTS_FAILED -eq 0 ]]; then
        echo -e "${GREEN}✅ All tests passed!${NC}"
        exit 0
    else
        echo -e "${RED}❌ Some tests failed${NC}"
        exit 1
    fi
}

# Run main function
main "$@"

