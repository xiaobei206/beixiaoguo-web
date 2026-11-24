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
	apiCacheMaxEntries = 4096
	apiCacheMaxBytes   = 64 << 20
	apiRequestTimeout  = 25 * time.Second
	apiUpstreamSlots   = 4
	apiQueueCapacity   = 24
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
	queue   chan struct{}
	seq     atomic.Uint64

	cacheMu    sync.Mutex
	apiCache   map[string]cacheEntry
	cacheBytes int

	settings       *settingsStore
	preheatMu      sync.Mutex
	preheat         preheatStatus
	preheatTrigger chan struct{}
	preheatRateMu  sync.Mutex
	preheatLastCall time.Time
}

type contextKey uint8

const requestIDKey contextKey = 1

var errSeriesNotFound = errors.New("series not found")

func New(client *sdk.Client, version string, logger *log.Logger) http.Handler {
	return newHandler(client, version, logger, false)
}

// NewWithPreheater starts the idle preheater. Tests and embedded callers can use New to avoid background network work.
func NewWithPreheater(client *sdk.Client, version string, logger *log.Logger) http.Handler {
	return newHandler(client, version, logger, true)
}

func newHandler(client *sdk.Client, version string, logger *log.Logger, startPreheater bool) http.Handler {
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
		slots:     make(chan struct{}, apiUpstreamSlots),
		queue:     make(chan struct{}, apiQueueCapacity),
		apiCache:  make(map[string]cacheEntry),
		settings:  newSettingsStore(logger),
	}
	s.applySettings()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/settings", s.handleSettings)
	mux.HandleFunc("PUT /api/v1/settings", s.handleSettings)
	mux.HandleFunc("POST /api/v1/preheat/run", s.preheatRun)
	mux.HandleFunc("POST /api/v1/playback/activity", s.playbackActivity)
	mux.HandleFunc("GET /api/v1/discover", s.discover)
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/suggest", s.suggest)
	mux.HandleFunc("GET /api/v1/hot-search", s.hotSearch)
	mux.HandleFunc("GET /api/v1/new-releases", s.newReleases)
	mux.HandleFunc("GET /api/v1/rank", s.shortRank)
	mux.HandleFunc("GET /api/v1/series/{id}", s.series)
	mux.HandleFunc("GET /api/v1/series/{id}/detail", s.seriesDetail)
	mux.HandleFunc("GET /api/v1/series/{id}/episodes", s.episodes)
	mux.HandleFunc("GET /api/v1/series/{id}/seasons", s.seasons)
	mux.HandleFunc("GET /api/v1/play/{episodeID}", s.play)
	mux.Handle("GET /api/v1/media/{token}", s.media)
	mux.Handle("GET /api/v1/images/{token}", s.images)
	mux.HandleFunc("GET /api/v1/tv/devices", s.tvDevices)
	mux.HandleFunc("POST /api/v1/tv/handoff", s.tvHandoff)
	mux.Handle("/", webui.Handler())

	handler := s.middleware(mux)
	if startPreheater {
		s.startPreheater()
	}
	return handler
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

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, settingsResponse(s))
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var next RuntimeSettings
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&next); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_SETTINGS", "设置格式无效", "")
			return
		}
		updated, err := s.settings.Update(next)
		if err != nil {
			s.logger.Printf("settings save failed error=%v", err)
			writeError(w, http.StatusInternalServerError, "SETTINGS_SAVE_FAILED", "设置保存失败，请检查 /opt 空间", "")
			return
		}
		s.applySettings()
		if !updated.PreheatEnabled {
			s.updatePreheat(func(st *preheatStatus) { if st.Running { st.Phase = "正在停止" } })
		}
		writeJSON(w, http.StatusOK, settingsResponse(s))
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.cacheMu.Lock()
	apiEntries, apiBytes := len(s.apiCache), s.cacheBytes
	s.cacheMu.Unlock()

	imageStats := imageproxy.CacheStats{}
	if s.images != nil {
		imageStats = s.images.Stats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"name": "贝小果",
		"version": s.version,
		"runtime": "go",
		"cache": map[string]any{
			"api_entries": apiEntries,
			"api_bytes": apiBytes,
			"api_limit_bytes": s.settings.Get().APIRAMMiB << 20,
			"api_limit_entries": s.settings.Get().APIMaxEntries,
			"image_ram_entries": imageStats.RAMEntries,
			"image_ram_bytes": imageStats.RAMBytes,
			"image_ram_limit_bytes": imageStats.RAMLimitBytes,
			"image_disk_entries": imageStats.DiskEntries,
			"image_disk_bytes": imageStats.DiskBytes,
			"image_disk_limit_bytes": imageStats.DiskLimitBytes,
		},
		"upstream_queue": map[string]any{
			"active": len(s.slots),
			"queued": len(s.queue),
			"capacity": cap(s.queue),
		},
		"image_queue": map[string]any{
			"active": imageStats.FetchActive,
			"queued": imageStats.FetchQueued,
			"capacity": imageStats.FetchQueueCapacity,
		},
		"preheat": s.preheatSnapshot(),
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

func (s *Server) seriesDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "缺少短剧 ID", "")
		return
	}
	s.run(w, r, "series-detail", func(ctx context.Context) (any, error) {
		// GetSeries and ListEpisodes both call the same upstream seriesDetail API.
		// GetSeriesBatch returns the series and its episodes in one upstream request.
		details, err := s.client.GetSeriesBatch(ctx, []string{id})
		if err != nil {
			return nil, err
		}
		if len(details) == 0 || details[0].Series == nil {
			return nil, errSeriesNotFound
		}
		return map[string]any{
			"series":   details[0].Series,
			"episodes": details[0].Episodes,
		}, nil
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

func (s *Server) playbackActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	var request struct {
		ClientID string `json:"client_id"`
		Playing  bool   `json:"playing"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.ClientID) == "" || len(request.ClientID) > 96 {
		writeError(w, http.StatusBadRequest, "INVALID_PLAYBACK_ACTIVITY", "播放状态参数无效", "")
		return
	}
	s.media.SetPlaybackState(request.ClientID, request.Playing)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playing": request.Playing})
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

func acquireRequestQueue(ctx context.Context, queue chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case queue <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	default:
		return false
	}
}

func acquireRequestSlot(ctx context.Context, slots chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) run(w http.ResponseWriter, r *http.Request, operation string, call func(context.Context) (any, error)) {
	cacheKey := operation + ":" + r.URL.RequestURI()
	var stale []byte
	if ttl := cacheTTL(operation); ttl > 0 {
		if cached, ok := s.getCached(cacheKey); ok {
			s.writePayload(w, cached)
			return
		}
		stale, _ = s.getStaleCached(cacheKey)
	}

	ctx, cancel := context.WithTimeout(r.Context(), apiRequestTimeout)
	defer cancel()

	// Admit only a bounded number of waiters. Cancelled and obsolete browser
	// requests leave the queue immediately instead of accumulating goroutines.
	if !acquireRequestQueue(ctx, s.queue) {
		if r.Context().Err() != nil {
			return
		}
		id, _ := r.Context().Value(requestIDKey).(string)
		w.Header().Set("Retry-After", "2")
		s.logger.Printf("upstream queue full request_id=%s operation=%s", id, operation)
		writeError(w, http.StatusServiceUnavailable, "REQUEST_QUEUE_FULL", "片库请求较多，触发了保护限流，稍后会自动重试", id)
		return
	}

	// Include semaphore wait time in the request deadline. Queue admission is
	// released as soon as a worker slot is acquired.
	if !acquireRequestSlot(ctx, s.slots) {
		<-s.queue
		if r.Context().Err() != nil {
			return
		}
		id, _ := r.Context().Value(requestIDKey).(string)
		s.logger.Printf("upstream queue timeout request_id=%s operation=%s", id, operation)
		writeError(w, http.StatusGatewayTimeout, "REQUEST_QUEUE_TIMEOUT", "目录请求排队超时，请稍后刷新重试", id)
		return
	}
	<-s.queue
	defer func() { <-s.slots }()

	result, err := call(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		if len(stale) > 0 {
			s.logger.Printf("serving stale API cache operation=%s", operation)
			s.writePayload(w, stale)
			return
		}
		id, _ := r.Context().Value(requestIDKey).(string)
		status, code, message := http.StatusBadGateway, "UPSTREAM_ERROR", "片库请求失败，请稍后重试"
		switch {
		case errors.Is(err, errSeriesNotFound):
			status, code, message = http.StatusNotFound, "SERIES_NOT_FOUND", "作品不存在或暂不可用"
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
	case "discover", "new-releases":
		return 70 * time.Minute
	case "rank", "short-rank":
		return 40 * time.Minute
	case "search":
		return 6 * time.Hour
	case "hot-search":
		return 30 * time.Minute
	case "series", "series-detail", "seasons":
		return 24 * time.Hour
	case "episodes":
		return 12 * time.Hour
	case "suggest":
		return 24 * time.Hour
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
		return nil, false
	}
	return item.data, true
}

func (s *Server) getStaleCached(key string) ([]byte, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	item, ok := s.apiCache[key]
	if !ok || len(item.data) == 0 {
		return nil, false
	}
	return item.data, true
}

func (s *Server) storeCached(key string, data []byte, ttl time.Duration) {
	if len(data) == 0 {
		return
	}
	settings := defaultRuntimeSettings()
	if s.settings != nil {
		settings = s.settings.Get()
	}
	maxBytes := settings.APIRAMMiB << 20
	maxEntries := settings.APIMaxEntries
	if len(data) > maxBytes/2 {
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
	for len(s.apiCache) >= maxEntries || s.cacheBytes+len(data) > maxBytes {
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

