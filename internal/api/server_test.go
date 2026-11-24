package api

import (
	"context"
	"testing"
	"time"
)

func TestAPICacheStoresExpiresAndEvicts(t *testing.T) {
	s:=&Server{apiCache:make(map[string]cacheEntry)}
	s.storeCached("one",[]byte("first"),time.Minute)
	if got,ok:=s.getCached("one");!ok || string(got)!="first" {t.Fatalf("cache hit=%v value=%q",ok,got)}
	s.cacheMu.Lock()
	item:=s.apiCache["one"]
	item.expiresAt=time.Now().Add(-time.Second)
	s.apiCache["one"]=item
	s.cacheMu.Unlock()
	if _,ok:=s.getCached("one");ok {t.Fatal("expired cache entry was served")}
	if len(s.apiCache)!=0 {t.Fatal("expired entry was not removed")}
}

func TestCacheTTLByOperation(t *testing.T) {
	if cacheTTL("discover")!=10*time.Minute {t.Fatal("discover TTL changed")}
	if cacheTTL("search")!=5*time.Minute {t.Fatal("search TTL changed")}
	if cacheTTL("series")!=30*time.Minute {t.Fatal("series TTL changed")}
	if cacheTTL("seasons")!=30*time.Minute {t.Fatal("seasons TTL changed")}
	if cacheTTL("episodes")!=20*time.Minute {t.Fatal("episodes TTL changed")}
	if cacheTTL("play")!=0 {t.Fatal("playback must not be cached")}
}


func TestSearchFiltersForType(t *testing.T) {
	tests := []struct {
		kind string
		want string
		bad  bool
	}{
		{kind: "", want: ""},
		{kind: "all", want: ""},
		{kind: "short", want: "content_type_short_play"},
		{kind: "comic", want: "content_type_dynamic_comic"},
		{kind: "ai", want: "content_type_ai_dynamic_comic"},
		{kind: "film", want: "content_type_long_video"},
		{kind: "unwatched", want: "search_range_unread"},
		{kind: "unknown", bad: true},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			got, err := searchFiltersForType(test.kind)
			if test.bad {
				if err == nil {
					t.Fatal("expected unsupported filter to fail")
				}
				return
			}
			if err != nil {
				t.Fatalf("searchFiltersForType returned error: %v", err)
			}
			if test.want == "" {
				if len(got) != 0 {
					t.Fatalf("got %v, want no filters", got)
				}
				return
			}
			if len(got) != 1 || got[0] != test.want {
				t.Fatalf("got %v, want [%s]", got, test.want)
			}
		})
	}
}


func TestAcquireRequestSlotHonorsDeadline(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	started := time.Now()
	if acquireRequestSlot(ctx, slots) {
		t.Fatal("acquired a busy slot after the context deadline")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("slot wait exceeded context deadline: %s", elapsed)
	}
	if len(slots) != 1 {
		t.Fatal("timed-out waiter modified the occupied slot")
	}
}

func TestAcquireRequestSlotSucceedsImmediately(t *testing.T) {
	slots := make(chan struct{}, 1)
	if !acquireRequestSlot(context.Background(), slots) {
		t.Fatal("available slot was not acquired")
	}
	if len(slots) != 1 {
		t.Fatal("slot token was not recorded")
	}
}

func TestAcquireRequestQueueIsBounded(t *testing.T) {
	queue := make(chan struct{}, 1)
	if !acquireRequestQueue(context.Background(), queue) {
		t.Fatal("first request was not admitted")
	}
	if acquireRequestQueue(context.Background(), queue) {
		t.Fatal("queue admitted a request beyond its capacity")
	}
	if len(queue) != 1 {
		t.Fatalf("queued=%d, want 1", len(queue))
	}
	<-queue
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if acquireRequestQueue(ctx, queue) {
		t.Fatal("cancelled request entered the queue")
	}
}
