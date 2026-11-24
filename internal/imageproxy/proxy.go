package imageproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxImageBytes = 8 << 20
	maxCacheBytes = 224 << 20
	entryLifetime = 7 * 24 * time.Hour
	cacheLifetime = 12 * time.Hour

	maxDiskCacheBytes      = int64(1 << 30)
	imageFetchConcurrency  = 8
	imageFetchQueueCapacity = 64
)

type Proxy struct {
	client      *http.Client
	logger      *log.Logger
	validate    func(*url.URL) error
	disk        *diskCache
	fetchSlots  chan struct{}
	fetchQueue  chan struct{}
	mu          sync.Mutex
	byToken     map[string]*entry
	bySource    map[string]string
	cacheBytes  int64
}

type entry struct {
	token string
	sourceKey string
	sourceURL string
	expiresAt time.Time
	fetchMu sync.Mutex
	body []byte
	contentType string
	etag string
	cachedAt time.Time
	lastUsed time.Time
}

func New(loggers ...*log.Logger) *Proxy {
	logger := log.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}

	diskDir := strings.TrimSpace(os.Getenv("BEIXIAOGUO_IMAGE_CACHE_DIR"))
	disk, diskErr := newDiskCache(diskDir, maxDiskCacheBytes)
	if diskErr != nil {
		logger.Printf("image disk cache disabled error_type=%T", diskErr)
		disk = nil
	} else if disk != nil {
		diskBytes, diskEntries := disk.stats()
		logger.Printf("image disk cache ready bytes=%d entries=%d max_bytes=%d", diskBytes, diskEntries, maxDiskCacheBytes)
	}

	transport := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       45 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 12 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: safeDialContext(&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 30 * time.Second,
		}),
	}
	p := &Proxy{
		logger:     logger,
		validate:   validateURL,
		disk:       disk,
		fetchSlots: make(chan struct{}, imageFetchConcurrency),
		fetchQueue: make(chan struct{}, imageFetchQueueCapacity),
		byToken:    make(map[string]*entry),
		bySource:  make(map[string]string),
	}
	p.client = &http.Client{
		Transport: transport,
		Timeout:   25 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many image CDN redirects")
			}
			if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("image CDN redirect attempted HTTPS downgrade")
			}
			if err := p.validate(req.URL); err != nil {
				return fmt.Errorf("unsafe image CDN redirect: %w", err)
			}
			return nil
		},
	}
	return p
}

func (p *Proxy) Register(raw string) string {
	u,err:=url.Parse(strings.TrimSpace(raw))
	if err!=nil {
		p.logger.Printf("image source rejected reason=url_parse_error")
		return ""
	}
	if validationErr:=p.validate(u);validationErr!=nil {
		p.logger.Printf("image source rejected host=%q reason=%v",u.Hostname(),validationErr)
		return ""
	}
	canonical:=strings.ToLower(u.Scheme)+"://"+strings.ToLower(u.Host)+u.EscapedPath()
	if canonical=="" {return ""}
	sum:=sha256.Sum256([]byte(canonical))
	sourceKey:=hex.EncodeToString(sum[:])

	p.mu.Lock()
	defer p.mu.Unlock()
	now:=time.Now()
	p.pruneLocked(now)
	if token:=p.bySource[sourceKey]; token!="" {
		if old:=p.byToken[token]; old!=nil {
			old.sourceURL=raw
			old.expiresAt=now.Add(entryLifetime)
			return "/api/v1/images/"+token
		}
	}
	random:=make([]byte,16)
	if _,err:=rand.Read(random);err!=nil {return ""}
	token:=hex.EncodeToString(random)
	p.byToken[token]=&entry{token:token,sourceKey:sourceKey,sourceURL:raw,expiresAt:now.Add(entryLifetime),lastUsed:now}
	p.bySource[sourceKey]=token
	return "/api/v1/images/"+token
}

func (p *Proxy) RewriteJSON(raw []byte) ([]byte,error) {
	rewritten,err:=rewriteJSON(raw,p)
	if err!=nil {return nil,err}
	return rewritten,nil
}

// rewriteJSON walks raw JSON values so rewriting an image URL never converts
// large integer fields through float64 and never changes their exact value.
func rewriteJSON(raw json.RawMessage,p *Proxy) (json.RawMessage,error) {
	trimmed:=bytes.TrimSpace(raw)
	if len(trimmed)==0 {return nil,fmt.Errorf("empty JSON value")}
	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if err:=json.Unmarshal(trimmed,&object);err!=nil {return nil,err}
		for key,value:=range object {
			if isImageField(key) {
				var source string
				if err:=json.Unmarshal(value,&source);err==nil && strings.TrimSpace(source)!="" {
					if local:=p.Register(source);local!="" {
						encoded,_:=json.Marshal(local)
						object[key]=encoded
						continue
					}
				}
			}
			rewritten,err:=rewriteJSON(value,p)
			if err!=nil {return nil,err}
			object[key]=rewritten
		}
		return json.Marshal(object)
	case '[':
		var items []json.RawMessage
		if err:=json.Unmarshal(trimmed,&items);err!=nil {return nil,err}
		for index,value:=range items {
			rewritten,err:=rewriteJSON(value,p)
			if err!=nil {return nil,err}
			items[index]=rewritten
		}
		return json.Marshal(items)
	default:
		return append(json.RawMessage(nil),trimmed...),nil
	}
}

func isImageField(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(key,"-","_")) {
	case "cover_url","avatar_url","image_url","poster_url","banner_url","thumb_url","thumbnail_url","background_url","head_url","picture_url":
		return true
	default:
		return false
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter,r *http.Request) {
	if r.Method!=http.MethodGet && r.Method!=http.MethodHead {
		w.Header().Set("Allow","GET, HEAD")
		http.Error(w,"method not allowed",http.StatusMethodNotAllowed)
		return
	}
	token:=r.PathValue("token")
	p.mu.Lock()
	item:=p.byToken[token]
	if item!=nil && time.Now().After(item.expiresAt) {
		p.removeLocked(token)
		item=nil
	}
	if item==nil {
		p.mu.Unlock()
		http.NotFound(w,r)
		return
	}
	item.lastUsed=time.Now()
	body:=item.body
	contentType:=item.contentType
	etag:=item.etag
	fresh:=len(body)>0 && time.Since(item.cachedAt)<cacheLifetime
	source:=item.sourceURL
	p.mu.Unlock()

	if !fresh {
		staleBody, staleType, staleETag := body, contentType, etag
		fetchedBody, fetchedType, fetchedETag, err := p.fetch(r.Context(), item, source)
		if err != nil {
			host := ""
			if u, parseErr := url.Parse(source); parseErr == nil {
				host = u.Hostname()
			}
			if len(staleBody) > 0 {
				p.logger.Printf("image proxy serving stale cache source_host=%q reason=%q", host, err.Error())
				body, contentType, etag = staleBody, staleType, staleETag
			} else {
				status := http.StatusBadGateway
				if errors.Is(err, errImageQueueFull) {
					status = http.StatusServiceUnavailable
					w.Header().Set("Retry-After", "2")
				}
				p.logger.Printf("image proxy failed source_host=%q error=%q", host, err.Error())
				http.Error(w, "cover image temporarily unavailable", status)
				return
			}
		} else {
			body, contentType, etag = fetchedBody, fetchedType, fetchedETag
		}
	}

	w.Header().Set("Content-Type",contentType)
	w.Header().Set("Cache-Control", "public, max-age=604800, stale-while-revalidate=2592000, stale-if-error=2592000")
	w.Header().Set("ETag",etag)
	w.Header().Set("X-Content-Type-Options","nosniff")
	w.Header().Set("Vary","Accept")
	if r.Header.Get("If-None-Match")==etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length",strconv.Itoa(len(body)))
	if r.Method==http.MethodHead {return}
	_,_=w.Write(body)
}

// officialJPEGVariant returns the browser-compatible JPEG rendition published by
// the official image CDN for eligible Red Fruit HEIC covers. It is deliberately
// limited to the known CDN domains and /novel-pic/ object path.
func officialJPEGVariant(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	knownCDN := host == "fqnovelpic.com" ||
		strings.HasSuffix(host, ".fqnovelpic.com") ||
		host == "byteimg.com" ||
		strings.HasSuffix(host, ".byteimg.com")
	path := u.Path
	if !knownCDN ||
		!strings.HasPrefix(path, "/novel-pic/") ||
		!strings.Contains(path, "~") ||
		!strings.HasSuffix(strings.ToLower(path), ".heic") {
		return "", false
	}

	// The CDN variant changes the representation, not the underlying image
	// object. Do not carry the original signature/query into the rendition URL.
	basePath := strings.SplitN(path, "~", 2)[0]
	u.Scheme = "https"
	u.Host = "p3-novel.byteimg.com"
	u.Path = basePath + "~tplv-shrink:640:0.image"
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String(), true
}

var errImageQueueFull = errors.New("image proxy request queue is full")

func (p *Proxy) fetch(ctx context.Context, item *entry, raw string) ([]byte, string, string, error) {
	item.fetchMu.Lock()
	defer item.fetchMu.Unlock()

	p.mu.Lock()
	if len(item.body) > 0 && time.Since(item.cachedAt) < cacheLifetime {
		body, contentType, etag := item.body, item.contentType, item.etag
		p.mu.Unlock()
		return body, contentType, etag, nil
	}
	hasStaleRAM := len(item.body) > 0
	raw = item.sourceURL
	p.mu.Unlock()

	// After a process restart there is no RAM entry, so a persistent hit can
	// be promoted immediately. An expired RAM entry instead triggers a refresh;
	// ServeHTTP retains it as stale-if-error fallback while the refresh runs.
	if !hasStaleRAM && p.disk != nil {
		if body, contentType, ok := p.disk.load(item.sourceKey); ok {
			etag := p.cacheRAM(item, body, contentType)
			return body, contentType, etag, nil
		}
	}

	sourceURL, err := url.Parse(raw)
	if err != nil || p.validate(sourceURL) != nil {
		return nil, "", "", fmt.Errorf("invalid cover source")
	}

	if variantURL, ok := officialJPEGVariant(raw); ok {
		body, contentType, variantErr := p.fetchSource(ctx, raw, variantURL, true)
		if variantErr == nil {
			return p.cacheFetched(item, body, contentType)
		}
		if ctx.Err() != nil {
			return nil, "", "", ctx.Err()
		}
		if errors.Is(variantErr, errImageQueueFull) {
			return nil, "", "", variantErr
		}
		p.logger.Printf("image proxy JPEG variant failed source_host=%q error=%q; falling back to original",
			sourceURL.Hostname(), variantErr.Error())
	}

	body, contentType, err := p.fetchSource(ctx, raw, raw, false)
	if err != nil {
		return nil, "", "", err
	}
	return p.cacheFetched(item, body, contentType)
}

func (p *Proxy) acquireFetchSlot(ctx context.Context) (func(), error) {
	select {
	case p.fetchQueue <- struct{}{}:
	default:
		return nil, errImageQueueFull
	}
	select {
	case p.fetchSlots <- struct{}{}:
		<-p.fetchQueue
		return func() { <-p.fetchSlots }, nil
	case <-ctx.Done():
		<-p.fetchQueue
		return nil, ctx.Err()
	}
}

func (p *Proxy) fetchSource(ctx context.Context, sourceRaw, requestRaw string, requireJPEG bool) ([]byte, string, error) {
	sourceURL, err := url.Parse(sourceRaw)
	if err != nil {
		return nil, "", fmt.Errorf("invalid cover source URL")
	}
	requestURL, err := url.Parse(requestRaw)
	if err != nil || p.validate(requestURL) != nil {
		return nil, "", fmt.Errorf("invalid cover request URL")
	}
	release, err := p.acquireFetchSlot(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestRaw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/jpeg,image/png,image/webp;q=0.95,image/avif;q=0.85,*/*;q=0.2")
	req.Header.Set("User-Agent", "Beixiaoguo/0.1.12 image-proxy")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	finalHost := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalHost = resp.Request.URL.Hostname()
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cover server returned HTTP %d after redirect (final_host=%q)", resp.StatusCode, finalHost)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) == 0 || len(body) > maxImageBytes {
		return nil, "", fmt.Errorf("cover image size is invalid")
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if requireJPEG {
		if !hasJPEGSignature(body) {
			return nil, "", fmt.Errorf("official cover variant did not return JPEG bytes")
		}
		// The file signature is authoritative for this known JPEG endpoint.
		contentType = "image/jpeg"
	} else {
		if contentType == "" || strings.EqualFold(contentType, "application/octet-stream") {
			contentType = http.DetectContentType(body)
		}
		if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
			return nil, "", fmt.Errorf("upstream response is not an image")
		}
	}
	p.logger.Printf("image proxy fetched source_host=%q final_host=%q content_type=%q bytes=%d official_jpeg_variant=%t",
		sourceURL.Hostname(), finalHost, contentType, len(body), requireJPEG)
	return body, contentType, nil
}

func hasJPEGSignature(body []byte) bool {
	return len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff
}

func (p *Proxy) cacheRAM(item *entry, body []byte, contentType string) string {
	sum := sha256.Sum256(body)
	etag := """ + hex.EncodeToString(sum[:]) + """

	p.mu.Lock()
	delta := int64(len(body)) - int64(len(item.body))
	item.body = body
	item.contentType = contentType
	item.etag = etag
	item.cachedAt = time.Now()
	item.lastUsed = time.Now()
	p.cacheBytes += delta
	p.evictLocked(item.token)
	p.mu.Unlock()
	return etag
}

func (p *Proxy) cacheFetched(item *entry, body []byte, contentType string) ([]byte, string, string, error) {
	etag := p.cacheRAM(item, body, contentType)
	if p.disk != nil {
		if err := p.disk.store(item.sourceKey, contentType, body); err != nil {
			p.logger.Printf("image disk cache write failed error_type=%T", err)
		}
	}
	return body, contentType, etag, nil
}

type CacheStats struct {
	RAMBytes           int64
	RAMEntries         int
	DiskBytes          int64
	DiskEntries        int
	FetchActive        int
	FetchQueued        int
	FetchQueueCapacity int
}

func (p *Proxy) Stats() CacheStats {
	stats := CacheStats{
		FetchActive:        len(p.fetchSlots),
		FetchQueued:        len(p.fetchQueue),
		FetchQueueCapacity: cap(p.fetchQueue),
	}
	p.mu.Lock()
	stats.RAMBytes = p.cacheBytes
	for _, item := range p.byToken {
		if len(item.body) > 0 {
			stats.RAMEntries++
		}
	}
	p.mu.Unlock()
	if p.disk != nil {
		stats.DiskBytes, stats.DiskEntries = p.disk.stats()
	}
	return stats
}

func (p *Proxy) pruneLocked(now time.Time) {
	for token,item:=range p.byToken {
		if now.After(item.expiresAt) {p.removeLocked(token)}
	}
}

func (p *Proxy) removeLocked(token string) {
	item:=p.byToken[token]
	if item==nil{return}
	p.cacheBytes-=int64(len(item.body))
	item.body=nil
	delete(p.byToken,token)
	delete(p.bySource,item.sourceKey)
}

func (p *Proxy) evictLocked(keep string) {
	for p.cacheBytes>maxCacheBytes {
		var victim *entry
		for token,item:=range p.byToken {
			if token==keep {continue}
			if victim==nil || item.lastUsed.Before(victim.lastUsed) {victim=item}
		}
		if victim==nil {break}
		p.cacheBytes-=int64(len(victim.body))
		victim.body=nil
		victim.contentType=""
		victim.etag=""
		victim.cachedAt=time.Time{}
	}
}

func validateURL(u *url.URL) error {
	if u==nil || u.Hostname()=="" || u.User!=nil || (u.Scheme!="https" && u.Scheme!="http") {return fmt.Errorf("invalid image URL")}
	port:=u.Port()
	if port!="" && !((u.Scheme=="https" && port=="443") || (u.Scheme=="http" && port=="80")) {return fmt.Errorf("invalid image port")}
	host:=strings.TrimSuffix(strings.ToLower(u.Hostname()),".")
	if host=="localhost" || strings.HasSuffix(host,".localhost") || strings.HasSuffix(host,".local") {return fmt.Errorf("local image host is blocked")}
	if ip,err:=netip.ParseAddr(host);err==nil && !isPublic(ip.Unmap()) {return fmt.Errorf("private image IP is blocked")}
	return nil
}

func isPublic(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return !netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

func safeDialContext(dialer *net.Dialer) func(context.Context,string,string)(net.Conn,error) {
	return func(ctx context.Context,network,address string)(net.Conn,error) {
		host,port,err:=net.SplitHostPort(address)
		if err!=nil{return nil,err}
		if net.ParseIP(host)!=nil {
			ip,err:=netip.ParseAddr(host)
			if err!=nil || !isPublic(ip.Unmap()) {return nil,fmt.Errorf("blocked image destination IP")}
			return dialer.DialContext(ctx,network,net.JoinHostPort(ip.Unmap().String(),port))
		}
		ips,err:=net.DefaultResolver.LookupIPAddr(ctx,host)
		if err!=nil{return nil,err}
		if len(ips)==0{return nil,fmt.Errorf("image host has no DNS addresses")}
		for _,candidate:=range ips {
			ip,ok:=netip.AddrFromSlice(candidate.IP)
			if !ok || !isPublic(ip.Unmap()) {return nil,fmt.Errorf("DNS returned a non-public image address")}
		}
		var last error
		for _,candidate:=range ips {
			ip,_:=netip.AddrFromSlice(candidate.IP)
			conn,dialErr:=dialer.DialContext(ctx,network,net.JoinHostPort(ip.Unmap().String(),port))
			if dialErr==nil{return conn,nil}
			last=dialErr
		}
		return nil,last
	}
}
