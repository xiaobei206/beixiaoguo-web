package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/beilunyang/openhongguo/sdk"

	"github.com/xiaobei206/beixiaoguo-web/internal/webui"
)

type Server struct {
	client  *sdk.Client
	version string
	logger  *log.Logger
	slots   chan struct{}
	seq     atomic.Uint64
}

type contextKey uint8

const requestIDKey contextKey = 1

func New(client *sdk.Client, version string, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		client:  client,
		version: version,
		logger:  logger,
		slots:   make(chan struct{}, 4),
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
	mux.Handle("/", webui.Handler())

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("bxg-%08d", s.seq.Add(1))
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		r = r.WithContext(ctx)

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; media-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")

		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Printf("http request_id=%s method=%s path=%q duration_ms=%d", id, r.Method, r.URL.Path, time.Since(started).Milliseconds())
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"name":    "贝小果",
		"version": s.version,
		"runtime": "go",
	})
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	channel := strings.TrimSpace(q.Get("channel"))
	cursor := strings.TrimSpace(q.Get("cursor"))
	s.run(w, r, "discover", func(ctx context.Context) (any, error) {
		return s.client.Discover(ctx, sdk.DiscoverOptions{Channel: channel, Cursor: cursor})
	})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if q == "" || len([]rune(q)) > 100 {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "请输入 1 到 100 个字符的搜索词", "")
		return
	}
	s.run(w, r, "search", func(ctx context.Context) (any, error) {
		return s.client.Search(ctx, q, sdk.SearchOptions{Cursor: cursor, PageSize: 20})
	})
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
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	s.run(w, r, "new-releases", func(ctx context.Context) (any, error) {
		return s.client.NewReleases(ctx, sdk.NewReleaseQuery{Date: date, Cursor: cursor})
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

func (s *Server) run(w http.ResponseWriter, r *http.Request, operation string, call func(context.Context) (any, error)) {
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
		status := http.StatusBadGateway
		code := "UPSTREAM_ERROR"
		message := "片库请求失败，请稍后重试"
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			code = "UPSTREAM_TIMEOUT"
			message = "片库响应超时，请稍后重试"
		}
		s.logger.Printf("upstream request_id=%s operation=%s error_type=%T error=%v", id, operation, err, err)
		writeError(w, status, code, message, id)
		return
	}
	writeJSON(w, http.StatusOK, result)
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
			"code":       code,
			"message":    message,
			"request_id": requestID,
		},
	})
}
