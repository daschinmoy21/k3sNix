package lrscheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// RegistryCache stores container image layer metadata in memory, indexed by image reference.
type RegistryCache struct {
	mu     sync.RWMutex
	images map[string]ImageMetadata
}

// NewRegistryCache creates an empty RegistryCache.
func NewRegistryCache() *RegistryCache {
	return &RegistryCache{
		images: make(map[string]ImageMetadata),
	}
}

// Put stores metadata for an image reference (e.g. "nginx:latest" or "myimage:v1").
func (c *RegistryCache) Put(ref string, meta ImageMetadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.images[ref] = meta
	// Also store by ID or short name if applicable
	if meta.Name != "" && meta.Tag != "" {
		c.images[meta.Name+":"+meta.Tag] = meta
	}
}

// Get retrieves metadata for an image reference.
func (c *RegistryCache) Get(ref string) (ImageMetadata, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta, ok := c.images[ref]
	if ok {
		return meta, true
	}
	// Fall back to matching without registry prefix or tag
	for k, v := range c.images {
		if strings.HasSuffix(ref, k) || strings.HasSuffix(k, ref) {
			return v, true
		}
	}
	return ImageMetadata{}, false
}

// Count returns the number of images currently indexed.
func (c *RegistryCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.images)
}

// LoadFromFile loads the cache.json file described in Listing 1 of the paper.
func (c *RegistryCache) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var lists ImageMetadataLists
	if err := json.Unmarshal(data, &lists); err == nil && len(lists.Lists) > 0 {
		c.mu.Lock()
		defer c.mu.Unlock()
		for k, v := range lists.Lists {
			c.images[k] = v
		}
		return nil
	}
	// Also support direct map[string]ImageMetadata
	var direct map[string]ImageMetadata
	if err := json.Unmarshal(data, &direct); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range direct {
		c.images[k] = v
	}
	return nil
}

// SaveToFile writes the cache.json format to disk.
func (c *RegistryCache) SaveToFile(path string) error {
	c.mu.RLock()
	copyMap := make(map[string]ImageMetadata, len(c.images))
	for k, v := range c.images {
		copyMap[k] = v
	}
	c.mu.RUnlock()

	lists := ImageMetadataLists{
		CacheFile: path,
		Lists:     copyMap,
	}
	data, err := json.MarshalIndent(lists, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// CalculateNodeCachedBytes calculates D_c^n(t) from Eq. (2):
// the sum of layer sizes for all layers of the container that are already present on the node.
func (c *RegistryCache) CalculateNodeCachedBytes(imageRef string, nodeLayers []string) (cachedBytes int64, totalBytes int64, err error) {
	meta, ok := c.Get(imageRef)
	if !ok {
		return 0, 0, fmt.Errorf("image metadata not found for %q", imageRef)
	}

	total := meta.TotalSize
	if total == 0 {
		for _, l := range meta.Layers {
			total += l.Size
		}
	}

	nodeSet := make(map[string]bool, len(nodeLayers))
	for _, l := range nodeLayers {
		nodeSet[l] = true
	}

	var cached int64
	for _, l := range meta.Layers {
		if nodeSet[l.Layer] {
			cached += l.Size
		}
	}

	return cached, total, nil
}

// RegistryWatcher periodically polls a Docker/OCI registry v2 catalog
// every 10 seconds as specified in Section V-1 of the paper.
type RegistryWatcher struct {
	registryURL  string
	pollInterval time.Duration
	cache        *RegistryCache
	client       *http.Client
	stopCh       chan struct{}
	running      bool
	mu           sync.Mutex
}

// NewRegistryWatcher creates a watcher polling registryURL.
func NewRegistryWatcher(registryURL string, pollInterval time.Duration, cache *RegistryCache) *RegistryWatcher {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	return &RegistryWatcher{
		registryURL:  strings.TrimRight(registryURL, "/"),
		pollInterval: pollInterval,
		cache:        cache,
		client:       &http.Client{Timeout: 10 * time.Second},
		stopCh:       make(chan struct{}),
	}
}

// Start begins background polling on a ticker.
func (w *RegistryWatcher) Start(ctx context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.stopCh = make(chan struct{})
	w.mu.Unlock()

	go func() {
		// Initial fetch
		_ = w.PollOnce(ctx)

		ticker := time.NewTicker(w.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			case <-ticker.C:
				_ = w.PollOnce(ctx)
			}
		}
	}()
}

// Stop terminates background polling.
func (w *RegistryWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		close(w.stopCh)
		w.running = false
	}
}

// CatalogResponse mirrors the OCI/Docker registry /v2/_catalog response.
type CatalogResponse struct {
	Repositories []string `json:"repositories"`
}

// TagListResponse mirrors the OCI/Docker registry /v2/<name>/tags/list response.
type TagListResponse struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// ManifestV2 mirrors the Docker image manifest schema 2.
type ManifestV2 struct {
	SchemaVersion int `json:"schemaVersion"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
	} `json:"layers"`
}

// PollOnce executes a single polling cycle querying /v2/_catalog and image manifests.
func (w *RegistryWatcher) PollOnce(ctx context.Context) error {
	if w.registryURL == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.registryURL+"/v2/_catalog", nil)
	if err != nil {
		return err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry /v2/_catalog returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var catalog CatalogResponse
	if err := json.Unmarshal(body, &catalog); err != nil {
		return err
	}

	for _, repo := range catalog.Repositories {
		tags, err := w.fetchTags(ctx, repo)
		if err != nil {
			continue
		}
		for _, tag := range tags {
			meta, err := w.fetchManifest(ctx, repo, tag)
			if err != nil {
				continue
			}
			w.cache.Put(repo+":"+tag, meta)
		}
	}
	return nil
}

func (w *RegistryWatcher) fetchTags(ctx context.Context, repo string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/tags/list", w.registryURL, repo), nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tags list for %s returned status %d", repo, resp.StatusCode)
	}

	var tagList TagListResponse
	if err := json.NewDecoder(resp.Body).Decode(&tagList); err != nil {
		return nil, err
	}
	return tagList.Tags, nil
}

func (w *RegistryWatcher) fetchManifest(ctx context.Context, repo, tag string) (ImageMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/manifests/%s", w.registryURL, repo, tag), nil)
	if err != nil {
		return ImageMetadata{}, err
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")

	resp, err := w.client.Do(req)
	if err != nil {
		return ImageMetadata{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ImageMetadata{}, fmt.Errorf("manifest for %s:%s returned status %d", repo, tag, resp.StatusCode)
	}

	var m ManifestV2
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return ImageMetadata{}, err
	}

	var totalSize int64
	layers := make([]LayerMetadata, len(m.Layers))
	for i, l := range m.Layers {
		layers[i] = LayerMetadata{
			Size:  l.Size,
			Layer: l.Digest,
		}
		totalSize += l.Size
	}

	return ImageMetadata{
		Name:      repo,
		Tag:       tag,
		TotalSize: totalSize,
		Layers:    layers,
	}, nil
}
