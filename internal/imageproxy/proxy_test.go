package imageproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
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
