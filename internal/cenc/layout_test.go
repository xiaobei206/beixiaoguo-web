package cenc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"math/rand"
	"testing"
)

type infoKind int

const (
	infoSENC infoKind = iota
	infoSENCSubsamples
	infoSAIO
)

type fixture struct {
	key   []byte
	file  []byte
	clear []byte
}

func makeFixture(kind infoKind, moovLast bool, sampleCount int) fixture {
	rng := rand.New(rand.NewSource(42))
	key := make([]byte, 16)
	for i := range key { key[i] = byte(i*7+3) }
	sizes := make([]int, sampleCount)
	plain := make([][]byte, sampleCount)
	ivs := make([][]byte, sampleCount)
	subs := make([][]subsample, sampleCount)
	for i := 0; i < sampleCount; i++ {
		sizes[i] = 3000 + rng.Intn(9000)
		plain[i] = make([]byte, sizes[i])
		_, _ = rng.Read(plain[i])
		ivs[i] = make([]byte, 8)
		_, _ = rng.Read(ivs[i])
		if kind == infoSENCSubsamples {
			subs[i] = []subsample{{clear: 100, encrypted: 1000}, {clear: 37, encrypted: int64(sizes[i]-100-1000-37-50)}, {clear: 50, encrypted: 0}}
		}
	}
	// A non-nil, zero-entry subsample table means this sample is entirely clear.
	if kind == infoSENCSubsamples && sampleCount > 0 {
		subs[0] = []subsample{}
	}
	encrypted := make([][]byte, sampleCount)
	for i := range plain {
		encrypted[i] = encryptSample(plain[i], ivs[i], subs[i], key, kind == infoSENCSubsamples)
	}

	var encryptedFile, clearFile []byte
	var payloadAt int64
	for pass := 0; pass < 2; pass++ {
		var aux [][]byte
		if kind == infoSAIO { aux = ivs }
		auxBytes := concat(aux)
		auxAt := payloadAt
		samplesAt := payloadAt + int64(len(auxBytes))
		build := func(makeClear bool) []byte {
			ftyp := makeBox("ftyp", append(append(ascii("isom"), u32(0)...), append(ascii("isom"), ascii("iso2")...)...))
			moov := makeMoov(makeClear, kind, sizes, ivs, subs, samplesAt, auxAt)
			payload := auxBytes
			if makeClear { payload = append(append([]byte(nil), auxBytes...), concat(plain)...)} else { payload = append(append([]byte(nil), auxBytes...), concat(encrypted)...)}
			mdat := makeBox("mdat", payload)
			if moovLast { return concat([][]byte{ftyp, mdat, moov}) }
			return concat([][]byte{ftyp, moov, mdat})
		}
		encryptedFile = build(false)
		clearFile = build(true)
		if pass == 0 {
			ftypSize := int64(8 + 4 + 4 + 8)
			mdatPayload := int64(len(auxBytes) + len(concat(encrypted)))
			moovSize := int64(len(encryptedFile)) - ftypSize - 8 - mdatPayload
			if moovLast { payloadAt = ftypSize + 8 } else { payloadAt = ftypSize + moovSize + 8 }
		}
	}
	return fixture{key:key,file:encryptedFile,clear:clearFile}
}

func encryptSample(plain, rawIV []byte, subs []subsample, key []byte, hasSubsampleTable bool) []byte {
	out := append([]byte(nil), plain...)
	var ranges [][2]int
	if !hasSubsampleTable {
		ranges = append(ranges, [2]int{0,len(plain)})
	} else {
		pos := 0
		for _, sub := range subs {
			pos += int(sub.clear)
			if sub.encrypted > 0 {
				ranges = append(ranges,[2]int{pos,int(sub.encrypted)})
			}
			pos += int(sub.encrypted)
		}
	}
	var streamPlain []byte
	for _, r := range ranges {
		streamPlain = append(streamPlain, plain[r[0]:r[0]+r[1]]...)
	}
	var iv16 [16]byte
	copy(iv16[:8], rawIV)
	block, _ := aes.NewCipher(key)
	ctr := cipher.NewCTR(block, iv16[:])
	streamCipher := make([]byte,len(streamPlain))
	ctr.XORKeyStream(streamCipher,streamPlain)
	at:=0
	for _,r:=range ranges {
		copy(out[r[0]:r[0]+r[1]],streamCipher[at:at+r[1]])
		at+=r[1]
	}
	return out
}

func makeMoov(clear bool, kind infoKind, sizes []int, ivs [][]byte, subs [][]subsample, chunkAt, auxAt int64) []byte {
	n := len(sizes)
	tenc := fullBox("tenc", append([]byte{0,0,1,8}, bytesRepeat(0xab,16)...))
	sinf := makeBox("sinf", concat([][]byte{
		makeBox("frma",ascii("hvc1")),
		fullBox("schm",append(ascii("cenc"),u32(0x10000)...)),
		makeBox("schi",tenc),
	}))
	if clear {
		sinf = replaceType(sinf,"free")
	}
	entryType := "encv"
	if clear { entryType = "hvc1" }
	entry := makeBox(entryType,append(bytesRepeat(0,78),append(makeBox("hvcC",[]byte{1,2,3,4}),sinf...)...))
	stsd := fullBox("stsd",append(u32(1),entry...))
	stszBody := append(u32(0),u32(int64(n))...)
	for _,size := range sizes { stszBody=append(stszBody,u32(int64(size))...) }
	stsz := fullBox("stsz",stszBody)
	first:=5
	stscBody:=append(u32(2),u32(1)...)
	stscBody=append(stscBody,u32(int64(first))...)
	stscBody=append(stscBody,u32(1)...)
	stscBody=append(stscBody,u32(2)...)
	stscBody=append(stscBody,u32(int64(n-first))...)
	stscBody=append(stscBody,u32(1)...)
	stsc:=fullBox("stsc",stscBody)
	second:=chunkAt
	for _,s:=range sizes[:first] { second+=int64(s) }
	stcoBody:=append(u32(2),u32(chunkAt)...)
	stcoBody=append(stcoBody,u32(second)...)
	stco:=fullBox("stco",stcoBody)

	var protection []byte
	switch kind {
	case infoSENC,infoSENCSubsamples:
		body:=append([]byte(nil),u32(int64(n))...)
		for i:=0;i<n;i++ {
			body=append(body,ivs[i]...)
			if kind==infoSENCSubsamples {
				body=append(body,u16(len(subs[i]))...)
				for _,sub:=range subs[i] {
					body=append(body,u16(int(sub.clear))...)
					body=append(body,u32(sub.encrypted)...)
				}
			}
		}
		name:="senc"
		if clear { name="free" }
		flags:=0
		if kind==infoSENCSubsamples { flags=2 }
		protection=fullBoxFlags(name,flags,body)
	case infoSAIO:
		saiz:=fullBox("saiz",append([]byte{8},u32(int64(n))...))
		saio:=fullBox("saio",append(u32(1),u32(auxAt)...))
		if clear { saiz=replaceType(saiz,"free"); saio=replaceType(saio,"free") }
		protection=append(saiz,saio...)
	}
	stbl:=makeBox("stbl",concat([][]byte{stsd,stsz,stsc,makeBox("stco",stco[8:]),protection}))
	trak:=makeBox("trak",makeBox("mdia",makeBox("minf",stbl)))
	pssh:=fullBox("pssh",bytesRepeat(0x11,20))
	if clear { pssh=replaceType(pssh,"free") }
	return makeBox("moov",append(trak,pssh...))
}

func makeBox(name string, body []byte) []byte {
	return append(append(u32(int64(8+len(body))),ascii(name)...),body...)
}
func fullBox(name string,body []byte) []byte { return fullBoxFlags(name,0,body) }
func fullBoxFlags(name string,flags int,body []byte) []byte {
	return makeBox(name,append([]byte{0,byte(flags>>16),byte(flags>>8),byte(flags)},body...))
}
func replaceType(b []byte,name string) []byte { out:=append([]byte(nil),b...); copy(out[4:8],name); return out }
func ascii(s string) []byte { return []byte(s) }
func u16(v int) []byte { return []byte{byte(v>>8),byte(v)} }
func u32(v int64) []byte { return []byte{byte(v>>24),byte(v>>16),byte(v>>8),byte(v)} }
func bytesRepeat(v byte,n int) []byte { b:=make([]byte,n); for i:=range b {b[i]=v}; return b }
func concat(parts [][]byte) []byte { var out []byte; for _,p:=range parts {out=append(out,p...)}; return out }

func TestDecryptWholeFile(t *testing.T) {
	for _,kind:=range []infoKind{infoSENC,infoSENCSubsamples,infoSAIO} {
		t.Run(fmt.Sprint(kind),func(t *testing.T){
			f:=makeFixture(kind,false,12)
			layout,err:=Open(int64(len(f.file)),f.key,func(off,n int64)([]byte,error){
				return append([]byte(nil),f.file[off:off+n]...),nil
			})
			if err!=nil {t.Fatal(err)}
			if !layout.IsProtected(){t.Fatal("expected protected layout")}
			got:=append([]byte(nil),f.file...)
			layout.Decrypt(got,0)
			if !bytes.Equal(got,f.clear){t.Fatal("decrypted file differs from clear fixture")}
		})
	}
}

func TestDecryptArbitraryRanges(t *testing.T) {
	for _,kind:=range []infoKind{infoSENC,infoSENCSubsamples,infoSAIO} {
		t.Run(fmt.Sprint(kind),func(t *testing.T){
			f:=makeFixture(kind,false,12)
			layout,err:=Open(int64(len(f.file)),f.key,func(off,n int64)([]byte,error){
				return append([]byte(nil),f.file[off:off+n]...),nil
			})
			if err!=nil {t.Fatal(err)}
			rng:=rand.New(rand.NewSource(7))
			for round:=0;round<10;round++ {
				got:=append([]byte(nil),f.file...)
				for off:=0;off<len(got); {
					n:=1+rng.Intn(9000)
					if off+n>len(got){n=len(got)-off}
					layout.Decrypt(got[off:off+n],int64(off))
					off+=n
				}
				if !bytes.Equal(got,f.clear){t.Fatalf("round %d differs from whole clear file",round)}
			}
		})
	}
}

func TestFindMoovAfterLargeMdat(t *testing.T) {
	f:=makeFixture(infoSENC,true,12)
	if len(f.file)<=64<<10 {t.Fatalf("fixture too small: %d",len(f.file))}
	layout,err:=Open(int64(len(f.file)),f.key,func(off,n int64)([]byte,error){
		return append([]byte(nil),f.file[off:off+n]...),nil
	})
	if err!=nil {t.Fatal(err)}
	got:=append([]byte(nil),f.file...)
	layout.Decrypt(got,0)
	if !bytes.Equal(got,f.clear){t.Fatal("moov-at-end decrypt mismatch")}
}

func TestOpenRejectsInvalidKey(t *testing.T) {
	f:=makeFixture(infoSENC,false,3)
	_,err:=Open(int64(len(f.file)),[]byte{1,2,3},func(off,n int64)([]byte,error){
		return append([]byte(nil),f.file[off:off+n]...),nil
	})
	if err==nil {t.Fatal("expected invalid-key error")}
}

func TestDecryptConcurrentRanges(t *testing.T) {
	f:=makeFixture(infoSENCSubsamples,false,12)
	layout,err:=Open(int64(len(f.file)),f.key,func(off,n int64)([]byte,error){
		return append([]byte(nil),f.file[off:off+n]...),nil
	})
	if err!=nil {t.Fatal(err)}
	got:=append([]byte(nil),f.file...)
	// Each goroutine owns an independent byte range buffer.
	type result struct { off int; data []byte }
	ranges:=make([]result,0)
	for off:=0;off<len(got); {
		n:=min(4096,len(got)-off)
		ranges=append(ranges,result{off:off,data:append([]byte(nil),got[off:off+n]...)})
		off+=n
	}
	done:=make(chan result,len(ranges))
	for _,r:=range ranges {
		go func(r result){layout.Decrypt(r.data,int64(r.off));done<-r}(r)
	}
	for range ranges {
		r:=<-done
		copy(got[r.off:r.off+len(r.data)],r.data)
	}
	if !bytes.Equal(got,f.clear){t.Fatal("concurrent range decrypt mismatch")}
}

func TestDecodeUtilities(t *testing.T) {
	got:=be32([]byte{0,0,1,2},0)
	if got!=258 {t.Fatalf("be32 = %d",got)}
	if be16([]byte{0x12,0x34},0)!=0x1234 {t.Fatal("be16 mismatch")}
	var v [8]byte
	binary.BigEndian.PutUint64(v[:],0x0102030405060708)
	if binary.BigEndian.Uint64(v[:])!=0x0102030405060708 {t.Fatal("binary helper mismatch")}
}
