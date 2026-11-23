package media

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beilunyang/openhongguo/sdk"
	"github.com/beilunyang/openhongguo/sdk/model"

	"github.com/xiaobei206/beixiaoguo-web/internal/cenc"
)

const (
	probeSize       = int64(64 << 10)
	maxSessions     = 12
	sessionLifetime = 45 * time.Minute
	maxMediaStreams = 2
	streamBuffer    = 64 << 10
)

type PlayInfo struct {
	EpisodeID       string          `json:"episode_id"`
	Source          string          `json:"src"`
	Quality         string          `json:"quality,omitempty"`
	Codec           string          `json:"codec,omitempty"`
	Width           int             `json:"width,omitempty"`
	Height          int             `json:"height,omitempty"`
	DurationSeconds int             `json:"duration_seconds,omitempty"`
	Available       []QualityOption `json:"available_qualities"`
	Protected       bool            `json:"protected"`
	ExpiresAt       time.Time       `json:"expires_at"`
}

type QualityOption struct {
	Label  string `json:"label"`
	Codec  string `json:"codec,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type Service struct {
	client      *sdk.Client
	httpClient  *http.Client
	logger      *log.Logger
	validateURL func(*url.URL) error

	mu       sync.Mutex
	sessions map[string]*playbackSession
	streams  chan struct{}
}

type playbackSession struct {
	episodeID string
	sources   []string
	key       []byte
	protected bool
	quality   string
	expiresAt time.Time

	mu       sync.Mutex
	prepared bool
	size     int64
	layout   *cenc.Layout
}

type byteRange struct {
	start int64
	end   int64
}

var (
	ErrUnsupportedStream = errors.New("media: only progressive MP4 streams are supported")
	ErrInvalidRange      = errors.New("media: invalid byte range")
	ErrRangeUnsatisfied  = errors.New("media: byte range is not satisfiable")
)

func New(client *sdk.Client, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		MaxConnsPerHost:       3,
		IdleConnTimeout:       45 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 12 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext:           safeDialContext(&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}),
	}
	return &Service{
		client:      client,
		httpClient:  &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		logger:      logger,
		validateURL: validatePublicURL,
		sessions:    make(map[string]*playbackSession),
		streams:     make(chan struct{}, maxMediaStreams),
	}
}

// Prepare resolves an episode's playback model and returns a same-origin media URL.
// CDN URLs and CENC key material remain server-side.
func (s *Service) Prepare(ctx context.Context, episodeID, quality string) (*PlayInfo, error) {
	episodeID = strings.TrimSpace(episodeID)
	if episodeID == "" || len(episodeID) > 128 {
		return nil, fmt.Errorf("invalid episode ID")
	}
	pb, err := s.client.ResolvePlayback(ctx, episodeID, sdk.PlaybackOptions{})
	if err != nil && !errors.Is(err, sdk.ErrDRMProtected) {
		return nil, err
	}
	if pb == nil || len(pb.Streams) == 0 {
		return nil, fmt.Errorf("playback model contains no streams")
	}

	stream, available, err := chooseStream(pb.Streams, quality)
	if err != nil {
		return nil, err
	}
	if stream.HLS || stream.Dash || pb.AudioURL != "" {
		return nil, ErrUnsupportedStream
	}
	protected := pb.Protected || stream.Protected
	if protected && stream.Encryption != "" && !strings.EqualFold(stream.Encryption, "cenc-aes-ctr") {
		return nil, fmt.Errorf("%w: unsupported encryption %q", ErrUnsupportedStream, stream.Encryption)
	}

	var key []byte
	if protected {
		contentKey, keyErr := sdk.ContentKey(pb)
		if keyErr != nil {
			return nil, fmt.Errorf("derive CENC content key: %w", keyErr)
		}
		key, err = hex.DecodeString(contentKey.Hex)
		if err != nil || len(key) != 16 {
			return nil, fmt.Errorf("SDK returned an invalid CENC content key")
		}
	}

	sources := make([]string, 0, 2)
	for _, raw := range []string{stream.URL, stream.BackupURL} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if _, err := s.parseSourceURL(raw); err != nil {
			continue
		}
		duplicate := false
		for _, old := range sources {
			if old == raw {
				duplicate = true
				break
			}
		}
		if !duplicate {
			sources = append(sources, raw)
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no permitted HTTPS MP4 source was returned")
	}

	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("create media session: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	expires := time.Now().Add(sessionLifetime)
	if pb.ExpiresAt > 0 {
		upstreamExpiry := time.Unix(pb.ExpiresAt, 0)
		if upstreamExpiry.Before(expires) {
			expires = upstreamExpiry
		}
	}
	if time.Until(expires) < time.Minute {
		return nil, fmt.Errorf("playback address is too close to expiry; retry playback")
	}

	session := &playbackSession{
		episodeID: episodeID,
		sources:   sources,
		key:       key,
		protected: protected,
		quality:   streamLabel(stream),
		expiresAt: expires,
	}
	s.storeSession(token, session)

	return &PlayInfo{
		EpisodeID:       episodeID,
		Source:          "/api/v1/media/" + token,
		Quality:         session.quality,
		Codec:           stream.Codec,
		Width:           stream.Width,
		Height:          stream.Height,
		DurationSeconds: pb.DurationSeconds,
		Available:       available,
		Protected:       protected,
		ExpiresAt:       expires,
	}, nil
}

func chooseStream(streams []model.Stream, requested string) (model.Stream, []QualityOption, error) {
	usable := make([]model.Stream, 0, len(streams))
	for _, stream := range streams {
		if stream.URL == "" || stream.HLS || stream.Dash {
			continue
		}
		usable = append(usable, stream)
	}
	if len(usable) == 0 {
		return model.Stream{}, nil, ErrUnsupportedStream
	}
	sort.SliceStable(usable, func(i, j int) bool {
		hi, hj := streamHeight(usable[i]), streamHeight(usable[j])
		if hi != hj {
			return hi > hj
		}
		return usable[i].Bitrate > usable[j].Bitrate
	})

	available := make([]QualityOption, 0, len(usable))
	for _, stream := range usable {
		label := streamLabel(stream)
		duplicate := false
		for _, old := range available {
			if old.Label == label {
				duplicate = true
				break
			}
		}
		if !duplicate {
			available = append(available, QualityOption{Label: label, Codec: stream.Codec, Width: stream.Width, Height: stream.Height})
		}
	}
	if requested == "" || strings.EqualFold(requested, "best") {
		return usable[0], available, nil
	}
	if strings.EqualFold(requested, "worst") {
		return usable[len(usable)-1], available, nil
	}
	for _, stream := range usable {
		if strings.EqualFold(streamLabel(stream), requested) || strings.EqualFold(stream.Codec, requested) {
			return stream, available, nil
		}
	}
	return model.Stream{}, available, fmt.Errorf("requested quality %q is unavailable", requested)
}

func streamHeight(stream model.Stream) int {
	if stream.Height > 0 {
		return stream.Height
	}
	height := 0
	for _, r := range stream.Definition {
		if r < '0' || r > '9' {
			break
		}
		height = height*10 + int(r-'0')
	}
	if height > 0 {
		return height
	}
	return stream.Width
}

func streamLabel(stream model.Stream) string {
	switch {
	case stream.Definition != "":
		return stream.Definition
	case stream.Quality != "":
		return stream.Quality
	case stream.Height > 0:
		return strconv.Itoa(stream.Height) + "p"
	default:
		return "自动"
	}
}

func (s *Service) storeSession(token string, session *playbackSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, old := range s.sessions {
		if now.After(old.expiresAt) {
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) >= maxSessions {
		var oldestToken string
		var oldest time.Time
		for id, old := range s.sessions {
			if oldestToken == "" || old.expiresAt.Before(oldest) {
				oldestToken, oldest = id, old.expiresAt
			}
		}
		delete(s.sessions, oldestToken)
	}
	s.sessions[token] = session
}

func (s *Service) session(token string) *playbackSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[token]
	if session != nil && time.Now().After(session.expiresAt) {
		delete(s.sessions, token)
		return nil
	}
	return session
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := s.session(r.PathValue("token"))
	if session == nil {
		http.Error(w, "media session expired; resolve playback again", http.StatusGone)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	case <-r.Context().Done():
		return
	}

	if err := session.prepare(r.Context(), s); err != nil {
		s.logger.Printf("media prepare episode=%s error_type=%T error=%v", session.episodeID, err, err)
		http.Error(w, "unable to prepare media stream", http.StatusBadGateway)
		return
	}
	requested, partial, err := parseRange(r.Header.Get("Range"), session.size)
	if err != nil {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", session.size))
		http.Error(w, "requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	var upstream *http.Response
	if r.Method == http.MethodGet {
		upstream, err = s.openRange(r.Context(), session, requested.start, requested.end)
		if err != nil {
			s.logger.Printf("media upstream episode=%s range=%d-%d error_type=%T error=%v", session.episodeID, requested.start, requested.end, err, err)
			http.Error(w, "media source did not honor byte range", http.StatusBadGateway)
			return
		}
		defer upstream.Body.Close()
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(requested.end-requested.start+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.start, requested.end, session.size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}

	buffer := make([]byte, streamBuffer)
	position := requested.start
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if session.layout != nil {
				session.layout.Decrypt(chunk, position)
			}
			written, writeErr := w.Write(chunk)
			position += int64(written)
			if writeErr != nil || written != n {
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				s.logger.Printf("media read episode=%s range=%d-%d error_type=%T error=%v", session.episodeID, requested.start, requested.end, readErr, readErr)
			}
			return
		}
	}
}

func (s *Service) parseSourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if s.validateURL != nil {
		if err := s.validateURL(u); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (s *Service) validateSource(raw string) error {
	_, err := s.parseSourceURL(raw)
	return err
}

func (session *playbackSession) prepare(ctx context.Context, service *Service) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.prepared {
		return nil
	}
	if time.Now().After(session.expiresAt) {
		return fmt.Errorf("playback session expired")
	}

	head, total, err := service.fetchBytes(ctx, session, 0, probeSize)
	if err != nil {
		return err
	}
	if total <= 0 || len(head) == 0 {
		return fmt.Errorf("upstream returned an empty media file")
	}
	if session.protected {
		layout, layoutErr := cenc.Open(total, session.key, func(offset, count int64) ([]byte, error) {
			return service.fetchBytesExact(ctx, session, offset, count, total)
		})
		if layoutErr != nil {
			return fmt.Errorf("parse CENC MP4 layout: %w", layoutErr)
		}
		if !layout.IsProtected() {
			return fmt.Errorf("playback was marked protected but MP4 contains no protected samples")
		}
		session.layout = layout
	}
	for i := range session.key {
		session.key[i] = 0
	}
	session.key = nil
	session.size = total
	session.prepared = true
	return nil
}

func (s *Service) fetchBytes(ctx context.Context, session *playbackSession, offset, count int64) ([]byte, int64, error) {
	end := offset + count - 1
	if offset < 0 || count <= 0 || end < offset {
		return nil, 0, fmt.Errorf("invalid upstream byte range")
	}
	var lastErr error
	for _, source := range session.sources {
		if err := s.validateSource(source); err != nil {
			lastErr = err
			continue
		}
		resp, err := s.openRawRange(ctx, source, offset, end)
		if err != nil {
			lastErr = err
			continue
		}
		crStart, crEnd, total, parseErr := parseContentRange(resp.Header.Get("Content-Range"))
		if parseErr != nil || crStart != offset || crEnd > end || total <= crEnd {
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream returned an invalid Content-Range")
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, count+1))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if int64(len(body)) != crEnd-crStart+1 {
			lastErr = fmt.Errorf("upstream range body length did not match Content-Range")
			continue
		}
		return body, total, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable media source")
	}
	return nil, 0, lastErr
}

func (s *Service) fetchBytesExact(ctx context.Context, session *playbackSession, offset, count, total int64) ([]byte, error) {
	if offset < 0 || count < 0 || offset > total || count > total-offset {
		return nil, fmt.Errorf("CENC requested an out-of-file auxiliary range")
	}
	body, gotTotal, err := s.fetchBytes(ctx, session, offset, count)
	if err != nil {
		return nil, err
	}
	if gotTotal != total || int64(len(body)) != count {
		return nil, fmt.Errorf("upstream auxiliary range was incomplete")
	}
	return body, nil
}

func (s *Service) openRange(ctx context.Context, session *playbackSession, start, end int64) (*http.Response, error) {
	var lastErr error
	for _, source := range session.sources {
		if err := s.validateSource(source); err != nil {
			lastErr = err
			continue
		}
		resp, err := s.openRawRange(ctx, source, start, end)
		if err != nil {
			lastErr = err
			continue
		}
		crStart, crEnd, total, parseErr := parseContentRange(resp.Header.Get("Content-Range"))
		if parseErr != nil || resp.StatusCode != http.StatusPartialContent ||
			crStart != start || crEnd != end || total != session.size {
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream did not honor the requested byte range")
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no media source")
	}
	return nil, lastErr
}

func (s *Service) openRawRange(ctx context.Context, source string, start, end int64) (*http.Response, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid byte range")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept", "video/mp4,application/octet-stream;q=0.9,*/*;q=0.1")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream HTTP status %d for byte range", resp.StatusCode)
	}
	return resp, nil
}

func parseRange(header string, size int64) (byteRange, bool, error) {
	if size <= 0 {
		return byteRange{}, false, ErrRangeUnsatisfied
	}
	if strings.TrimSpace(header) == "" {
		return byteRange{start: 0, end: size - 1}, false, nil
	}
	header = strings.TrimSpace(header)
	if len(header) < 6 || !strings.EqualFold(header[:6], "bytes=") {
		return byteRange{}, false, ErrInvalidRange
	}
	value := header[6:]
	if strings.Contains(value, ",") {
		return byteRange{}, false, ErrInvalidRange
	}
	parts := strings.SplitN(value, "-", 2)
	if len(parts) != 2 {
		return byteRange{}, false, ErrInvalidRange
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return byteRange{}, false, ErrInvalidRange
		}
		if suffix > size {
			suffix = size
		}
		return byteRange{start: size - suffix, end: size - 1}, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return byteRange{}, false, ErrRangeUnsatisfied
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return byteRange{}, false, ErrInvalidRange
		}
		if end >= size {
			end = size - 1
		}
	}
	return byteRange{start: start, end: end}, true, nil
}

func parseContentRange(value string) (start, end, total int64, err error) {
	value = strings.TrimSpace(value)
	if len(value) < 7 || !strings.EqualFold(value[:6], "bytes ") {
		return 0, 0, 0, fmt.Errorf("missing bytes unit")
	}
	parts := strings.SplitN(strings.TrimSpace(value[6:]), "/", 2)
	if len(parts) != 2 || parts[1] == "*" {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range")
	}
	rangeParts := strings.SplitN(parts[0], "-", 2)
	if len(rangeParts) != 2 {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range")
	}
	start, err = strconv.ParseInt(rangeParts[0], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	end, err = strconv.ParseInt(rangeParts[1], 10, 64)
	if err != nil {
		return 0, 0, 0, err
	}
	total, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range bounds")
	}
	return start, end, total, nil
}

func validatePublicURL(u *url.URL) error {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("media source must be an HTTPS URL without credentials")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("media source uses a disallowed port")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("media source host is not public")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !isPublicIP(ip) {
		return fmt.Errorf("media source resolves to a non-public IP")
	}
	return nil
}

func isPublicIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() ||
		ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	sharedCarrier := netip.MustParsePrefix("100.64.0.0/10")
	return !sharedCarrier.Contains(ip)
}

func safeDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if net.ParseIP(host) != nil {
			ip, _ := netip.ParseAddr(host)
			if !isPublicIP(ip) {
				return nil, fmt.Errorf("blocked connection to non-public IP")
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("media source host has no IP addresses")
		}
		for _, candidate := range ips {
			ip, ok := netip.AddrFromSlice(candidate.IP)
			if !ok || !isPublicIP(ip) {
				return nil, fmt.Errorf("blocked DNS response containing a non-public IP")
			}
		}
		var lastErr error
		for _, candidate := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}
