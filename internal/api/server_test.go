package api

import (
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
	if cacheTTL("discover")!=3*time.Minute {t.Fatal("discover TTL changed")}
	if cacheTTL("series")!=20*time.Minute {t.Fatal("series TTL changed")}
	if cacheTTL("play")!=0 {t.Fatal("playback must not be cached")}
}
