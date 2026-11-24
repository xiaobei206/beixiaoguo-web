package api

import (
	"path/filepath"
	"testing"
)

func TestSettingsStorePersistsAndClampsValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BEIXIAOGUO_SETTINGS_PATH", path)

	store := newSettingsStore(nil)
	defaults := store.Get()
	if !defaults.PreheatEnabled || !defaults.PreheatOnStartup || !defaults.PreheatHourly {
		t.Fatal("preheat defaults are not enabled")
	}
	if defaults.PreheatItems != 100 || defaults.ImageDiskGiB != 4 || defaults.APIMaxEntries != 4096 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	next := defaults
	next.PreheatEnabled = false
	next.PreheatItems = 999
	next.ImageDiskGiB = 99
	next.APIMaxEntries = 9000
	next.ImageRAMIdleHours = 0
	if _, err := store.Update(next); err != nil {
		t.Fatal(err)
	}

	reloaded := newSettingsStore(nil).Get()
	if reloaded.PreheatEnabled {
		t.Fatal("disabled preheat flag was not persisted")
	}
	if reloaded.PreheatItems != 300 || reloaded.ImageDiskGiB != 8 || reloaded.APIMaxEntries != 8192 {
		t.Fatalf("values were not clamped: %+v", reloaded)
	}
	if reloaded.ImageRAMIdleHours != 168 {
		t.Fatalf("zero idle hours did not use default: %d", reloaded.ImageRAMIdleHours)
	}
}
