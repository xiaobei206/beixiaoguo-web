package imageproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxImageBytes = 8 << 20
	maxCacheBytes = 96 << 20
	entryLifetime = 7 * 24 * time.Hour
	cacheLifetime = 12 * time.Hour
)

type Proxy struct {
	client *http.Client
	logger *log.Logger
	validate func(*url.URL) error
	mu sync.Mutex
	byToken map[string]*entry
	bySource map[string]string
	cacheBytes int64
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
	logger:=log.Default()
	if len(loggers)>0 && loggers[0]!=nil {logger=loggers[0]}
	transport:=&http.Transport{
		Proxy:nil,
		MaxIdleConns:12,
		MaxIdleConnsPerHost:3,
		MaxConnsPerHost:4,
		IdleConnTimeout:45*time.Second,
		TLSHandshakeTimeout:8*time.Second,
		ResponseHeaderTimeout:12*time.Second,
		TLSClientConfig:&tls.Config{MinVersion:tls.VersionTLS12},
		DialContext:safeDialContext(&net.Dialer{Timeout:8*time.Second,KeepAlive:30*time.Second}),
	}
	p:=&Proxy{
		logger:logger,
		validate:validateURL,
		byToken:make(map[string]*entry),
		bySource:make(map[string]string),
	}
	p.client=&http.Client{
		Transport:transport,
		Timeout:25*time.Second,
		CheckRedirect:func(req *http.Request,via []*http.Request)error {
			if len(via)>=5 {return fmt.Errorf("too many image CDN redirects")}
			if len(via)>0 && via[len(via)-1].URL.Scheme=="https" && req.URL.Scheme!="https" {
				return fmt.Errorf("image CDN redirect attempted HTTPS downgrade")
			}
			if err:=p.validate(req.URL);err!=nil {return fmt.Errorf("unsafe image CDN redirect: %w",err)}
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
		var err error
		body,contentType,etag,err=p.fetch(r.Context(),item,source)
		if err!=nil {
			host:=""
			if u,parseErr:=url.Parse(source);parseErr==nil {host=u.Hostname()}
			p.logger.Printf("image proxy failed source_host=%q error=%q",host,err.Error())
			http.Error(w,"cover image temporarily unavailable",http.StatusBadGateway)
			return
		}
	}

	w.Header().Set("Content-Type",contentType)
	w.Header().Set("Cache-Control","public, max-age=43200, stale-while-revalidate=86400")
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

func (p *Proxy) fetch(ctx context.Context,item *entry,raw string)([]byte,string,string,error) {
	item.fetchMu.Lock()
	defer item.fetchMu.Unlock()
	p.mu.Lock()
	if len(item.body)>0 && time.Since(item.cachedAt)<cacheLifetime {
		body,contentType,etag:=item.body,item.contentType,item.etag
		p.mu.Unlock()
		return body,contentType,etag,nil
	}
	raw=item.sourceURL
	p.mu.Unlock()
	u,err:=url.Parse(raw)
	if err!=nil || p.validate(u)!=nil {return nil,"","",fmt.Errorf("invalid cover source")}
	req,err:=http.NewRequestWithContext(ctx,http.MethodGet,raw,nil)
	if err!=nil {return nil,"","",err}
	req.Header.Set("Accept","image/jpeg,image/png,image/webp;q=0.95,image/avif;q=0.85,*/*;q=0.2")
	req.Header.Set("User-Agent","Beixiaoguo/0.1.3 image-proxy")
	resp,err:=p.client.Do(req)
	if err!=nil {return nil,"","",err}
	defer resp.Body.Close()
	if resp.StatusCode!=http.StatusOK {
		finalHost:=""
		if resp.Request!=nil && resp.Request.URL!=nil {finalHost=resp.Request.URL.Hostname()}
		return nil,"","",fmt.Errorf("cover server returned HTTP %d after redirect (final_host=%q)",resp.StatusCode,finalHost)
	}
	body,err:=io.ReadAll(io.LimitReader(resp.Body,maxImageBytes+1))
	if err!=nil {return nil,"","",err}
	if len(body)==0 || len(body)>maxImageBytes {return nil,"","",fmt.Errorf("cover image size is invalid")}
	contentType,_,_:=mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType=="" || strings.EqualFold(contentType,"application/octet-stream") {
		contentType=http.DetectContentType(body)
	}
	if !strings.HasPrefix(strings.ToLower(contentType),"image/") {
		return nil,"","",fmt.Errorf("upstream response is not an image")
	}
	finalHost:=""
	if resp.Request!=nil && resp.Request.URL!=nil {finalHost=resp.Request.URL.Hostname()}
	p.logger.Printf("image proxy fetched source_host=%q final_host=%q content_type=%q bytes=%d",u.Hostname(),finalHost,contentType,len(body))
	sum:=sha256.Sum256(body)
	newETag:="\""+hex.EncodeToString(sum[:])+"\""

	p.mu.Lock()
	delta:=int64(len(body))-int64(len(item.body))
	item.body=body
	item.contentType=contentType
	item.etag=newETag
	item.cachedAt=time.Now()
	p.cacheBytes+=delta
	p.evictLocked(item.token)
	p.mu.Unlock()
	return body,contentType,newETag,nil
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
