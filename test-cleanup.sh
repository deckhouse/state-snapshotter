#!/bin/bash
#
# Cleanup script for Unified Snapshots test resources
# Safely removes test Snapshots, SnapshotContents, and related resources
#
# Usage:
#   ./test-cleanup.sh [options]
#
# Options:
#   --snapshot-name NAME     Cleanup specific Snapshot by name
#   --namespace NAMESPACE    Cleanup resources in namespace (default: default)
#   --snapshot-kind KIND     Snapshot kind (default: Snapshot)
#   --all                    Cleanup all test resources (by label)
#   --dry-run                Show what would be deleted without actually deleting
#   --force                  Force cleanup (remove finalizers)
#
# Examples:
#   ./test-cleanup.sh --snapshot-name test-smoke-1234567890
#   ./test-cleanup.sh --all --namespace default
#   ./test-cleanup.sh --snapshot-name test-smoke-1234567890 --force

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
NAMESPACE="default"
SNAPSHOT_KIND="Snapshot"
SNAPSHOT_NAME=""
CLEANUP_ALL=false
DRY_RUN=false
FORCE=false
SNAPSHOT_API_GROUP="storage.deckhouse.io"
SNAPSHOT_API_VERSION="v1alpha1"

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[✓]${NC} $*"
}

log_error() {
    echo -e "${RED}[✗]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[!]${NC} $*"
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --snapshot-name)
            SNAPSHOT_NAME="$2"
            shift 2
            ;;
        --namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        --snapshot-kind)
            SNAPSHOT_KIND="$2"
            shift 2
            ;;
        --all)
            CLEANUP_ALL=true
            shift
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --force)
            FORCE=true
            shift
            ;;
        --help|-h)
            cat <<EOF
Cleanup script for Unified Snapshots test resources

Usage:
  $0 [options]

Options:
  --snapshot-name NAME     Cleanup specific Snapshot by name
  --namespace NAMESPACE    Cleanup resources in namespace (default: default)
  --snapshot-kind KIND     Snapshot kind (default: Snapshot)
  --all                    Cleanup all test resources (by name pattern)
  --dry-run                Show what would be deleted without actually deleting
  --force                  Force cleanup (remove finalizers)
  --help, -h               Show this help message

Examples:
  $0 --snapshot-name test-smoke-1234567890
  $0 --all --namespace default
  $0 --snapshot-name test-smoke-1234567890 --force
EOF
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# Validate arguments
if [[ "$CLEANUP_ALL" == "false" ]] && [[ -z "$SNAPSHOT_NAME" ]]; then
    log_error "Either --snapshot-name or --all must be specified"
    exit 1
fi

if [[ "$DRY_RUN" == "true" ]]; then
    log_warn "DRY RUN MODE - No resources will be deleted"
fi

# Resource names
# Note: Kubernetes CRDs always use plural form for resource names
SNAPSHOT_RESOURCE="${SNAPSHOT_KIND,,}s.${SNAPSHOT_API_GROUP}"
CONTENT_KIND="${SNAPSHOT_KIND}Content"
CONTENT_RESOURCE="${CONTENT_KIND,,}s.${SNAPSHOT_API_GROUP}"  # Note: plural form (snapshotcontents, not snapshotcontent)

# Function to cleanup a specific Snapshot
cleanup_snapshot() {
    local snapshot_name=$1
    local namespace=$2
    
    log_info "Cleaning up Snapshot: $snapshot_name in namespace $namespace"
    
    # Check if Snapshot exists
    if ! kubectl get "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" &>/dev/null; then
        log_warn "Snapshot $snapshot_name not found in namespace $namespace"
        return 0
    fi
    
    # Get SnapshotContent name
    local content_name=$(kubectl get "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" \
        -o jsonpath='{.status.contentName}' 2>/dev/null || echo "")
    
    # Delete Snapshot
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would delete: $SNAPSHOT_RESOURCE/$snapshot_name in namespace $namespace"
    else
        log_info "Deleting Snapshot: $snapshot_name"
        kubectl delete "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" --wait=false || true
    fi
    
    # Wait for Snapshot deletion or force cleanup
    if [[ "$DRY_RUN" == "false" ]]; then
        local elapsed=0
        while [[ $elapsed -lt 30 ]]; do
            if ! kubectl get "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" &>/dev/null; then
                log_success "Snapshot deleted"
                break
            fi
            sleep 1
            elapsed=$((elapsed + 1))
        done
        
        # If still exists and force is enabled, remove finalizers
        if kubectl get "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" &>/dev/null; then
            if [[ "$FORCE" == "true" ]]; then
                log_warn "Snapshot still exists, removing finalizers..."
                kubectl patch "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" \
                    -p '{"metadata":{"finalizers":[]}}' --type=merge || true
                kubectl delete "$SNAPSHOT_RESOURCE" "$snapshot_name" -n "$namespace" --wait=false || true
            else
                log_warn "Snapshot still exists (may have finalizers). Use --force to remove finalizers"
            fi
        fi
    fi
    
    # Cleanup SnapshotContent if found
    if [[ -n "$content_name" ]]; then
        cleanup_content "$content_name"
    fi
}

# Function to cleanup SnapshotContent
cleanup_content() {
    local content_name=$1
    
    log_info "Cleaning up SnapshotContent: $content_name"
    
    # Check if SnapshotContent exists
    if ! kubectl get "$CONTENT_RESOURCE" "$content_name" &>/dev/null; then
        log_warn "SnapshotContent $content_name not found"
        return 0
    fi
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[DRY RUN] Would delete: $CONTENT_RESOURCE/$content_name"
        
        # Show finalizers
        local finalizers=$(kubectl get "$CONTENT_RESOURCE" "$content_name" \
            -o jsonpath='{.metadata.finalizers[*]}' 2>/dev/null || echo "")
        if [[ -n "$finalizers" ]]; then
            log_info "[DRY RUN] Finalizers: $finalizers"
        fi
        return 0
    fi
    
    # Remove finalizers if force is enabled
    if [[ "$FORCE" == "true" ]]; then
        local finalizers=$(kubectl get "$CONTENT_RESOURCE" "$content_name" \
            -o jsonpath='{.metadata.finalizers[*]}' 2>/dev/null || echo "")
        if [[ -n "$finalizers" ]]; then
            log_warn "Removing finalizers from SnapshotContent: $content_name"
            kubectl patch "$CONTENT_RESOURCE" "$content_name" \
                -p '{"metadata":{"finalizers":[]}}' --type=merge || true
        fi
    fi
    
    # Delete SnapshotContent
    log_info "Deleting SnapshotContent: $content_name"
    kubectl delete "$CONTENT_RESOURCE" "$content_name" --wait=false || true
    
    # Wait for deletion
    local elapsed=0
    while [[ $elapsed -lt 30 ]]; do
        if ! kubectl get "$CONTENT_RESOURCE" "$content_name" &>/dev/null; then
            log_success "SnapshotContent deleted"
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    
    if kubectl get "$CONTENT_RESOURCE" "$content_name" &>/dev/null; then
        log_warn "SnapshotContent still exists (may have finalizers). Use --force to remove finalizers"
    fi
}

# Function to cleanup ObjectKeeper
cleanup_objectkeeper() {
    local snapshot_name=$1
    local ok_name="ret-${SNAPSHOT_KIND,,}-${snapshot_name}"
    
    if ! kubectl get crd objectkeepers.deckhouse.io &>/dev/null; then
        return 0
    fi
    
    if kubectl get objectkeeper.deckhouse.io "$ok_name" &>/dev/null; then
        log_info "Cleaning up ObjectKeeper: $ok_name"
        
        if [[ "$DRY_RUN" == "true" ]]; then
            log_info "[DRY RUN] Would delete: objectkeeper.deckhouse.io/$ok_name"
        else
            kubectl delete objectkeeper.deckhouse.io "$ok_name" --wait=false || true
            log_success "ObjectKeeper deleted"
        fi
    fi
}

# Main cleanup function
main() {
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    echo "🧹 Unified Snapshots Cleanup"
    echo "═══════════════════════════════════════════════════════════════"
    echo ""
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log_warn "DRY RUN MODE - No resources will be deleted"
        echo ""
    fi
    
    if [[ "$CLEANUP_ALL" == "true" ]]; then
        log_info "Cleaning up all test resources in namespace: $NAMESPACE"
        log_info "Looking for Snapshots matching pattern: test-smoke-*"
        
        # Find all test snapshots
        local snapshots=$(kubectl get "$SNAPSHOT_RESOURCE" -n "$NAMESPACE" \
            -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        
        if [[ -z "$snapshots" ]]; then
            log_info "No test Snapshots found in namespace $NAMESPACE"
            return 0
        fi
        
        # Cleanup each snapshot
        for snapshot in $snapshots; do
            if [[ "$snapshot" =~ ^test-smoke- ]]; then
                cleanup_snapshot "$snapshot" "$NAMESPACE"
                cleanup_objectkeeper "$snapshot"
                echo ""
            fi
        done
        
        # Also cleanup orphaned SnapshotContents
        log_info "Checking for orphaned SnapshotContents..."
        local contents=$(kubectl get "$CONTENT_RESOURCE" \
            -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
        
        for content in $contents; do
            if [[ "$content" =~ ^snapcontent- ]] || [[ "$content" =~ test-smoke ]]; then
                log_warn "Found potentially orphaned SnapshotContent: $content"
                if [[ "$DRY_RUN" == "false" ]] && [[ "$FORCE" == "true" ]]; then
                    cleanup_content "$content"
                else
                    log_info "Use --force to cleanup orphaned content: $content"
                fi
            fi
        done
    else
        # Cleanup specific snapshot
        cleanup_snapshot "$SNAPSHOT_NAME" "$NAMESPACE"
        cleanup_objectkeeper "$SNAPSHOT_NAME"
    fi
    
    echo ""
    echo "═══════════════════════════════════════════════════════════════"
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "Dry run completed. No resources were deleted."
    else
        log_success "Cleanup completed!"
    fi
    echo "═══════════════════════════════════════════════════════════════"
    echo ""
}

# Run cleanup
main

