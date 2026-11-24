package api

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type RuntimeSettings struct {
	PreheatEnabled         bool "json:\"preheat_enabled\""
	PreheatOnStartup       bool "json:\"preheat_on_startup\""
	PreheatHourly          bool "json:\"preheat_hourly\""
	PreheatItems           int  "json:\"preheat_items\""
	IdleSeconds            int  "json:\"idle_seconds\""
	ImageRAMMiB            int  "json:\"image_ram_mib\""
	ImageDiskGiB           int  "json:\"image_disk_gib\""
	ImageRAMIdleHours      int  "json:\"image_ram_idle_hours\""
	ImageDiskRetentionDays int  "json:\"image_disk_retention_days\""
	APIRAMMiB              int  "json:\"api_ram_mib\""
	APIMaxEntries          int  "json:\"api_max_entries\""
}

type settingsStore struct {
	mu     sync.RWMutex
	path   string
	value  RuntimeSettings
	logger *log.Logger
}

func defaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		PreheatEnabled: true, PreheatOnStartup: true, PreheatHourly: true,
		PreheatItems: 100, IdleSeconds: 180,
		ImageRAMMiB: 224, ImageDiskGiB: 4,
		ImageRAMIdleHours: 168, ImageDiskRetentionDays: 90,
		APIRAMMiB: 64, APIMaxEntries: 4096,
	}
}

func newSettingsStore(logger *log.Logger) *settingsStore {
	path := os.Getenv("BEIXIAOGUO_SETTINGS_PATH")
	if path == "" {
		path = "/opt/beixiaoguo/settings.json"
	}
	store := &settingsStore{path: path, value: defaultRuntimeSettings(), logger: logger}
	data, err := os.ReadFile(path)
	if err == nil {
		var loaded RuntimeSettings
		if json.Unmarshal(data, &loaded) == nil {
			store.value = normalizeRuntimeSettings(loaded)
		} else if logger != nil {
			logger.Printf("settings file is invalid; defaults loaded")
		}
	}
	return store
}

func normalizeRuntimeSettings(value RuntimeSettings) RuntimeSettings {
	defaults := defaultRuntimeSettings()
	clamp := func(v, minimum, maximum, fallback int) int {
		if v == 0 {
			return fallback
		}
		if v < minimum {
			return minimum
		}
		if v > maximum {
			return maximum
		}
		return v
	}
	value.PreheatItems = clamp(value.PreheatItems, 20, 300, defaults.PreheatItems)
	value.IdleSeconds = clamp(value.IdleSeconds, 60, 600, defaults.IdleSeconds)
	value.ImageRAMMiB = clamp(value.ImageRAMMiB, 64, 512, defaults.ImageRAMMiB)
	value.ImageDiskGiB = clamp(value.ImageDiskGiB, 1, 8, defaults.ImageDiskGiB)
	value.ImageRAMIdleHours = clamp(value.ImageRAMIdleHours, 1, 720, defaults.ImageRAMIdleHours)
	value.ImageDiskRetentionDays = clamp(value.ImageDiskRetentionDays, 7, 365, defaults.ImageDiskRetentionDays)
	value.APIRAMMiB = clamp(value.APIRAMMiB, 16, 256, defaults.APIRAMMiB)
	value.APIMaxEntries = clamp(value.APIMaxEntries, 256, 8192, defaults.APIMaxEntries)
	return value
}

func (s *settingsStore) Get() RuntimeSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

func (s *settingsStore) Update(value RuntimeSettings) (RuntimeSettings, error) {
	value = normalizeRuntimeSettings(value)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return RuntimeSettings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return RuntimeSettings{}, fmt.Errorf("create settings directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*.tmp")
	if err != nil {
		return RuntimeSettings{}, fmt.Errorf("create settings temporary file: %w", err)
	}
	tempName := temp.Name()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return RuntimeSettings{}, err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return RuntimeSettings{}, err
	}
	if _, err := temp.Write([]byte("\n")); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return RuntimeSettings{}, err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return RuntimeSettings{}, err
	}
	if err := os.Rename(tempName, s.path); err != nil {
		_ = os.Remove(tempName)
		return RuntimeSettings{}, fmt.Errorf("replace settings file: %w", err)
	}
	s.value = value
	return value, nil
}

func (s *Server) applySettings() {
	cfg := s.settings.Get()
	if s.media != nil {
		s.media.SetIdleGrace(time.Duration(cfg.IdleSeconds) * time.Second)
	}
	if s.images != nil {
		s.images.Configure(
			int64(cfg.ImageRAMMiB)<<20,
			time.Duration(cfg.ImageRAMIdleHours)*time.Hour,
			int64(cfg.ImageDiskGiB)<<30,
			time.Duration(cfg.ImageDiskRetentionDays)*24*time.Hour,
		)
	}
	maxBytes := cfg.APIRAMMiB << 20
	maxEntries := cfg.APIMaxEntries
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for key, item := range s.apiCache {
		if time.Now().After(item.expiresAt) {
			s.cacheBytes -= len(item.data)
			delete(s.apiCache, key)
		}
	}
	for len(s.apiCache) > maxEntries || s.cacheBytes > maxBytes {
		oldestKey := ""
		var oldest time.Time
		for key, item := range s.apiCache {
			if oldestKey == "" || item.createdAt.Before(oldest) {
				oldestKey, oldest = key, item.createdAt
			}
		}
		if oldestKey == "" {
			break
		}
		s.cacheBytes -= len(s.apiCache[oldestKey].data)
		delete(s.apiCache, oldestKey)
	}
}
