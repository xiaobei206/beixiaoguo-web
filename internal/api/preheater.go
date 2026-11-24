package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/beilunyang/openhongguo/sdk"
	"github.com/beilunyang/openhongguo/sdk/model"
)

var errPreheatInterrupted = errors.New("preheat interrupted by playback or settings")

type preheatStatus struct {
	Running       bool      "json:\"running\""
	Phase         string    "json:\"phase\""
	Current       string    "json:\"current\""
	StartedAt     time.Time "json:\"started_at,omitempty\""
	FinishedAt    time.Time "json:\"finished_at,omitempty\""
	NextRunAt     time.Time "json:\"next_run_at,omitempty\""
	TargetItems   int       "json:\"target_items\""
	APIPages      int       "json:\"api_pages\""
	ItemsReady    int       "json:\"items_ready\""
	ImagesTotal   int       "json:\"images_total\""
	ImagesDone    int       "json:\"images_done\""
	ImagesFailed  int       "json:\"images_failed\""
	APIFailed     int       "json:\"api_failed\""
	Skipped       int       "json:\"skipped\""
	LastResult    string    "json:\"last_result\""
}

var preheatChannels = []string{"推荐", "真人剧", "漫剧", "找剧", "电影", "小说"}

func (s *Server) startPreheater() {
	s.preheatTrigger = make(chan struct{}, 1)
	go s.preheatLoop()
}

func (s *Server) preheatLoop() {
	cfg := s.settings.Get()
	if cfg.PreheatEnabled && cfg.PreheatOnStartup {
		s.runPreheatCycle()
	}
	for {
		cfg = s.settings.Get()
		nextHour := time.Now().Truncate(time.Hour).Add(time.Hour)
		wait := time.Until(nextHour)
		if wait < time.Second {
			wait = time.Second
		}
		if !cfg.PreheatEnabled || !cfg.PreheatHourly {
			wait = 30 * time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			cfg = s.settings.Get()
			if cfg.PreheatEnabled && cfg.PreheatHourly {
				s.runPreheatCycle()
			}
		case <-s.preheatTrigger:
			if !timer.Stop() {
				select { case <-timer.C: default: }
			}
			if s.settings.Get().PreheatEnabled {
				s.runPreheatCycle()
			}
		}
	}
}

func (s *Server) triggerPreheat() {
	if s.preheatTrigger == nil || s.preheatSnapshot().Running {
		return
	}
	select {
	case s.preheatTrigger <- struct{}{}:
	default:
	}
}

func (s *Server) preheatSnapshot() preheatStatus {
	s.preheatMu.Lock()
	defer s.preheatMu.Unlock()
	status := s.preheat
	if status.NextRunAt.IsZero() {
		status.NextRunAt = time.Now().Truncate(time.Hour).Add(time.Hour)
	}
	return status
}

func (s *Server) updatePreheat(change func(*preheatStatus)) {
	s.preheatMu.Lock()
	change(&s.preheat)
	s.preheatMu.Unlock()
}

func (s *Server) runPreheatCycle() {
	if !s.preheatMu.TryLock() {
		return
	}
	if s.preheat.Running {
		s.preheatMu.Unlock()
		return
	}
	now := time.Now()
	cfg := s.settings.Get()
	s.preheat = preheatStatus{
		Running: true, Phase: "准备目录", StartedAt: now,
		TargetItems: cfg.PreheatItems, LastResult: "运行中",
		NextRunAt: now.Truncate(time.Hour).Add(time.Hour),
	}
	s.preheatMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current := s.settings.Get()
				if !current.PreheatEnabled || s.media.PlaybackActive() {
					cancel()
					return
				}
			}
		}
	}()

	if s.media.PlaybackActive() {
		cancel()
	}
	covers := make([]string, 0, cfg.PreheatItems*8)
	for _, channel := range preheatChannels {
		if ctx.Err() != nil {
			break
		}
		s.updatePreheat(func(st *preheatStatus) { st.Phase = "目录预热"; st.Current = channel })
		items, err := s.preheatDiscover(ctx, channel, cfg.PreheatItems)
		covers = append(covers, items...)
		if err != nil {
			s.updatePreheat(func(st *preheatStatus) { st.APIFailed++ })
			if errors.Is(err, errPreheatInterrupted) || ctx.Err() != nil {
				break
			}
			continue
		}
	}
	if ctx.Err() == nil {
		s.updatePreheat(func(st *preheatStatus) { st.Phase = "当日新剧"; st.Current = "新剧上线" })
		items, err := s.preheatNewReleases(ctx, cfg.PreheatItems)
		covers = append(covers, items...)
		if err != nil {
			s.updatePreheat(func(st *preheatStatus) { st.APIFailed++ })
		}
	}
	if ctx.Err() == nil {
		s.updatePreheat(func(st *preheatStatus) { st.Phase = "排行榜"; st.Current = "短剧排行榜" })
		items, err := s.preheatRank(ctx, cfg.PreheatItems)
		covers = append(covers, items...)
		if err != nil {
			s.updatePreheat(func(st *preheatStatus) { st.APIFailed++ })
		}
	}
	if ctx.Err() == nil {
		s.updatePreheat(func(st *preheatStatus) { st.Phase = "热搜"; st.Current = "热搜榜" })
		items, err := s.preheatHotSearch(ctx)
		if err != nil {
			s.updatePreheat(func(st *preheatStatus) { st.APIFailed++ })
		} else {
			covers = append(covers, items...)
		}
	}

	covers = uniqueStrings(covers)
	if ctx.Err() == nil {
		s.updatePreheat(func(st *preheatStatus) {
			st.Phase = "封面预热"
			st.Current = "准备 " + fmt.Sprint(len(covers)) + " 张封面"
			st.ImagesTotal = len(covers)
		})
		s.preheatImages(ctx, covers)
	}
	interrupted := ctx.Err() != nil
	cancel()
	<-monitorDone
	s.updatePreheat(func(st *preheatStatus) {
		st.Running = false
		st.FinishedAt = time.Now()
		if interrupted {
			st.LastResult = "因播放活动或设置变更而暂停，未完成任务留待下轮"
			st.Skipped++
			st.Phase = "已暂停"
		} else if st.APIFailed > 0 || st.ImagesFailed > 0 {
			st.LastResult = "已完成，部分任务失败或超时"
			st.Phase = "完成（有跳过项）"
		} else {
			st.LastResult = "全部预热完成"
			st.Phase = "完成"
		}
		st.Current = ""
	})
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (s *Server) preheatCall(ctx context.Context, operation, requestURI string, call func(context.Context) (any, error)) ([]byte, error) {
	for {
		if ctx.Err() != nil || s.media.PlaybackActive() {
			return nil, errPreheatInterrupted
		}
		s.preheatRateMu.Lock()
		wait := 1200*time.Millisecond - time.Since(s.preheatLastCall)
		s.preheatRateMu.Unlock()
		if wait > 0 && !preheatDelay(ctx, wait) {
			return nil, errPreheatInterrupted
		}
		if len(s.queue) == 0 && len(s.slots) == 0 {
			select {
			case s.slots <- struct{}{}:
				goto acquired
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, errPreheatInterrupted
		case <-time.After(300 * time.Millisecond):
		}
	}

acquired:
	defer func() { <-s.slots }()
	s.preheatRateMu.Lock()
	s.preheatLastCall = time.Now()
	s.preheatRateMu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := call(callCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.logger.Printf("preheat timeout operation=%s uri=%q", operation, requestURI)
		} else {
			s.logger.Printf("preheat failed operation=%s error_type=%T", operation, err)
		}
		if ctx.Err() == nil {
			_ = preheatDelay(ctx, 1200*time.Millisecond)
		}
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	s.storeCached(operation+":"+requestURI, raw, cacheTTL(operation))
	s.updatePreheat(func(st *preheatStatus) { st.APIPages++ })
	return raw, nil
}

func (s *Server) preheatDiscover(ctx context.Context, channel string, target int) ([]string, error) {
	cursor := ""
	count := 0
	covers := make([]string, 0, target)
	for pageNo := 0; pageNo < 20 && count < target; pageNo++ {
		params := url.Values{}
		params.Set("channel", channel)
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		uri := "/api/v1/discover?" + params.Encode()
		var page *sdk.Page[model.SeriesSummary]
		_, err := s.preheatCall(ctx, "discover", uri, func(callCtx context.Context) (any, error) {
			result, callErr := s.client.Discover(callCtx, sdk.DiscoverOptions{Channel: channel, Cursor: cursor})
			page = result
			return result, callErr
		})
		if err != nil {
			return covers, err
		}
		if page == nil || len(page.Items) == 0 {
			break
		}
		count += len(page.Items)
		s.updatePreheat(func(st *preheatStatus) { st.ItemsReady += len(page.Items) })
		for _, item := range page.Items {
			covers = append(covers, item.CoverURL)
		}
		if !page.HasMore || page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}
	return covers, nil
}

func (s *Server) preheatNewReleases(ctx context.Context, target int) ([]string, error) {
	date := time.Now().Format("20060102")
	cursor := ""
	count := 0
	covers := make([]string, 0, target)
	for pageNo := 0; pageNo < 20 && count < target; pageNo++ {
		params := url.Values{}
		params.Set("date", date)
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		uri := "/api/v1/new-releases?" + params.Encode()
		var page *model.NewReleasePage
		_, err := s.preheatCall(ctx, "new-releases", uri, func(callCtx context.Context) (any, error) {
			result, callErr := s.client.NewReleases(callCtx, sdk.NewReleaseQuery{Date: date, Cursor: cursor})
			page = result
			return result, callErr
		})
		if err != nil {
			return covers, err
		}
		if page == nil || len(page.Items) == 0 {
			break
		}
		count += len(page.Items)
		s.updatePreheat(func(st *preheatStatus) { st.ItemsReady += len(page.Items) })
		for _, item := range page.Items {
			covers = append(covers, item.CoverURL)
		}
		if !page.HasMore || page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}
	return covers, nil
}

func (s *Server) preheatRank(ctx context.Context, target int) ([]string, error) {
	cursor := ""
	count := 0
	covers := make([]string, 0, target)
	for pageNo := 0; pageNo < 20 && count < target; pageNo++ {
		params := url.Values{}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		uri := "/api/v1/rank"
		if len(params) > 0 {
			uri += "?" + params.Encode()
		}
		var page *model.ShortRanking
		_, err := s.preheatCall(ctx, "short-rank", uri, func(callCtx context.Context) (any, error) {
			result, callErr := s.client.ShortRank(callCtx, sdk.ShortRankQuery{Cursor: cursor})
			page = result
			return result, callErr
		})
		if err != nil {
			return covers, err
		}
		if page == nil || len(page.Items) == 0 {
			break
		}
		count += len(page.Items)
		s.updatePreheat(func(st *preheatStatus) { st.ItemsReady += len(page.Items) })
		for _, item := range page.Items {
			covers = append(covers, item.CoverURL, item.AvatarURL)
		}
		if !page.HasMore || page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}
	return covers, nil
}

func (s *Server) preheatHotSearch(ctx context.Context) ([]string, error) {
	var hot *model.HotSearch
	_, err := s.preheatCall(ctx, "hot-search", "/api/v1/hot-search", func(callCtx context.Context) (any, error) {
		result, callErr := s.client.HotSearch(callCtx)
		hot = result
		return result, callErr
	})
	if err != nil || hot == nil {
		return nil, err
	}
	covers := make([]string, 0, 64)
	for _, board := range hot.Boards {
		for _, item := range board.Items {
			covers = append(covers, item.CoverURL)
		}
	}
	for _, item := range hot.Guesses {
		covers = append(covers, item.CoverURL)
	}
	s.updatePreheat(func(st *preheatStatus) { st.ItemsReady += len(covers) })
	return covers, nil
}

func (s *Server) preheatImages(ctx context.Context, urls []string) {
	if s.images == nil || len(urls) == 0 {
		return
	}
	jobs := make(chan string)
	var workers sync.WaitGroup
	workerCount := 2
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for raw := range jobs {
				if ctx.Err() != nil || s.media.PlaybackActive() {
					return
				}
				callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				err := s.images.Prefetch(callCtx, raw)
				cancel()
				s.updatePreheat(func(st *preheatStatus) {
					st.ImagesDone++
					if err != nil {
						st.ImagesFailed++
					}
				})
				if !preheatDelay(ctx, 500*time.Millisecond) {
					return
				}
			}
		}()
	}
sendLoop:
	for _, raw := range urls {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- raw:
		}
	}
	close(jobs)
	workers.Wait()
}

func preheatDelay(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Server) preheatRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.settings.Get().PreheatEnabled {
		writeJSON(w, 409, map[string]string{"error": "自动预热已关闭"})
		return
	}
	s.triggerPreheat()
	writeJSON(w, 202, map[string]any{"accepted": true, "status": s.preheatSnapshot()})
}

func settingsResponse(s *Server) map[string]any {
	stats := map[string]any{}
	if s.images != nil {
		imageStats := s.images.Stats()
		stats["image_ram_bytes"] = imageStats.RAMBytes
		stats["image_ram_limit_bytes"] = imageStats.RAMLimitBytes
		stats["image_disk_bytes"] = imageStats.DiskBytes
		stats["image_disk_limit_bytes"] = imageStats.DiskLimitBytes
		stats["image_ram_entries"] = imageStats.RAMEntries
		stats["image_disk_entries"] = imageStats.DiskEntries
	}
	s.cacheMu.Lock()
	stats["api_entries"] = len(s.apiCache)
	stats["api_bytes"] = s.cacheBytes
	s.cacheMu.Unlock()
	return map[string]any{
		"settings": s.settings.Get(),
		"preheat":  s.preheatSnapshot(),
		"cache":    stats,
	}
}
