package staticcache

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type StaticCachedFile struct {
	FilePath    string // The file path of the cached file
	ContentType string // The MIME type of the cached file
	ExpiryTime  int64  // The Unix timestamp when the cache expires
}

type StaticCacheConfig struct {
	Enabled        bool     // Whether static caching is enabled on this proxy rule
	Timeout        int64    // How long to cache static files in seconds
	MaxFileSize    int64    // Maximum file size to cache in bytes
	FileExtensions []string // File extensions to cache, e.g. []string{".css", ".js", ".png"}
	SkipSubpaths   []string // Subpaths to skip caching, e.g. []string{"/api/", "/admin/"}
	CacheFileDir   string   // Directory to store cached files
}

type StaticCacheResourcesPool struct {
	config      *StaticCacheConfig
	cachedFiles sync.Map // in the type of map[string]*StaticCachedFile
}

func NewStaticCacheResourcesPool(config *StaticCacheConfig) *StaticCacheResourcesPool {
	//Check if the config dir exists, if not create it
	if config.CacheFileDir != "" {
		if _, err := os.Stat(config.CacheFileDir); os.IsNotExist(err) {
			os.MkdirAll(config.CacheFileDir, 0755)
		}
	}
	return &StaticCacheResourcesPool{
		config:      config,
		cachedFiles: sync.Map{},
	}
}

// GetDefaultStaticCacheConfig returns a default static cache configuration
func GetDefaultStaticCacheConfig(cacheFolderDir string) *StaticCacheConfig {
	return &StaticCacheConfig{
		Enabled:        false,
		Timeout:        3600,             // 1 hourt
		MaxFileSize:    25 * 1024 * 1024, // 25 MB
		FileExtensions: []string{".html", ".css", ".js", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".woff", ".woff2", ".ttf", ".eot"},
		SkipSubpaths:   []string{},
		CacheFileDir:   cacheFolderDir,
	}
}

func (pool *StaticCacheResourcesPool) IsEnabled() bool {
	return pool.config.Enabled
}

func (pool *StaticCacheResourcesPool) GetConfig() *StaticCacheConfig {
	return pool.config
}

// CheckFileSizeShouldBeCached checks if the file size is within the limit to be cached
func (pool *StaticCacheResourcesPool) CheckFileSizeShouldBeCached(contentLength int64) bool {
	return pool.config.MaxFileSize <= 0 || contentLength <= pool.config.MaxFileSize
}

// ShouldCacheRequest checks if a request should be cached based on the configuration
func (pool *StaticCacheResourcesPool) ShouldCacheRequest(requestPath string) bool {
	if !pool.config.Enabled {
		return false
	}

	// Check if path should be skipped
	for _, skipPath := range pool.config.SkipSubpaths {
		if strings.Contains(requestPath, skipPath) {
			return false
		}
	}

	ext := strings.ToLower(filepath.Ext(requestPath))
	found := false
	for _, allowedExt := range pool.config.FileExtensions {
		if ext == strings.ToLower(allowedExt) {
			found = true
			break
		}
	}
	return found
}

// GetCachedFile retrieves a cached file if it exists and is not expired
func (pool *StaticCacheResourcesPool) GetCachedFile(requestPath string) (*StaticCachedFile, bool) {
	cacheKey := pool.generateCacheKey(requestPath)

	value, exists := pool.cachedFiles.Load(cacheKey)

	if !exists {
		return nil, false
	}

	cachedFile, ok := value.(*StaticCachedFile)

	if !ok {
		return nil, false
	}

	// Check if cache is expired
	if time.Now().Unix() > cachedFile.ExpiryTime {
		// Remove expired cache
		pool.cachedFiles.Delete(cacheKey)
		pool.removeFileFromDisk(cachedFile.FilePath)
		return nil, false
	}

	return cachedFile, true
}

// generateCacheKey creates a unique key for caching based on the request path
func (pool *StaticCacheResourcesPool) generateCacheKey(requestPath string) string {
	// Use the request path as the key, normalized
	return strings.TrimPrefix(requestPath, "/")
}

// removeFileFromDisk removes a cached file from disk
func (pool *StaticCacheResourcesPool) removeFileFromDisk(filePath string) {
	os.Remove(filePath)
}

// StoreCachedFile stores a file in the cache with the given content and expiry time
func (pool *StaticCacheResourcesPool) StoreCachedFile(requestPath, contentType string, content []byte) error {
	cacheKey := pool.generateCacheKey(requestPath)

	// Create cache directory if it doesn't exist
	cacheDir := pool.config.CacheFileDir
	if cacheDir == "" {
		cacheDir = "./cache" // Default cache directory
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}

	// Generate file path
	fileName := strings.ReplaceAll(cacheKey, "/", "_")
	filePath := filepath.Join(cacheDir, fileName)

	// Write content to file
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return err
	}

	// Calculate expiry time
	expiryTime := time.Now().Add(time.Duration(pool.config.Timeout) * time.Second).Unix()

	// Store in memory cache
	cachedFile := &StaticCachedFile{
		FilePath:    filePath,
		ContentType: contentType,
		ExpiryTime:  expiryTime,
	}

	pool.cachedFiles.Store(cacheKey, cachedFile)
	return nil
}

// ServeCachedFile serves a cached file to the HTTP response writer
func (pool *StaticCacheResourcesPool) ServeCachedFile(w http.ResponseWriter, cachedFile *StaticCachedFile) error {
	file, err := os.Open(cachedFile.FilePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Set content type
	if cachedFile.ContentType != "" {
		w.Header().Set("Content-Type", cachedFile.ContentType)
	} else {
		// Try to detect content type from file extension
		ext := filepath.Ext(cachedFile.FilePath)
		if mimeType := mime.TypeByExtension(ext); mimeType != "" {
			w.Header().Set("Content-Type", mimeType)
		}
	}

	// Set cache headers
	w.Header().Set("Cache-Control", "public, max-age=3600") // Cache for 1 hour in browser

	// Copy file content to response
	_, err = io.Copy(w, file)
	return err
}

// RemoveExpiredCache removes all expired cached files from memory and disk
func (pool *StaticCacheResourcesPool) RemoveExpiredCache() {
	currentTime := time.Now().Unix()

	pool.cachedFiles.Range(func(key, value interface{}) bool {
		cachedFile, ok := value.(*StaticCachedFile)
		if !ok {
			return true
		}

		if currentTime > cachedFile.ExpiryTime {
			// Remove from memory
			pool.cachedFiles.Delete(key)
			// Remove from disk
			pool.removeFileFromDisk(cachedFile.FilePath)
		}

		return true
	})
}
