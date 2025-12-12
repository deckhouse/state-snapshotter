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

package usecase

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// ArchiveService handles archive operations
type ArchiveService struct {
	client      client.Client // For ManifestCheckpoint (direct, no cache, no list/watch needed)
	chunkClient client.Client // For chunks (direct, no cache, no list/watch needed)
	logger      logger.LoggerInterface
	cache       *ArchiveCache

	// Limits for DoS protection
	maxObjectsPerCheckpoint int
	maxArchiveSizeBytes     int64
}

// ArchiveCache provides caching for merged archives
type ArchiveCache struct {
	mu    sync.RWMutex
	items map[string]*CacheItem
}

// CacheItem represents a cached archive
type CacheItem struct {
	Data      []byte
	Checksum  string
	CreatedAt time.Time
	TTL       time.Duration
}

// NewArchiveService creates a new ArchiveService
// client: used for ManifestCheckpoint (direct, no cache, no list/watch needed)
// chunkClient: used for chunks (direct, no cache, no list/watch needed)
// Both use direct client to avoid informer requirements
func NewArchiveService(client client.Client, chunkClient client.Client, logger logger.LoggerInterface) *ArchiveService {
	return &ArchiveService{
		client:      client,
		chunkClient: chunkClient,
		logger:      logger,
		cache: &ArchiveCache{
			items: make(map[string]*CacheItem),
		},
		// Default limits: 10k objects, 100MB archive
		maxObjectsPerCheckpoint: 10000,
		maxArchiveSizeBytes:     100 * 1024 * 1024,
	}
}

// ArchiveRequest represents a request to download an archive
type ArchiveRequest struct {
	CheckpointName  string
	CheckpointUID   string // Used for cache key to detect checkpoint regeneration
	SourceNamespace string // For RBAC validation
}

// ArchiveInfo contains information about an archive
type ArchiveInfo struct {
	CheckpointName  string
	SourceNamespace string
	FileCount       int
	EstimatedSize   int64
}

// GetArchive retrieves and merges all chunks for a checkpoint into a single archive
// This is a convenience wrapper that fetches the checkpoint first
func (s *ArchiveService) GetArchive(ctx context.Context, req *ArchiveRequest) ([]byte, string, error) {
	// Validate checkpoint exists and is ready
	var checkpoint storagev1alpha1.ManifestCheckpoint
	if err := s.client.Get(ctx, types.NamespacedName{Name: req.CheckpointName}, &checkpoint); err != nil {
		if errors.IsNotFound(err) {
			return nil, "", fmt.Errorf("checkpoint %s not found", req.CheckpointName)
		}
		return nil, "", fmt.Errorf("failed to get checkpoint: %w", err)
	}

	return s.GetArchiveFromCheckpoint(ctx, &checkpoint, req)
}

// GetArchiveFromCheckpoint retrieves and merges all chunks from an already-loaded checkpoint
// This avoids duplicate Get/Ready checks when the checkpoint is already available
func (s *ArchiveService) GetArchiveFromCheckpoint(ctx context.Context, checkpoint *storagev1alpha1.ManifestCheckpoint, req *ArchiveRequest) ([]byte, string, error) {
	// Check cache first (UID-based to detect checkpoint regeneration)
	cacheKey := s.GetCacheKey(req.CheckpointName, req.CheckpointUID)
	if cached := s.cache.get(cacheKey); cached != nil {
		s.logger.Info("Cache hit for archive", "key", cacheKey)
		return cached.Data, cached.Checksum, nil
	}

	// Validate SourceNamespace matches
	// Note: This check currently serves as an invariant check, not RBAC protection,
	// because SourceNamespace in req is always taken from checkpoint.Spec.SourceNamespace.
	// It ensures consistency but doesn't protect against namespace-based access control.
	if checkpoint.Spec.SourceNamespace != req.SourceNamespace {
		return nil, "", fmt.Errorf("source namespace mismatch: checkpoint has %s, requested %s",
			checkpoint.Spec.SourceNamespace, req.SourceNamespace)
	}

	// Validate checkpoint is ready (using Ready condition)
	readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, "Ready")
	if readyCondition == nil || readyCondition.Status != metav1.ConditionTrue {
		reason := "Unknown"
		if readyCondition != nil {
			reason = readyCondition.Reason
		}
		return nil, "", fmt.Errorf("checkpoint is not ready: Ready condition status=%s, reason=%s",
			func() string {
				if readyCondition == nil {
					return "Unknown"
				}
				return string(readyCondition.Status)
			}(), reason)
	}

	// Validate UID matches (detect checkpoint regeneration)
	if string(checkpoint.UID) != req.CheckpointUID {
		s.logger.Warning("Checkpoint UID mismatch - checkpoint was regenerated",
			"checkpoint", req.CheckpointName,
			"expectedUID", req.CheckpointUID,
			"actualUID", string(checkpoint.UID))
		// Continue anyway, but cache will use new UID
	}

	// Check object count limit
	if checkpoint.Status.TotalObjects > s.maxObjectsPerCheckpoint {
		return nil, "", fmt.Errorf("checkpoint exceeds object limit: %d > %d", checkpoint.Status.TotalObjects, s.maxObjectsPerCheckpoint)
	}

	// Collect objects from chunks
	objects, err := s.collectObjectsFromCheckpoint(ctx, req.CheckpointName, checkpoint)
	if err != nil {
		return nil, "", fmt.Errorf("failed to collect objects from checkpoint: %w", err)
	}

	s.logger.Info("Collected objects from checkpoint",
		"checkpoint", req.CheckpointName,
		"totalObjects", len(objects))

	// Check archive size limit after creation
	if checkpoint.Status.TotalSizeBytes > s.maxArchiveSizeBytes {
		s.logger.Warning("Checkpoint archive size exceeds limit",
			"checkpoint", req.CheckpointName,
			"size", checkpoint.Status.TotalSizeBytes,
			"limit", s.maxArchiveSizeBytes)
		// Continue anyway, but log warning
	}

	// Create JSON archive
	s.logger.Debug("Creating JSON archive", "checkpoint", req.CheckpointName, "objects", len(objects))
	archiveData, err := s.createJSONArchive(objects)
	if err != nil {
		s.logger.Error(err, "Failed to create archive", "checkpoint", req.CheckpointName)
		return nil, "", err
	}

	s.logger.Info("Archive created successfully",
		"checkpoint", req.CheckpointName,
		"size", len(archiveData))

	// Calculate checksum
	checksum := s.calculateChecksum(archiveData)

	// Cache the result (using actual checkpoint UID)
	actualCacheKey := s.GetCacheKey(req.CheckpointName, string(checkpoint.UID))
	s.cache.set(actualCacheKey, &CacheItem{
		Data:      archiveData,
		Checksum:  checksum,
		CreatedAt: time.Now(),
		TTL:       5 * time.Minute,
	})

	return archiveData, checksum, nil
}

// collectObjectsFromCheckpoint collects objects from ManifestCheckpoint chunks
// Validates chunk sequence and ownership
func (s *ArchiveService) collectObjectsFromCheckpoint(ctx context.Context, checkpointName string, checkpoint *storagev1alpha1.ManifestCheckpoint) ([]unstructured.Unstructured, error) {
	// Sort chunks by index
	chunks := checkpoint.Status.Chunks
	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].Index < chunks[j].Index
	})

	if len(chunks) == 0 {
		return nil, fmt.Errorf("no chunks found in ManifestCheckpoint %s", checkpointName)
	}

	// Validate chunk index sequence (must be 0, 1, 2, ...)
	for i, chunkInfo := range chunks {
		if chunkInfo.Index != i {
			return nil, fmt.Errorf("invalid chunk index sequence: expected %d, got %d (chunk: %s)", i, chunkInfo.Index, chunkInfo.Name)
		}
	}

	allObjects := make([]unstructured.Unstructured, 0)
	var mu sync.Mutex

	// Create worker pool with errgroup
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(10) // Process up to 10 chunks in parallel

	for _, chunkInfo := range chunks {
		// Capture loop variable
		g.Go(func() error {
			// Add timeout for each chunk request
			ctx, cancel := context.WithTimeout(gCtx, 5*time.Second)
			defer cancel()

			// Get chunk using direct client (no informer cache, no list/watch needed)
			var chunk storagev1alpha1.ManifestCheckpointContentChunk
			if err := s.chunkClient.Get(ctx, types.NamespacedName{Name: chunkInfo.Name}, &chunk); err != nil {
				if errors.IsNotFound(err) {
					// Missing chunk is a fatal error - archive would be incomplete
					return fmt.Errorf("chunk missing: %s (index: %d, checkpoint: %s)", chunkInfo.Name, chunkInfo.Index, checkpointName)
				}
				return fmt.Errorf("failed to get chunk %s: %w", chunkInfo.Name, err)
			}

			// Validate chunk belongs to this checkpoint
			if chunk.Spec.CheckpointName != checkpointName {
				return fmt.Errorf("chunk %s does not belong to checkpoint %s (belongs to: %s)", chunkInfo.Name, checkpointName, chunk.Spec.CheckpointName)
			}

			// Validate chunk index matches
			if chunk.Spec.Index != chunkInfo.Index {
				return fmt.Errorf("chunk %s index mismatch: expected %d, got %d", chunkInfo.Name, chunkInfo.Index, chunk.Spec.Index)
			}

			// Decode and decompress chunk data with checksum verification
			objects, err := s.decodeChunkDataWithChecksum(chunk.Spec.Data, chunkInfo.Checksum, chunkInfo.Name)
			if err != nil {
				return fmt.Errorf("failed to decode chunk %s: %w", chunkInfo.Name, err)
			}

			// Collect objects
			mu.Lock()
			allObjects = append(allObjects, objects...)
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return allObjects, nil
}

// decodeChunkDataWithChecksum decodes base64(gzip(json[])) data with checksum verification
func (s *ArchiveService) decodeChunkDataWithChecksum(encodedData string, expectedChecksum string, chunkName string) ([]unstructured.Unstructured, error) {
	// Decode base64
	data, err := base64.StdEncoding.DecodeString(encodedData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	// Verify checksum if provided
	if expectedChecksum != "" {
		hash := sha256.Sum256(data)
		actualChecksum := hex.EncodeToString(hash[:])
		if actualChecksum != expectedChecksum {
			return nil, fmt.Errorf("checksum mismatch for chunk %s: expected %s, got %s", chunkName, expectedChecksum, actualChecksum)
		}
	}

	// Decompress gzip
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress: %w", err)
	}

	// Parse JSON array
	var jsonArray []interface{}
	if err := json.Unmarshal(decompressed, &jsonArray); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON array: %w", err)
	}

	s.logger.Debug("Decoded chunk data",
		"chunk", chunkName,
		"itemsCount", len(jsonArray),
		"decompressedSize", len(decompressed))

	// Check if data is in Key/Value format (array of Key/Value arrays)
	if len(jsonArray) > 0 {
		if firstItem, ok := jsonArray[0].([]interface{}); ok {
			if len(firstItem) > 0 {
				if firstPair, ok := firstItem[0].(map[string]interface{}); ok {
					if _, hasKey := firstPair["Key"]; hasKey {
						if _, hasValue := firstPair["Value"]; hasValue {
							s.logger.Info("Chunk data is in Key/Value format, will convert",
								"chunk", chunkName,
								"itemsCount", len(jsonArray))
						}
					}
				}
			}
		}
	}

	// Convert to unstructured objects, handling Key/Value format if present
	objects := make([]unstructured.Unstructured, 0, len(jsonArray))
	for i, item := range jsonArray {
		// Check if item is an array of Key/Value pairs (yaml.MapSlice serialized as JSON array)
		if itemArray, ok := item.([]interface{}); ok {
			// This is an array of Key/Value pairs - convert to map
			s.logger.Debug("Detected Key/Value array in chunk, converting to map",
				"chunk", chunkName,
				"index", i,
				"pairsCount", len(itemArray))

			objMap := s.convertKeyValueArrayToMap(itemArray)
			if objMap == nil {
				s.logger.Warning("Failed to convert Key/Value array to map, skipping",
					"chunk", chunkName,
					"index", i)
				continue
			}

			obj := unstructured.Unstructured{Object: objMap}
			objects = append(objects, obj)
			continue
		}

		// Regular map object
		objMap, ok := item.(map[string]interface{})
		if !ok {
			s.logger.Warning("Skipping non-map item in chunk",
				"chunk", chunkName,
				"index", i,
				"type", reflect.TypeOf(item).String())
			continue
		}

		// Convert any nested Key/Value structures
		objMap = s.convertMapSliceToMap(objMap).(map[string]interface{})

		obj := unstructured.Unstructured{Object: objMap}
		objects = append(objects, obj)
	}

	s.logger.Debug("Converted chunk to unstructured objects",
		"chunk", chunkName,
		"objectsCount", len(objects))

	return objects, nil
}

// createJSONArchive creates a JSON array with all resources
func (s *ArchiveService) createJSONArchive(objects []unstructured.Unstructured) ([]byte, error) {
	// Convert unstructured objects to JSON-serializable maps
	jsonObjects := make([]interface{}, 0, len(objects))
	for _, obj := range objects {
		// Convert object to map and normalize (handle yaml.MapSlice if present)
		objMap := s.convertMapSliceToMap(obj.Object)

		// Filter out status and managedFields from metadata
		// These fields are runtime-specific and should not be included in snapshots
		if objMapMap, ok := objMap.(map[string]interface{}); ok {
			// Remove status field
			delete(objMapMap, "status")

			// Remove managedFields from metadata if present
			if metadata, ok := objMapMap["metadata"].(map[string]interface{}); ok {
				delete(metadata, "managedFields")
			}
		}

		jsonObjects = append(jsonObjects, objMap)
	}

	// Marshal to JSON with indentation
	jsonData, err := json.MarshalIndent(jsonObjects, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal to JSON: %w", err)
	}

	// Debug: log first 500 bytes of JSON to verify format
	if len(jsonData) > 0 {
		previewLen := 500
		if len(jsonData) < previewLen {
			previewLen = len(jsonData)
		}
		preview := string(jsonData[:previewLen])
		s.logger.Debug("Generated JSON archive preview",
			"totalSize", len(jsonData),
			"objectsCount", len(objects),
			"preview", preview)

		// Check if JSON contains Key/Value structure
		if strings.Contains(preview, `"Key"`) && strings.Contains(preview, `"Value"`) {
			s.logger.Warning("JSON archive contains Key/Value structure - conversion may have failed",
				"preview", preview)
		}
	}

	return jsonData, nil
}

// convertKeyValueArrayToMap converts an array of Key/Value pairs to a map[string]interface{}
// This handles yaml.MapSlice that was serialized as JSON array of {Key, Value} objects
func (s *ArchiveService) convertKeyValueArrayToMap(keyValueArray []interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for _, pair := range keyValueArray {
		pairMap, ok := pair.(map[string]interface{})
		if !ok {
			continue
		}

		// Check if this is a Key/Value pair
		keyVal, hasKey := pairMap["Key"]
		valueVal, hasValue := pairMap["Value"]

		if !hasKey || !hasValue {
			continue
		}

		// Convert key to string
		var keyStr string
		if str, ok := keyVal.(string); ok {
			keyStr = str
		} else {
			keyStr = fmt.Sprint(keyVal)
		}

		// Recursively convert value (it might be another Key/Value array or nested structure)
		result[keyStr] = s.convertMapSliceToMap(valueVal)
	}

	return result
}

// convertMapSliceToMap converts yaml.MapSlice or any nested structure to map[string]interface{} recursively
// This ensures proper JSON serialization without Key/Value structure
// yaml.MapSlice serializes to Key/Value structure in JSON, so we need to convert it properly
// yaml.MapSlice is not exported, so we use reflection to detect it
func (s *ArchiveService) convertMapSliceToMap(v interface{}) interface{} {
	if v == nil {
		return nil
	}

	val := reflect.ValueOf(v)
	typ := val.Type()

	// Check if it's yaml.MapSlice by checking the type name and structure
	// yaml.MapSlice is []yaml.MapItem where MapItem is struct{Key, Value interface{}}
	// After yaml.Unmarshal, it becomes []map[string]interface{} with "Key" and "Value" keys
	if typ.Kind() == reflect.Slice {
		if val.Len() > 0 {
			firstElem := val.Index(0)
			firstElemInterface := firstElem.Interface()

			// Check if first element is a map with "Key" and "Value" keys (yaml.MapSlice after JSON unmarshal)
			if firstElemMap, ok := firstElemInterface.(map[string]interface{}); ok {
				if _, hasKey := firstElemMap["Key"]; hasKey {
					if _, hasValue := firstElemMap["Value"]; hasValue {
						// This is a slice of Key/Value maps - convert to map
						// Convert slice to []interface{} for convertKeyValueArrayToMap
						sliceInterface := make([]interface{}, val.Len())
						for i := 0; i < val.Len(); i++ {
							sliceInterface[i] = val.Index(i).Interface()
						}
						return s.convertKeyValueArrayToMap(sliceInterface)
					}
				}
			}

			// Check if first element is a struct with Key and Value fields (yaml.MapItem structure)
			if firstElem.Kind() == reflect.Struct {
				keyField := firstElem.FieldByName("Key")
				valueField := firstElem.FieldByName("Value")
				if keyField.IsValid() && valueField.IsValid() {
					// This looks like yaml.MapSlice - convert it
					result := make(map[string]interface{})
					for i := 0; i < val.Len(); i++ {
						item := val.Index(i)
						key := item.FieldByName("Key")
						value := item.FieldByName("Value")
						if key.IsValid() && value.IsValid() {
							keyInterface := key.Interface()
							valueInterface := value.Interface()
							// Convert key to string
							// In Kubernetes YAML, keys are always strings, but we handle edge cases
							var keyStr string
							if str, ok := keyInterface.(string); ok {
								keyStr = str
							} else if key.Kind() == reflect.String {
								keyStr = key.String()
							} else {
								// Fallback: convert any type to string (for edge cases)
								keyStr = fmt.Sprint(keyInterface)
							}
							// Recursively convert value
							result[keyStr] = s.convertMapSliceToMap(valueInterface)
						}
					}
					return result
				}
			}
		}
		// Regular slice - convert elements recursively
		result := make([]interface{}, val.Len())
		for i := 0; i < val.Len(); i++ {
			result[i] = s.convertMapSliceToMap(val.Index(i).Interface())
		}
		return result
	}

	// Handle map[string]interface{}
	if typ.Kind() == reflect.Map {
		if typ.Key().Kind() == reflect.String {
			result := make(map[string]interface{})
			for _, key := range val.MapKeys() {
				keyStr := key.String()
				result[keyStr] = s.convertMapSliceToMap(val.MapIndex(key).Interface())
			}
			return result
		}
		// map[interface{}]interface{} - convert to map[string]interface{}
		if typ.Key().Kind() == reflect.Interface {
			result := make(map[string]interface{})
			for _, key := range val.MapKeys() {
				keyInterface := key.Interface()
				// Convert key to string (with fallback for edge cases)
				var keyStr string
				if str, ok := keyInterface.(string); ok {
					keyStr = str
				} else {
					// Fallback: convert any type to string (for edge cases)
					keyStr = fmt.Sprint(keyInterface)
				}
				result[keyStr] = s.convertMapSliceToMap(val.MapIndex(key).Interface())
			}
			return result
		}
	}

	// Primitive types (string, int, bool, etc.) - return as-is
	return v
}

// calculateChecksum calculates SHA256 checksum of the archive
func (s *ArchiveService) calculateChecksum(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// GetArchiveInfo returns information about the archive without downloading it
func (s *ArchiveService) GetArchiveInfo(ctx context.Context, req *ArchiveRequest) (*ArchiveInfo, error) {
	// Get ManifestCheckpoint
	var checkpoint storagev1alpha1.ManifestCheckpoint
	if err := s.client.Get(ctx, types.NamespacedName{Name: req.CheckpointName}, &checkpoint); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("checkpoint %s not found", req.CheckpointName)
		}
		return nil, fmt.Errorf("failed to get checkpoint: %w", err)
	}

	// Validate SourceNamespace
	if checkpoint.Spec.SourceNamespace != req.SourceNamespace {
		return nil, fmt.Errorf("source namespace mismatch: checkpoint has %s, requested %s",
			checkpoint.Spec.SourceNamespace, req.SourceNamespace)
	}

	totalFiles := checkpoint.Status.TotalObjects
	estimatedSize := checkpoint.Status.TotalSizeBytes

	return &ArchiveInfo{
		CheckpointName:  req.CheckpointName,
		SourceNamespace: req.SourceNamespace,
		FileCount:       totalFiles,
		EstimatedSize:   estimatedSize,
	}, nil
}

// GetCacheKey generates a cache key based on checkpoint name and UID
// UID-based to detect checkpoint regeneration
func (s *ArchiveService) GetCacheKey(checkpointName, checkpointUID string) string {
	return fmt.Sprintf("%s:%s", checkpointName, checkpointUID)
}

// Cache methods
func (c *ArchiveCache) get(key string) *CacheItem {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, exists := c.items[key]
	if !exists {
		return nil
	}

	// Check TTL
	if time.Since(item.CreatedAt) > item.TTL {
		return nil
	}

	return item
}

func (c *ArchiveCache) set(key string, item *CacheItem) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[key] = item
}

// GetCacheItem returns a cache item by key (for external access)
func (s *ArchiveService) GetCacheItem(key string) *CacheItem {
	return s.cache.get(key)
}

// CleanupCache cleans up expired cache items
func (s *ArchiveService) CleanupCache() {
	s.cache.CleanupExpired()
}

// CleanupExpired removes expired items from cache
func (c *ArchiveCache) CleanupExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, item := range c.items {
		if now.Sub(item.CreatedAt) > item.TTL {
			delete(c.items, key)
		}
	}
}
