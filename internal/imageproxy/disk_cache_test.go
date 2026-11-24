package imageproxy

import (
	"strings"
	"testing"
)

func TestDiskCachePersistsAndEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	cache, err := newDiskCache(dir, 55)
	if err != nil {
		t.Fatal(err)
	}

	bodyA := []byte("image-a")
	bodyB := []byte("image-b")
	bodyC := []byte("image-c")
	keyA := strings.Repeat("a", 64)
	keyB := strings.Repeat("b", 64)
	keyC := strings.Repeat("c", 64)

	if err := cache.store(keyA, "image/jpeg", bodyA); err != nil {
		t.Fatal(err)
	}
	if got, contentType, ok := cache.load(keyA); !ok || contentType != "image/jpeg" || string(got) != string(bodyA) {
		t.Fatalf("first cache read = %q, %q, %t", got, contentType, ok)
	}
	if err := cache.store(keyB, "image/jpeg", bodyB); err != nil {
		t.Fatal(err)
	}

	// Touch A after B was written; C should evict B, not A.
	if _, _, ok := cache.load(keyA); !ok {
		t.Fatal("A disappeared before eviction")
	}
	if err := cache.store(keyC, "image/webp", bodyC); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.load(keyB); ok {
		t.Fatal("least-recently-used B was not evicted")
	}
	if _, _, ok := cache.load(keyA); !ok {
		t.Fatal("recently-used A was evicted")
	}
	if _, _, ok := cache.load(keyC); !ok {
		t.Fatal("new C entry was not stored")
	}

	reopened, err := newDiskCache(dir, 55)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.load(keyA); !ok {
		t.Fatal("A did not survive cache restart")
	}
	if _, contentType, ok := reopened.load(keyC); !ok || contentType != "image/webp" {
		t.Fatalf("C did not survive restart: content_type=%q ok=%t", contentType, ok)
	}
	if size, entries := reopened.stats(); size <= 0 || entries != 2 || size > 55 {
		t.Fatalf("disk stats = bytes:%d entries:%d", size, entries)
	}
}

func TestDiskCacheRejectsInvalidKeyAndContentType(t *testing.T) {
	cache, err := newDiskCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.store("../outside", "image/jpeg", []byte("image")); err == nil {
		t.Fatal("invalid cache key was accepted")
	}
	if err := cache.store(strings.Repeat("d", 64), "text/html", []byte("not-an-image")); err == nil {
		t.Fatal("non-image content type was accepted")
	}
}
