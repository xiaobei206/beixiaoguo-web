package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		name string
		header string
		size int64
		want byteRange
		partial bool
		wantErr error
	}{
		{name:"whole",header:"",size:100,want:byteRange{0,99}},
		{name:"closed",header:"bytes=10-19",size:100,want:byteRange{10,19},partial:true},
		{name:"open-ended",header:"bytes=90-",size:100,want:byteRange{90,99},partial:true},
		{name:"suffix",header:"bytes=-10",size:100,want:byteRange{90,99},partial:true},
		{name:"suffix-larger-than-file",header:"bytes=-200",size:100,want:byteRange{0,99},partial:true},
		{name:"clip-end",header:"bytes=90-200",size:100,want:byteRange{90,99},partial:true},
		{name:"start-outside",header:"bytes=100-",size:100,wantErr:ErrRangeUnsatisfied},
		{name:"zero-suffix",header:"bytes=-0",size:100,wantErr:ErrInvalidRange},
		{name:"multiple",header:"bytes=0-1,4-5",size:100,wantErr:ErrInvalidRange},
		{name:"wrong-unit",header:"items=0-1",size:100,wantErr:ErrInvalidRange},
		{name:"bad-order",header:"bytes=20-10",size:100,wantErr:ErrInvalidRange},
	}
	for _, tt := range tests {
		t.Run(tt.name,func(t *testing.T) {
			got,partial,err:=parseRange(tt.header,tt.size)
			if tt.wantErr!=nil {
				if err!=tt.wantErr { t.Fatalf("error=%v want=%v",err,tt.wantErr) }
				return
			}
			if err!=nil { t.Fatal(err) }
			if got!=tt.want || partial!=tt.partial { t.Fatalf("got=%+v partial=%t want=%+v partial=%t",got,partial,tt.want,tt.partial) }
		})
	}
}

func TestParseContentRange(t *testing.T) {
	start,end,total,err:=parseContentRange("bytes 10-19/100")
	if err!=nil { t.Fatal(err) }
	if start!=10 || end!=19 || total!=100 { t.Fatalf("got %d-%d/%d",start,end,total) }
	for _,bad:=range []string{"","bytes */100","bytes 3-2/4","bytes 0-4/4","items 0-1/2"} {
		if _,_,_,err:=parseContentRange(bad); err==nil { t.Errorf("accepted %q",bad) }
	}
}

func TestServeHTTPStreamsRequestedRange(t *testing.T) {
	payload:=[]byte("0123456789abcdefghijklmnopqrstuvwxyz")
	upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		var start,end int
		if _,err:=fmt.Sscanf(r.Header.Get("Range"),"bytes=%d-%d",&start,&end); err!=nil {
			http.Error(w,"missing range",http.StatusBadRequest); return
		}
		if start<0 || end>=len(payload) || start>end {
			w.Header().Set("Content-Range",fmt.Sprintf("bytes */%d",len(payload)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable); return
		}
		w.Header().Set("Content-Range",fmt.Sprintf("bytes %d-%d/%d",start,end,len(payload)))
		w.Header().Set("Content-Length",strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_,_=w.Write(payload[start:end+1])
	}))
	defer upstream.Close()

	service:=&Service{
		httpClient:upstream.Client(),
		logger:log.New(io.Discard,"",0),
		validateURL:func(*url.URL)error{return nil},
		sessions:map[string]*playbackSession{
			"test-token":{episodeID:"episode-test",sources:[]string{upstream.URL},expiresAt:time.Now().Add(time.Minute),prepared:true,size:int64(len(payload))},
		},
		streams:make(chan struct{},2),
	}
	req:=httptest.NewRequest(http.MethodGet,"/api/v1/media/test-token",nil)
	req.SetPathValue("token","test-token")
	req.Header.Set("Range","bytes=5-14")
	rec:=httptest.NewRecorder()
	service.ServeHTTP(rec,req)
	if rec.Code!=http.StatusPartialContent { t.Fatalf("status=%d body=%s",rec.Code,rec.Body.String()) }
	if got,want:=rec.Header().Get("Content-Range"),"bytes 5-14/36"; got!=want { t.Fatalf("Content-Range=%q want=%q",got,want) }
	if got,want:=rec.Body.String(),string(payload[5:15]); got!=want { t.Fatalf("body=%q want=%q",got,want) }
}

func TestServeHTTPRejectsMultipleRanges(t *testing.T) {
	service:=&Service{
		httpClient:http.DefaultClient,
		logger:log.New(io.Discard,"",0),
		validateURL:func(*url.URL)error{return nil},
		sessions:map[string]*playbackSession{
			"test-token":{episodeID:"x",sources:[]string{"https://example.invalid/video"},expiresAt:time.Now().Add(time.Minute),prepared:true,size:10},
		},
		streams:make(chan struct{},2),
	}
	req:=httptest.NewRequest(http.MethodGet,"/api/v1/media/test-token",nil)
	req.SetPathValue("token","test-token")
	req.Header.Set("Range","bytes=0-1,4-5")
	rec:=httptest.NewRecorder()
	service.ServeHTTP(rec,req)
	if rec.Code!=http.StatusRequestedRangeNotSatisfiable { t.Fatalf("status=%d",rec.Code) }
	if !strings.Contains(rec.Header().Get("Content-Range"),"*/10") { t.Fatalf("Content-Range=%q",rec.Header().Get("Content-Range")) }
}

func TestFetchBytesUsesExactRange(t *testing.T) {
	payload:=bytes.Repeat([]byte("z"),100)
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		if r.Header.Get("Range")!="bytes=10-29" { t.Errorf("Range=%q",r.Header.Get("Range")) }
		w.Header().Set("Content-Range","bytes 10-29/100")
		w.WriteHeader(http.StatusPartialContent)
		_,_=w.Write(payload[10:30])
	}))
	defer server.Close()
	service:=&Service{
		httpClient:server.Client(),
		logger:log.New(io.Discard,"",0),
		validateURL:func(*url.URL)error{return nil},
	}
	session:=&playbackSession{sources:[]string{server.URL}}
	data,total,err:=service.fetchBytes(context.Background(),session,10,20)
	if err!=nil { t.Fatal(err) }
	if total!=100 || !bytes.Equal(data,payload[10:30]) { t.Fatalf("unexpected body length=%d total=%d",len(data),total) }
}
