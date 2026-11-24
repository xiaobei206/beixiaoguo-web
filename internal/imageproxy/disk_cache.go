package imageproxy

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"sync"
	"time"
)

const (
	diskCacheMagic    = "BXGIMG1\n"
	diskCacheLifetime = 90 * 24 * time.Hour
	minDiskFreeBytes  = int64(1 << 30)
)

type diskFile struct {
	path     string
	size     int64
	lastUsed time.Time
}

type diskCache struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	lifetime time.Duration
	minFree  int64
	bytes    int64
	entries  map[string]diskFile
}

func newDiskCache(dir string, maxBytes int64) (*diskCache, error) {
	if strings.TrimSpace(dir) == "" || maxBytes <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("image cache path is not a directory")
	}

	cache := &diskCache{
		dir:      dir,
		maxBytes: maxBytes,
		lifetime: diskCacheLifetime,
		minFree:  minDiskFreeBytes,
		entries:  make(map[string]diskFile),
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, file := range files {
		name := file.Name()
		if strings.HasPrefix(name, ".image-") && strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, ".img") {
			continue
		}
		key := strings.TrimSuffix(name, ".img")
		if !validDiskKey(key) || file.IsDir() {
			continue
		}
		fileInfo, statErr := file.Info()
		if statErr != nil || !fileInfo.Mode().IsRegular() {
			continue
		}
		cache.entries[key] = diskFile{
			path:     filepath.Join(dir, name),
			size:     fileInfo.Size(),
			lastUsed: fileInfo.ModTime(),
		}
		cache.bytes += fileInfo.Size()
	}
	cache.evictLocked("")
	return cache, nil
}

func validDiskKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, char := range key {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func (c *diskCache) load(key string) ([]byte, string, bool) {
	if c == nil || !validDiskKey(key) {
		return nil, "", false
	}

	c.mu.Lock()
	item, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return nil, "", false
	}
	if time.Since(item.lastUsed) > c.lifetime {
		c.removeLocked(key)
		c.mu.Unlock()
		return nil, "", false
	}
	item.lastUsed = time.Now()
	c.entries[key] = item
	c.mu.Unlock()

	raw, err := os.ReadFile(item.path)
	if err != nil || !bytes.HasPrefix(raw, []byte(diskCacheMagic)) {
		c.remove(key)
		return nil, "", false
	}
	header := raw[len(diskCacheMagic):]
	separator := bytes.IndexByte(header, '\n')
	if separator <= 0 || separator >= 128 {
		c.remove(key)
		return nil, "", false
	}
	contentType := strings.TrimSpace(string(header[:separator]))
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		c.remove(key)
		return nil, "", false
	}
	body := append([]byte(nil), header[separator+1:]...)
	if len(body) == 0 || len(body) > maxImageBytes {
		c.remove(key)
		return nil, "", false
	}
	return body, contentType, true
}

func (c *diskCache) store(key, contentType string, body []byte) error {
	if c == nil {
		return nil
	}
	if !validDiskKey(key) || len(body) == 0 || len(body) > maxImageBytes {
		return fmt.Errorf("invalid image cache entry")
	}
	contentType = strings.TrimSpace(contentType)
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") ||
		strings.ContainsAny(contentType, "\r\n") {
		return fmt.Errorf("invalid image content type")
	}

	var data bytes.Buffer
	data.WriteString(diskCacheMagic)
	data.WriteString(contentType)
	data.WriteByte('\n')
	data.Write(body)
	if int64(data.Len()) > c.maxBytes {
		return nil
	}

	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(c.dir, &filesystem); err != nil {
		return fmt.Errorf("cannot verify image cache free space: %w", err)
	}
	available := int64(filesystem.Bavail) * int64(filesystem.Bsize)
	if available-int64(data.Len()) < c.minFree {
		return fmt.Errorf("image disk cache is preserving the final 128 MiB of free space")
	}

	temp, err := os.CreateTemp(c.dir, ".image-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if _, err = temp.Write(data.Bytes()); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err = temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}

	target := filepath.Join(c.dir, key+".img")
	c.mu.Lock()
	defer c.mu.Unlock()

	oldSize := int64(0)
	if old, ok := c.entries[key]; ok {
		oldSize = old.size
	}
	for c.bytes-oldSize+int64(data.Len()) > c.maxBytes {
		victimKey := ""
		var oldest time.Time
		for candidate, item := range c.entries {
			if candidate == key {
				continue
			}
			if victimKey == "" || item.lastUsed.Before(oldest) {
				victimKey = candidate
				oldest = item.lastUsed
			}
		}
		if victimKey == "" {
			_ = os.Remove(tempName)
			return fmt.Errorf("image disk cache has no evictable space")
		}
		c.removeLocked(victimKey)
	}

	if err = os.Rename(tempName, target); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	c.bytes = c.bytes - oldSize + int64(data.Len())
	c.entries[key] = diskFile{
		path:     target,
		size:     int64(data.Len()),
		lastUsed: time.Now(),
	}
	return nil
}

func (c *diskCache) remove(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.removeLocked(key)
	c.mu.Unlock()
}

func (c *diskCache) removeLocked(key string) {
	item, ok := c.entries[key]
	if !ok {
		return
	}
	_ = os.Remove(item.path)
	c.bytes -= item.size
	if c.bytes < 0 {
		c.bytes = 0
	}
	delete(c.entries, key)
}

func (c *diskCache) evictLocked(keep string) {
	for c.bytes > c.maxBytes {
		victimKey := ""
		var oldest time.Time
		for key, item := range c.entries {
			if key == keep {
				continue
			}
			if victimKey == "" || item.lastUsed.Before(oldest) {
				victimKey = key
				oldest = item.lastUsed
			}
		}
		if victimKey == "" {
			return
		}
		c.removeLocked(victimKey)
	}
}

// SetLimits updates the persistent cache budget and retention policy at runtime.
func (c *diskCache) SetLimits(maxBytes int64, lifetime time.Duration) {
	if c == nil {
		return
	}
	if maxBytes < 1 {
		maxBytes = int64(4 << 30)
	}
	if lifetime < 24*time.Hour {
		lifetime = diskCacheLifetime
	}
	c.mu.Lock()
	c.maxBytes = maxBytes
	c.lifetime = lifetime
	now := time.Now()
	for key, item := range c.entries {
		if now.Sub(item.lastUsed) > c.lifetime {
			c.removeLocked(key)
		}
	}
	c.evictLocked("")
	c.mu.Unlock()
}

func (c *diskCache) limitBytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxBytes
}

func (c *diskCache) stats() (bytes int64, entries int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes, len(c.entries)
}
