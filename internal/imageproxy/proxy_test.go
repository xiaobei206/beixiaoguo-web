package imageproxy

import (
	"context"
	"errors"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRewriteJSONReplacesCoverURLsWithStableSameOriginURLs(t *testing.T) {
	p:=New()
	input:=[]byte("{\"items\":[{\"cover_url\":\"https://cdn.example/a.jpg?token=one\",\"title\":\"A\"},{\"avatar_url\":\"https://cdn.example/b.jpg?token=x\"}]}")
	first,err:=p.RewriteJSON(input)
	if err!=nil {t.Fatal(err)}
	second,err:=p.RewriteJSON([]byte("{\"items\":[{\"cover_url\":\"https://cdn.example/a.jpg?token=two\",\"title\":\"A\"}]}"))
	if err!=nil {t.Fatal(err)}
	var a,b map[string]any
	_ = json.Unmarshal(first,&a)
	_ = json.Unmarshal(second,&b)
	urlA:=a["items"].([]any)[0].(map[string]any)["cover_url"].(string)
	urlB:=b["items"].([]any)[0].(map[string]any)["cover_url"].(string)
	if urlA!=urlB || !strings.HasPrefix(urlA,"/api/v1/images/") {t.Fatalf("unstable cover URL %q %q",urlA,urlB)}
}

func TestServeHTTPCachesAndRevalidatesFromRAM(t *testing.T) {
	var calls int
	upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		calls++
		w.Header().Set("Content-Type","image/jpeg")
		_,_=w.Write([]byte("jpeg-test-content"))
	}))
	defer upstream.Close()
	p:=New()
	p.client=upstream.Client()
	p.validate=func(*url.URL)error{return nil}
	tokenURL:=p.Register("https://cdn.example/poster.jpg?token=one")
	u,_:=url.Parse(tokenURL)
	token:=strings.TrimPrefix(u.Path,"/api/v1/images/")
	item:=p.byToken[token]
	p.mu.Lock()
	item.sourceURL=upstream.URL
	p.mu.Unlock()

	for i:=0;i<2;i++ {
		req:=httptest.NewRequest(http.MethodGet,tokenURL,nil)
		req.SetPathValue("token",token)
		rec:=httptest.NewRecorder()
		p.ServeHTTP(rec,req)
		if rec.Code!=http.StatusOK || rec.Body.String()!="jpeg-test-content" {t.Fatalf("response %d: %s",rec.Code,rec.Body.String())}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=604800") {
			t.Fatalf("image browser cache lifetime was not extended: %q", rec.Header().Get("Cache-Control"))
		}
	}
	if calls!=1 {t.Fatalf("upstream calls=%d, want 1",calls)}
}

func TestValidateURL(t *testing.T) {
	for _,raw:=range []string{"file:///etc/passwd","http://127.0.0.1/image","https://192.168.1.2/a","https://example.com:8443/a"} {
		u,_:=url.Parse(raw)
		if validateURL(u)==nil {t.Errorf("accepted %s",raw)}
	}
	u,_:=url.Parse("https://cdn.example.com/poster.jpg")
	if err:=validateURL(u);err!=nil {t.Fatal(err)}
}

func TestRegisterConcurrent(t *testing.T) {
	p:=New()
	var wg sync.WaitGroup
	results:=make(chan string,20)
	for i:=0;i<20;i++ {
		wg.Add(1)
		go func(i int){defer wg.Done();results<-p.Register("https://cdn.example/a.jpg?sig="+string(rune('a'+i)))}(i)
	}
	wg.Wait()
	close(results)
	for value:=range results {if value=="" {t.Fatal("empty registration")}}
}

func TestRewriteJSONPreservesLargeIntegers(t *testing.T) {
	p:=New()
	input:=[]byte(`{"play_count":9007199254740993,"cover_url":"https://cdn.example/a.jpg?token=one","nested":{"count":9223372036854775807}}`)
	got,err:=p.RewriteJSON(input)
	if err!=nil {t.Fatal(err)}
	text:=string(got)
	if !strings.Contains(text,"9007199254740993") || !strings.Contains(text,"9223372036854775807") {
		t.Fatalf("large integer changed during rewrite: %s",text)
	}
}


func TestServeHTTPFollowsValidatedCDNRedirects(t *testing.T) {
	var finalCalls int
	final:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		finalCalls++
		w.Header().Set("Content-Type","image/jpeg")
		_,_=w.Write([]byte("jpeg-after-redirect"))
	}))
	defer final.Close()
	redirect:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		http.Redirect(w,r,final.URL+"/poster.jpg",http.StatusFound)
	}))
	defer redirect.Close()

	p:=New()
	transport,ok:=p.client.Transport.(*http.Transport)
	if !ok {t.Fatal("unexpected HTTP transport")}
	// Local test servers stand in for public CDN hosts; production still uses
	// safeDialContext and validateURL on every redirect target.
	transport.DialContext=(&net.Dialer{}).DialContext
	p.validate=func(*url.URL)error{return nil}
	tokenURL:=p.Register("https://cdn.example/poster.jpg?signature=one")
	token:=strings.TrimPrefix(tokenURL,"/api/v1/images/")
	p.mu.Lock()
	p.byToken[token].sourceURL=redirect.URL
	p.mu.Unlock()

	req:=httptest.NewRequest(http.MethodGet,tokenURL,nil)
	req.SetPathValue("token",token)
	rec:=httptest.NewRecorder()
	p.ServeHTTP(rec,req)
	if rec.Code!=http.StatusOK || rec.Body.String()!="jpeg-after-redirect" {
		t.Fatalf("redirect response status=%d body=%q",rec.Code,rec.Body.String())
	}
	if finalCalls!=1 {t.Fatalf("final CDN calls=%d, want 1",finalCalls)}
}


type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func imageResponse(req *http.Request, status int, contentType string, body []byte) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", contentType)
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(string(body))),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func TestOfficialJPEGVariantURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{
			name: "known byteimg HEIC object",
			raw:  "https://p3-fqnovelpic.byteimg.com/novel-pic/2026/cover.heic~tplv-w640.heic?signature=discarded",
			want: "https://p3-novel.byteimg.com/novel-pic/2026/cover.heic~tplv-shrink:640:0.image",
			ok:   true,
		},
		{
			name: "known fqnovelpic host",
			raw:  "https://static.fqnovelpic.com/novel-pic/cover.heic~tplv-existing.heic",
			want: "https://p3-novel.byteimg.com/novel-pic/cover.heic~tplv-shrink:640:0.image",
			ok:   true,
		},
		{
			name: "ordinary jpeg",
			raw:  "https://p3-novel.byteimg.com/novel-pic/cover.jpg~tplv-cover.jpg",
			ok:   false,
		},
		{
			name: "untrusted domain",
			raw:  "https://notbyteimg.com/novel-pic/cover.heic~tplv-cover.heic",
			ok:   false,
		},
		{
			name: "different path",
			raw:  "https://p3-novel.byteimg.com/other/cover.heic~tplv-cover.heic",
			ok:   false,
		},
		{
			name: "no transformation marker",
			raw:  "https://p3-novel.byteimg.com/novel-pic/cover.heic",
			ok:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := officialJPEGVariant(test.raw)
			if ok != test.ok {
				t.Fatalf("ok=%t, want %t; URL=%q", ok, test.ok, got)
			}
			if got != test.want {
				t.Fatalf("URL=%q, want %q", got, test.want)
			}
		})
	}
}

func TestServeHTTPUsesOfficialJPEGVariant(t *testing.T) {
	source := "https://p3-fqnovelpic.byteimg.com/novel-pic/cover.heic~tplv-existing.heic?signature=one"
	p := New()
	tokenURL := p.Register(source)
	if tokenURL == "" {
		t.Fatal("Register rejected eligible image URL")
	}
	token := strings.TrimPrefix(tokenURL, "/api/v1/images/")
	p.validate = func(*url.URL) error { return nil }

	var requested []string
	jpeg := []byte{0xff, 0xd8, 0xff, 0xd9}
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.String())
		if req.URL.Host != "p3-novel.byteimg.com" {
			t.Fatalf("expected the official JPEG variant first, requested %s", req.URL)
		}
		return imageResponse(req, http.StatusOK, "image/jpeg", jpeg), nil
	})}

	req := httptest.NewRequest(http.MethodGet, tokenURL, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	wantURL := "https://p3-novel.byteimg.com/novel-pic/cover.heic~tplv-shrink:640:0.image"
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type=%q, want image/jpeg", got)
	}
	if rec.Body.String() != string(jpeg) {
		t.Fatalf("body=%v, want JPEG bytes %v", rec.Body.Bytes(), jpeg)
	}
	if len(requested) != 1 || requested[0] != wantURL {
		t.Fatalf("requested URLs=%v, want [%q]", requested, wantURL)
	}
}

func TestServeHTTPFallsBackWhenOfficialVariantIsNotJPEG(t *testing.T) {
	source := "https://p3-fqnovelpic.byteimg.com/novel-pic/cover.heic~tplv-existing.heic?signature=one"
	p := New()
	tokenURL := p.Register(source)
	if tokenURL == "" {
		t.Fatal("Register rejected eligible image URL")
	}
	token := strings.TrimPrefix(tokenURL, "/api/v1/images/")
	p.validate = func(*url.URL) error { return nil }

	var requested []string
	originalBytes := []byte("original-heic-bytes")
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.String())
		if req.URL.Host == "p3-novel.byteimg.com" {
			return imageResponse(req, http.StatusOK, "image/jpeg", []byte("not-a-jpeg")), nil
		}
		return imageResponse(req, http.StatusOK, "image/heic", originalBytes), nil
	})}

	req := httptest.NewRequest(http.MethodGet, tokenURL, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/heic" {
		t.Fatalf("fallback Content-Type=%q, want image/heic", got)
	}
	if rec.Body.String() != string(originalBytes) {
		t.Fatalf("fallback body=%q, want %q", rec.Body.String(), originalBytes)
	}
	if len(requested) != 2 ||
		!strings.Contains(requested[0], "p3-novel.byteimg.com") ||
		!strings.Contains(requested[1], "p3-fqnovelpic.byteimg.com") {
		t.Fatalf("expected variant then original fallback, got %v", requested)
	}
}

func TestImageFetchQueueIsBoundedAndCancelable(t *testing.T) {
	p := &Proxy{
		fetchSlots: make(chan struct{}, 1),
		fetchQueue: make(chan struct{}, 1),
	}
	release, err := p.acquireFetchSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := p.acquireFetchSlot(ctx)
		waiting <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(p.fetchQueue) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(p.fetchQueue) != 1 {
		t.Fatal("second image request did not enter the bounded wait queue")
	}
	if _, err := p.acquireFetchSlot(context.Background()); !errors.Is(err, errImageQueueFull) {
		t.Fatalf("overflow error = %v, want image queue full", err)
	}
	if err := <-waiting; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled queue waiter error = %v", err)
	}
	if len(p.fetchQueue) != 0 {
		t.Fatal("timed-out image request left a queue token behind")
	}
}

func TestServeHTTPFallsBackToStaleRAMImageOnCDNError(t *testing.T) {
	p := New()
	p.validate = func(*url.URL) error { return nil }
	tokenURL := p.Register("https://cdn.example/poster.jpg")
	token := strings.TrimPrefix(tokenURL, "/api/v1/images/")
	item := p.byToken[token]
	stale := []byte("previously-cached-image")
	p.cacheRAM(item, stale, "image/jpeg")
	p.mu.Lock()
	item.cachedAt = time.Now().Add(-cacheLifetime - time.Minute)
	p.mu.Unlock()
	p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("simulated CDN outage")
	})}

	req := httptest.NewRequest(http.MethodGet, tokenURL, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(stale) {
		t.Fatalf("stale response status=%d body=%q", rec.Code, rec.Body.String())
	}
}


func TestServeHTTPPromotesPersistentImageBeforeCDNRefresh(t *testing.T) {
	p := New()
	p.validate = func(*url.URL) error { return nil }
	disk, err := newDiskCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	p.disk = disk
	tokenURL := p.Register("https://cdn.example/poster.jpg")
	token := strings.TrimPrefix(tokenURL, "/api/v1/images/")
	item := p.byToken[token]

	stale := []byte("old-ram-image")
	p.cacheRAM(item, stale, "image/jpeg")
	p.mu.Lock()
	item.cachedAt = time.Now().Add(-cacheLifetime - time.Minute)
	p.mu.Unlock()

	persistent := []byte("disk-cached-image")
	if err := disk.store(item.sourceKey, "image/jpeg", persistent); err != nil {
		t.Fatal(err)
	}
	p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("CDN should not be contacted while the persistent image is valid")
		return nil, errors.New("unexpected CDN request")
	})}

	req := httptest.NewRequest(http.MethodGet, tokenURL, nil)
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(persistent) {
		t.Fatalf("persistent cache response status=%d body=%q", rec.Code, rec.Body.String())
	}
}
