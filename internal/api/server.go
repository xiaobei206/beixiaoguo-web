package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beilunyang/openhongguo/sdk"

	"github.com/xiaobei206/beixiaoguo-web/internal/imageproxy"
	"github.com/xiaobei206/beixiaoguo-web/internal/media"
	"github.com/xiaobei206/beixiaoguo-web/internal/tv"
	"github.com/xiaobei206/beixiaoguo-web/internal/webui"
)

const (
	apiCacheMaxEntries = 192
	apiCacheMaxBytes   = 16 << 20
)

type cacheEntry struct {
	data      []byte
	expiresAt time.Time
	createdAt time.Time
}

type Server struct {
	client  *sdk.Client
	media   *media.Service
	images  *imageproxy.Proxy
	tv      *tv.Service
	version string
	logger  *log.Logger
	slots   chan struct{}
	seq     atomic.Uint64

	cacheMu    sync.Mutex
	apiCache   map[string]cacheEntry
	cacheBytes int
}

type contextKey uint8

const requestIDKey contextKey = 1

func New(client *sdk.Client, version string, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		client:    client,
		media:     media.New(client, logger),
		images:    imageproxy.New(logger),
		tv:        tv.New(logger),
		version:   version,
		logger:    logger,
		slots:     make(chan struct{}, 4),
		apiCache:  make(map[string]cacheEntry),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/discover", s.discover)
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/suggest", s.suggest)
	mux.HandleFunc("GET /api/v1/hot-search", s.hotSearch)
	mux.HandleFunc("GET /api/v1/new-releases", s.newReleases)
	mux.HandleFunc("GET /api/v1/rank", s.shortRank)
	mux.HandleFunc("GET /api/v1/series/{id}", s.series)
	mux.HandleFunc("GET /api/v1/series/{id}/episodes", s.episodes)
	mux.HandleFunc("GET /api/v1/series/{id}/seasons", s.seasons)
	mux.HandleFunc("GET /api/v1/play/{episodeID}", s.play)
	mux.Handle("GET /api/v1/media/{token}", s.media)
	mux.Handle("GET /api/v1/images/{token}", s.images)
	mux.HandleFunc("GET /api/v1/tv/devices", s.tvDevices)
	mux.HandleFunc("POST /api/v1/tv/handoff", s.tvHandoff)
	mux.Handle("/", webui.Handler())

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("bxg-%08d", s.seq.Add(1))
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; media-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")

		started := time.Now()
		next.ServeHTTP(w, r)
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/api/v1/media/"):
			path = "/api/v1/media/:token"
		case strings.HasPrefix(path, "/api/v1/images/"):
			path = "/api/v1/images/:token"
		}
		s.logger.Printf("http request_id=%s method=%s path=%q duration_ms=%d", id, r.Method, path, time.Since(started).Milliseconds())
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": "贝小果", "version": s.version, "runtime": "go",
	})
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.run(w, r, "discover", func(ctx context.Context) (any, error) {
		return s.client.Discover(ctx, sdk.DiscoverOptions{
			Channel: strings.TrimSpace(q.Get("channel")),
			Cursor:  strings.TrimSpace(q.Get("cursor")),
		})
	})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	filterType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if q == "" || len([]rune(q)) > 100 {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "请输入 1 到 100 个字符的搜索词", "")
		return
	}
	filters, err := searchFiltersForType(filterType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_FILTER", err.Error(), "")
		return
	}
	s.run(w, r, "search", func(ctx context.Context) (any, error) {
		return s.client.Search(ctx, q, sdk.SearchOptions{
			Cursor: cursor, PageSize: 20, Filters: filters,
		})
	})
}

func searchFiltersForType(kind string) ([]string, error) {
	switch kind {
	case "", "all":
		return nil, nil
	case "short":
		return []string{sdk.SearchFilterShortPlay}, nil
	case "comic":
		return []string{sdk.SearchFilterComic}, nil
	case "ai":
		return []string{sdk.SearchFilterAIComic}, nil
	case "film":
		return []string{sdk.SearchFilterFilm}, nil
	case "unwatched":
		return []string{sdk.SearchFilterUnwatched}, nil
	default:
		return nil, fmt.Errorf("不支持的搜索筛选：%s", kind)
	}
}

func (s *Server) suggest(w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSpace(r.URL.Query().Get("q"))
	if prefix == "" || len([]rune(prefix)) > 60 {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "请输入搜索前缀", "")
		return
	}
	s.run(w, r, "suggest", func(ctx context.Context) (any, error) {
		return s.client.Suggest(ctx, prefix)
	})
}

func (s *Server) hotSearch(w http.ResponseWriter, r *http.Request) {
	s.run(w, r, "hot-search", func(ctx context.Context) (any, error) {
		return s.client.HotSearch(ctx)
	})
}

func (s *Server) newReleases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.run(w, r, "new-releases", func(ctx context.Context) (any, error) {
		return s.client.NewReleases(ctx, sdk.NewReleaseQuery{
			Date:   strings.TrimSpace(q.Get("date")),
			Cursor: strings.TrimSpace(q.Get("cursor")),
		})
	})
}

func (s *Server) shortRank(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := sdk.ShortRankQuery{
		Content: strings.TrimSpace(q.Get("content")),
		Board:   strings.TrimSpace(q.Get("board")),
		Cursor:  strings.TrimSpace(q.Get("cursor")),
	}
	s.run(w, r, "short-rank", func(ctx context.Context) (any, error) {
		return s.client.ShortRank(ctx, query)
	})
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "缺少短剧 ID", "")
		return
	}
	s.run(w, r, "series", func(ctx context.Context) (any, error) {
		return s.client.GetSeries(ctx, id)
	})
}

func (s *Server) episodes(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "缺少短剧 ID", "")
		return
	}
	s.run(w, r, "episodes", func(ctx context.Context) (any, error) {
		return s.client.ListEpisodes(ctx, id, sdk.EpisodeOptions{})
	})
}

func (s *Server) seasons(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "缺少短剧 ID", "")
		return
	}
	s.run(w, r, "seasons", func(ctx context.Context) (any, error) {
		return s.client.Seasons(ctx, id)
	})
}

func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	episodeID := strings.TrimSpace(r.PathValue("episodeID"))
	quality := strings.TrimSpace(r.URL.Query().Get("quality"))
	if episodeID == "" || len(episodeID) > 128 {
		writeError(w, http.StatusBadRequest, "INVALID_EPISODE", "缺少有效的分集 ID", "")
		return
	}
	s.run(w, r, "play", func(ctx context.Context) (any, error) {
		return s.media.Prepare(ctx, episodeID, quality)
	})
}

func (s *Server) tvDevices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	devices, err := s.tv.Discover(ctx, 3200*time.Millisecond)
	if err != nil {
		id, _ := r.Context().Value(requestIDKey).(string)
		s.logger.Printf("tv discovery request_id=%s error=%v",id,err)
		writeError(w, http.StatusServiceUnavailable, "TV_DISCOVERY_FAILED", "N1 局域网电视搜索失败："+err.Error(), id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) tvHandoff(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request tv.HandoffRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_HANDOFF", "投屏参数格式无效", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 42*time.Second)
	defer cancel()
	result, err := s.tv.Handoff(ctx, request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "TV_HANDOFF_FAILED", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request, operation string, call func(context.Context) (any, error)) {
	cacheKey := operation + ":" + r.URL.RequestURI()
	if ttl := cacheTTL(operation); ttl > 0 {
		if cached, ok := s.getCached(cacheKey); ok {
			s.writePayload(w, cached)
			return
		}
	}

	select {
	case s.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.slots }()

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	result, err := call(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		id, _ := r.Context().Value(requestIDKey).(string)
		status, code, message := http.StatusBadGateway, "UPSTREAM_ERROR", "片库请求失败，请稍后重试"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status, code, message = http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "片库响应超时，请稍后重试"
		case errors.Is(err, media.ErrUnsupportedStream):
			status, code, message = http.StatusNotImplemented, "UNSUPPORTED_STREAM", "当前仅支持单文件 MP4；分段视频暂不支持"
		case strings.Contains(err.Error(), "requested quality"):
			status, code, message = http.StatusBadRequest, "QUALITY_UNAVAILABLE", "该清晰度暂不可用"
		}
		s.logger.Printf("upstream request_id=%s operation=%s error_type=%T error=%v", id, operation, err, err)
		writeError(w, status, code, message, id)
		return
	}

	raw, err := json.Marshal(result)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENCODE_FAILED", "片库响应无法编码", "")
		return
	}
	if ttl := cacheTTL(operation); ttl > 0 {
		s.storeCached(cacheKey, raw, ttl)
	}
	s.writePayload(w, raw)
}

func cacheTTL(operation string) time.Duration {
	switch operation {
	case "discover", "rank", "short-rank", "new-releases":
		return 3 * time.Minute
	case "search":
		return 90 * time.Second
	case "hot-search":
		return 60 * time.Second
	case "series", "seasons":
		return 20 * time.Minute
	case "episodes":
		return 10 * time.Minute
	case "suggest":
		return 30 * time.Second
	default:
		return 0
	}
}

func (s *Server) getCached(key string) ([]byte, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	item, ok := s.apiCache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(item.expiresAt) {
		s.cacheBytes -= len(item.data)
		delete(s.apiCache, key)
		return nil, false
	}
	return item.data, true
}

func (s *Server) storeCached(key string, data []byte, ttl time.Duration) {
	if len(data) == 0 || len(data) > apiCacheMaxBytes/2 {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	now := time.Now()
	for existingKey, item := range s.apiCache {
		if now.After(item.expiresAt) {
			s.cacheBytes -= len(item.data)
			delete(s.apiCache, existingKey)
		}
	}
	if previous, ok := s.apiCache[key]; ok {
		s.cacheBytes -= len(previous.data)
		delete(s.apiCache, key)
	}
	for len(s.apiCache) >= apiCacheMaxEntries || s.cacheBytes+len(data) > apiCacheMaxBytes {
		var oldestKey string
		var oldest time.Time
		for existingKey, item := range s.apiCache {
			if oldestKey == "" || item.createdAt.Before(oldest) {
				oldestKey, oldest = existingKey, item.createdAt
			}
		}
		if oldestKey == "" {
			break
		}
		s.cacheBytes -= len(s.apiCache[oldestKey].data)
		delete(s.apiCache, oldestKey)
	}
	copyOfData := append([]byte(nil), data...)
	s.apiCache[key] = cacheEntry{data: copyOfData, expiresAt: now.Add(ttl), createdAt: now}
	s.cacheBytes += len(copyOfData)
}

func (s *Server) writePayload(w http.ResponseWriter, raw []byte) {
	if s.images != nil {
		if rewritten, err := s.images.RewriteJSON(raw); err == nil {
			raw = rewritten
		} else {
			s.logger.Printf("image URL rewrite failed: %T", err)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code": code, "message": message, "request_id": requestID,
		},
	})
}

