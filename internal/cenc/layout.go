// Package cenc reads and decrypts CENC-protected progressive MP4 files in place.
//
// Decryption does not change byte positions. Protected sample-entry and
// auxiliary boxes are renamed to same-length clear boxes, and encrypted
// sample ranges are AES-CTR decrypted on demand. This makes arbitrary HTTP
// byte ranges seekable without downloading a complete episode.
//
// The parser is a Go port of the project's existing MIT-licensed Android
// CENC implementation. Keep behavior aligned with the Android client.
package cenc

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

const (
	headerProbeSize = int64(64 << 10)
	maxMoovSize     = int64(32 << 20)
	maxSamples      = 1_000_000
	maxChunks       = 1_000_000
	maxAuxSize      = int64(32 << 20)
)

var (
	ErrInvalidKey = errors.New("cenc: invalid key size")
	ErrBadMP4     = errors.New("cenc: invalid MP4 layout")
)

type Fetch func(offset, count int64) ([]byte, error)

type Layout struct {
	block      cipher.Block
	moovOffset int64
	moov       []byte
	samples    []sample
}

type sample struct {
	offset     int64
	size       int64
	iv         iv
	subsamples []subsample // nil means the entire sample is encrypted
}

type subsample struct {
	clear     int64
	encrypted int64
}

type iv struct {
	high uint64
	low  uint64
}

func Open(total int64, key []byte, fetch Fetch) (*Layout, error) {
	if total <= 0 || fetch == nil {
		return nil, fmt.Errorf("%w: invalid file size or fetch function", ErrBadMP4)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	offset, moov, err := findMoov(total, fetch)
	if err != nil {
		return nil, err
	}
	return parseLayout(offset, moov, block, fetch)
}

func parseLayout(moovOffset int64, moov []byte, block cipher.Block, fetch Fetch) (*Layout, error) {
	buf := append([]byte(nil), moov...)
	root, ok := parseBox(buf, 0, len(buf))
	if !ok || root.typ != "moov" {
		return nil, fmt.Errorf("%w: not a moov box", ErrBadMP4)
	}

	var samples []sample
	for _, child := range root.children(0) {
		switch child.typ {
		case "trak":
			found, err := parseTrak(child, moovOffset, fetch)
			if err != nil {
				return nil, err
			}
			samples = append(samples, found...)
		case "pssh":
			child.rename("free")
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].offset < samples[j].offset })
	return &Layout{block: block, moovOffset: moovOffset, moov: buf, samples: samples}, nil
}

func (l *Layout) IsProtected() bool {
	return l != nil && len(l.samples) > 0
}

// Decrypt changes buf in place. fileOffset is the absolute offset of buf[0]
// in the encrypted file. It is safe to call concurrently on disjoint buffers.
func (l *Layout) Decrypt(buf []byte, fileOffset int64) {
	if l == nil || len(buf) == 0 || fileOffset < 0 || int64(len(buf)) > math.MaxInt64-fileOffset {
		return
	}
	end := fileOffset + int64(len(buf))

	// Replace the protected metadata boxes with same-size clear boxes.
	moovEnd := l.moovOffset + int64(len(l.moov))
	if fileOffset < moovEnd && end > l.moovOffset {
		a := max64(fileOffset, l.moovOffset)
		b := min64(end, moovEnd)
		copy(buf[a-fileOffset:b-fileOffset], l.moov[a-l.moovOffset:b-l.moovOffset])
	}

	// Decrypt only samples that intersect this byte range.
	i := l.firstSampleEndingAfter(fileOffset)
	for ; i < len(l.samples) && l.samples[i].offset < end; i++ {
		s := l.samples[i]
		if s.subsamples == nil {
			a := max64(fileOffset, s.offset)
			b := min64(end, s.offset+s.size)
			l.xor(buf[a-fileOffset:b-fileOffset], s.iv, a-s.offset)
			continue
		}
		pos := s.offset
		streamOffset := int64(0)
		for _, sub := range s.subsamples {
			pos += sub.clear
			encEnd := pos + sub.encrypted
			a := max64(fileOffset, pos)
			b := min64(end, encEnd)
			if a < b {
				l.xor(buf[a-fileOffset:b-fileOffset], s.iv, streamOffset+(a-pos))
			}
			streamOffset += sub.encrypted
			pos = encEnd
		}
	}
}

func (l *Layout) firstSampleEndingAfter(offset int64) int {
	return sort.Search(len(l.samples), func(i int) bool {
		return l.samples[i].offset+l.samples[i].size > offset
	})
}

// xor applies AES(counter) to data. The CENC counter's high 64 bits are the
// IV high half; the low 64 bits count AES blocks and wrap as uint64.
func (l *Layout) xor(data []byte, initial iv, streamOffset int64) {
	if len(data) == 0 || streamOffset < 0 {
		return
	}
	firstBlock := uint64(streamOffset / aes.BlockSize)
	skip := int(streamOffset % aes.BlockSize)
	var counter [aes.BlockSize]byte
	var keyStream [aes.BlockSize]byte
	binary.BigEndian.PutUint64(counter[:8], initial.high)
	for consumed := 0; consumed < len(data); {
		binary.BigEndian.PutUint64(counter[8:], initial.low+firstBlock)
		l.block.Encrypt(keyStream[:], counter[:])
		n := minInt(len(data)-consumed, aes.BlockSize-skip)
		for i := 0; i < n; i++ {
			data[consumed+i] ^= keyStream[skip+i]
		}
		consumed += n
		firstBlock++
		skip = 0
	}
}

func findMoov(total int64, fetch Fetch) (int64, []byte, error) {
	headCount := min64(headerProbeSize, total)
	head, err := fetch(0, headCount)
	if err != nil {
		return 0, nil, fmt.Errorf("cenc: read MP4 header: %w", err)
	}
	if int64(len(head)) < headCount {
		// A short initial range is acceptable only if it still contains the
		// boxes we need; later header reads fall back to fetch.
		headCount = int64(len(head))
	}

	for offset := int64(0); offset+8 <= total; {
		var header []byte
		if offset+16 <= int64(len(head)) {
			header = head[offset : offset+16]
		} else if offset+8 <= int64(len(head)) {
			header = head[offset : offset+8]
		} else {
			n := min64(16, total-offset)
			header, err = fetch(offset, n)
			if err != nil {
				return 0, nil, fmt.Errorf("cenc: read box header at %d: %w", offset, err)
			}
		}
		if len(header) < 8 {
			return 0, nil, fmt.Errorf("%w: short box header at %d", ErrBadMP4, offset)
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		typ := fourCC(header[4:8])
		switch size {
		case 1:
			if len(header) < 16 {
				header, err = fetch(offset, min64(16, total-offset))
				if err != nil || len(header) < 16 {
					return 0, nil, fmt.Errorf("%w: short large-size box header at %d", ErrBadMP4, offset)
				}
			}
			u := binary.BigEndian.Uint64(header[8:16])
			if u > math.MaxInt64 {
				return 0, nil, fmt.Errorf("%w: box too large at %d", ErrBadMP4, offset)
			}
			size = int64(u)
		case 0:
			size = total - offset
		}
		if size < 8 || size > total-offset {
			return 0, nil, fmt.Errorf("%w: bad box %q at %d", ErrBadMP4, typ, offset)
		}
		if typ == "moov" {
			if size > maxMoovSize {
				return 0, nil, fmt.Errorf("%w: moov exceeds %d bytes", ErrBadMP4, maxMoovSize)
			}
			if offset+size <= int64(len(head)) {
				return offset, append([]byte(nil), head[offset:offset+size]...), nil
			}
			moov, err := fetch(offset, size)
			if err != nil {
				return 0, nil, fmt.Errorf("cenc: read moov box: %w", err)
			}
			if int64(len(moov)) != size {
				return 0, nil, fmt.Errorf("%w: short moov box", ErrBadMP4)
			}
			return offset, moov, nil
		}
		offset += size
	}
	return 0, nil, fmt.Errorf("%w: no moov box", ErrBadMP4)
}

type track struct {
	ivSize      int
	constIV     []byte
	isProtected bool
	sizes       []int64
	offsets     []int64
	ivs         []iv
	subs        [][]subsample
	senc        bool
	saizDefault int
	saizSizes   []byte
	saioOffsets []int64
	stsc        []int64
}

func parseTrak(trakBox *box, moovOffset int64, fetch Fetch) ([]sample, error) {
	stbl := trakBox.find("mdia", "minf", "stbl")
	if stbl == nil {
		return nil, nil
	}
	stsd := stbl.find("stsd")
	if stsd == nil || stsd.bodyCount() < 8 {
		return nil, nil
	}
	entry, ok := parseBox(stsd.buf, stsd.bodyAt+8, stsd.end)
	if !ok {
		return nil, nil
	}
	var childStart int
	switch entry.typ {
	case "encv":
		childStart = 78
	case "enca":
		childStart = 28
		if entry.bodyCount() >= 10 {
			switch be16(entry.buf, entry.bodyAt+8) {
			case 1:
				childStart += 16
			case 2:
				childStart += 36
			}
		}
	default:
		return nil, nil
	}

	t := track{}
	format := ""
	for _, child := range entry.children(childStart) {
		if child.typ != "sinf" {
			continue
		}
		if frma := child.find("frma"); frma != nil && frma.bodyCount() >= 4 {
			format = fourCC(frma.buf[frma.bodyAt : frma.bodyAt+4])
		}
		if schm := child.find("schm"); schm != nil && schm.bodyCount() >= 8 {
			scheme := fourCC(schm.buf[schm.bodyAt+4 : schm.bodyAt+8])
			if scheme != "cenc" {
				return nil, fmt.Errorf("%w: scheme %q is not supported", ErrBadMP4, scheme)
			}
		}
		tenc := child.find("schi", "tenc")
		if tenc == nil || tenc.bodyCount() < 24 {
			return nil, fmt.Errorf("%w: no tenc box", ErrBadMP4)
		}
		p := tenc.bodyAt
		t.isProtected = tenc.buf[p+6] != 0
		t.ivSize = int(tenc.buf[p+7])
		if t.isProtected && t.ivSize == 0 && tenc.bodyCount() >= 25 {
			n := int(tenc.buf[p+24])
			if tenc.bodyCount() >= 25+n {
				t.constIV = append([]byte(nil), tenc.buf[p+25:p+25+n]...)
			}
		}
		child.rename("free")
	}
	if format == "" {
		return nil, fmt.Errorf("%w: no original sample format", ErrBadMP4)
	}
	entry.rename(format)
	if !t.isProtected {
		return nil, nil
	}

	for _, child := range stbl.children(0) {
		p0, count := child.bodyAt, child.bodyCount()
		switch child.typ {
		case "stsz":
			if count < 12 {
				return nil, fmt.Errorf("%w: bad stsz box", ErrBadMP4)
			}
			fixed := be32(child.buf, p0+4)
			n := be32(child.buf, p0+8)
			if n > maxSamples {
				return nil, fmt.Errorf("%w: sample count %d exceeds limit", ErrBadMP4, n)
			}
			t.sizes = make([]int64, int(n))
			for k := range t.sizes {
				switch {
				case fixed != 0:
					t.sizes[k] = fixed
				case 12+4*k+4 <= count:
					t.sizes[k] = be32(child.buf, p0+12+4*k)
				default:
					return nil, fmt.Errorf("%w: short stsz sample table", ErrBadMP4)
				}
			}
		case "stsc":
			if count < 8 {
				return nil, fmt.Errorf("%w: bad stsc box", ErrBadMP4)
			}
			n := be32(child.buf, p0+4)
			if n > maxChunks || 8+12*n > int64(count) {
				return nil, fmt.Errorf("%w: invalid stsc entry count", ErrBadMP4)
			}
			for k := int64(0); k < n; k++ {
				e := p0 + 8 + int(12*k)
				t.stsc = append(t.stsc, be32(child.buf, e), be32(child.buf, e+4))
			}
		case "stco", "co64":
			if count < 8 {
				return nil, fmt.Errorf("%w: bad %s box", ErrBadMP4, child.typ)
			}
			n := be32(child.buf, p0+4)
			width := int64(4)
			if child.typ == "co64" {
				width = 8
			}
			if n > maxChunks || 8+width*n > int64(count) {
				return nil, fmt.Errorf("%w: invalid %s entry count", ErrBadMP4, child.typ)
			}
			t.offsets = make([]int64, 0, int(n))
			for k := int64(0); k < n; k++ {
				e := p0 + 8 + int(k*width)
				if width == 4 {
					t.offsets = append(t.offsets, be32(child.buf, e))
				} else {
					v := binary.BigEndian.Uint64(child.buf[e : e+8])
					if v > math.MaxInt64 {
						return nil, fmt.Errorf("%w: chunk offset overflow", ErrBadMP4)
					}
					t.offsets = append(t.offsets, int64(v))
				}
			}
		case "senc":
			if err := parseSenc(&t, child.buf, p0, count); err != nil {
				return nil, err
			}
			child.rename("free")
		case "saiz":
			if count < 4 {
				return nil, fmt.Errorf("%w: bad saiz box", ErrBadMP4)
			}
			p := 4
			if child.buf[p0+3]&1 != 0 {
				p += 8
			}
			if count < p+5 {
				return nil, fmt.Errorf("%w: short saiz box", ErrBadMP4)
			}
			t.saizDefault = int(child.buf[p0+p])
			n := be32(child.buf, p0+p+1)
			if t.saizDefault == 0 {
				if n > maxSamples || int64(count) < int64(p+5)+n {
					return nil, fmt.Errorf("%w: invalid saiz sample count", ErrBadMP4)
				}
				t.saizSizes = append([]byte(nil), child.buf[p0+p+5:p0+p+5+int(n)]...)
			}
			child.rename("free")
		case "saio":
			if count < 4 {
				return nil, fmt.Errorf("%w: bad saio box", ErrBadMP4)
			}
			p := 4
			if child.buf[p0+3]&1 != 0 {
				p += 8
			}
			if count < p+4 {
				return nil, fmt.Errorf("%w: short saio box", ErrBadMP4)
			}
			n := be32(child.buf, p0+p)
			width := int64(4)
			if child.buf[p0] == 1 {
				width = 8
			}
			if n > maxChunks || int64(count) < int64(p+4)+n*width {
				return nil, fmt.Errorf("%w: invalid saio entry count", ErrBadMP4)
			}
			for k := int64(0); k < n; k++ {
				e := p0 + p + 4 + int(k*width)
				if width == 4 {
					t.saioOffsets = append(t.saioOffsets, be32(child.buf, e))
				} else {
					v := binary.BigEndian.Uint64(child.buf[e : e+8])
					if v > math.MaxInt64 {
						return nil, fmt.Errorf("%w: auxiliary offset overflow", ErrBadMP4)
					}
					t.saioOffsets = append(t.saioOffsets, int64(v))
				}
			}
			child.rename("free")
		}
	}
	if !t.senc {
		if err := readAux(&t, moovOffset, entry.buf, fetch); err != nil {
			return nil, err
		}
	}
	return addSamples(&t)
}

func parseSenc(t *track, b []byte, at, count int) error {
	if count < 8 {
		return fmt.Errorf("%w: short senc box", ErrBadMP4)
	}
	t.senc = true
	flags := be32(b, at) & 0xffffff
	n := be32(b, at+4)
	return readInfo(t, b, at+8, count-8, n, flags&2 != 0, nil)
}

func readInfo(t *track, b []byte, base, length int, n int64, subsamples bool, sizes func(int) int) error {
	if n < 0 || n > maxSamples || (t.ivSize > 0 && n > int64(length)) {
		return fmt.Errorf("%w: sample encryption info is too short", ErrBadMP4)
	}
	count := int(n)
	t.ivs = make([]iv, count)
	t.subs = make([][]subsample, count)
	p := 0
	for k := 0; k < count; k++ {
		start := p
		if t.ivSize > 0 {
			if p+t.ivSize > length {
				return fmt.Errorf("%w: sample IV is short", ErrBadMP4)
			}
			t.ivs[k] = parseIV(b, base+p, t.ivSize)
			p += t.ivSize
		} else {
			t.ivs[k] = parseIV(t.constIV, 0, len(t.constIV))
		}
		hasSubsamples := subsamples
		if sizes != nil {
			hasSubsamples = sizes(k) > t.ivSize
		}
		if hasSubsamples {
			if p+2 > length {
				return fmt.Errorf("%w: subsample count is short", ErrBadMP4)
			}
			m := be16(b, base+p)
			p += 2
			if int64(p)+6*int64(m) > int64(length) {
				return fmt.Errorf("%w: subsample entries are short", ErrBadMP4)
			}
			t.subs[k] = make([]subsample, m)
			for j := 0; j < m; j++ {
				t.subs[k][j] = subsample{
					clear:     be16(b, base+p),
					encrypted: be32(b, base+p+2),
				}
				p += 6
			}
		}
		if sizes != nil {
			p = start + sizes(k)
			if p > length {
				return fmt.Errorf("%w: saiz entry exceeds auxiliary data", ErrBadMP4)
			}
		}
	}
	return nil
}

func readAux(t *track, moovOffset int64, moov []byte, fetch Fetch) error {
	if len(t.saioOffsets) != 1 {
		return fmt.Errorf("%w: expected one sample encryption auxiliary offset", ErrBadMP4)
	}
	if len(t.sizes) == 0 {
		return fmt.Errorf("%w: encrypted track has no samples", ErrBadMP4)
	}
	var total int64
	sizeAt := func(k int) int {
		switch {
		case t.saizDefault != 0:
			return t.saizDefault
		case k < len(t.saizSizes):
			return int(t.saizSizes[k])
		default:
			return t.ivSize
		}
	}
	for k := range t.sizes {
		n := int64(sizeAt(k))
		if n < 0 || n > maxAuxSize-total {
			return fmt.Errorf("%w: auxiliary data exceeds limit", ErrBadMP4)
		}
		total += n
	}
	offset := t.saioOffsets[0]
	if offset >= moovOffset && offset-moovOffset <= int64(len(moov)) && total <= int64(len(moov))-(offset-moovOffset) {
		return readInfo(t, moov, int(offset-moovOffset), int(total), int64(len(t.sizes)), false, sizeAt)
	}
	if fetch == nil {
		return fmt.Errorf("%w: sample info is outside moov", ErrBadMP4)
	}
	data, err := fetch(offset, total)
	if err != nil {
		return fmt.Errorf("cenc: read auxiliary sample info: %w", err)
	}
	if int64(len(data)) != total {
		return fmt.Errorf("%w: short auxiliary sample info", ErrBadMP4)
	}
	return readInfo(t, data, 0, len(data), int64(len(t.sizes)), false, sizeAt)
}

func addSamples(t *track) ([]sample, error) {
	if len(t.stsc) == 0 || len(t.offsets) == 0 {
		return nil, fmt.Errorf("%w: no sample chunks", ErrBadMP4)
	}
	if len(t.ivs) < len(t.sizes) {
		return nil, fmt.Errorf("%w: %d IVs for %d samples", ErrBadMP4, len(t.ivs), len(t.sizes))
	}
	out := make([]sample, 0, len(t.sizes))
	sampleIndex := 0
	for entry := 0; entry < len(t.stsc); entry += 2 {
		first := int(t.stsc[entry]) - 1
		perChunk := t.stsc[entry+1]
		last := len(t.offsets)
		if entry+2 < len(t.stsc) {
			last = int(t.stsc[entry+2]) - 1
		}
		if perChunk < 0 || perChunk > maxSamples {
			return nil, fmt.Errorf("%w: invalid samples-per-chunk value", ErrBadMP4)
		}
		for chunk := maxInt(first, 0); chunk < last && chunk < len(t.offsets); chunk++ {
			offset := t.offsets[chunk]
			for j := int64(0); j < perChunk && sampleIndex < len(t.sizes); j++ {
				size := t.sizes[sampleIndex]
				if offset < 0 || size < 0 || size > math.MaxInt64-offset {
					return nil, fmt.Errorf("%w: sample offset overflow", ErrBadMP4)
				}
				var subs []subsample
				if t.subs[sampleIndex] != nil {
					subs = append(make([]subsample, 0, len(t.subs[sampleIndex])), t.subs[sampleIndex]...)
				}
				out = append(out, sample{
					offset: offset, size: size, iv: t.ivs[sampleIndex], subsamples: subs,
				})
				offset += size
				sampleIndex++
			}
		}
	}
	if sampleIndex != len(t.sizes) {
		return nil, fmt.Errorf("%w: chunks hold %d of %d samples", ErrBadMP4, sampleIndex, len(t.sizes))
	}
	return out, nil
}

func parseIV(b []byte, at, size int) iv {
	var raw [16]byte
	if size > 0 && at >= 0 && at+size <= len(b) {
		copy(raw[:], b[at:at+minInt(size, len(raw))])
	}
	return iv{high: binary.BigEndian.Uint64(raw[:8]), low: binary.BigEndian.Uint64(raw[8:])}
}

type box struct {
	buf    []byte
	at     int
	bodyAt int
	end    int
	typ    string
}

func parseBox(buf []byte, at, limit int) (*box, bool) {
	if at < 0 || limit > len(buf) || at+8 > limit {
		return nil, false
	}
	size := int64(binary.BigEndian.Uint32(buf[at : at+4]))
	header := int64(8)
	if size == 1 {
		if at+16 > limit {
			return nil, false
		}
		u := binary.BigEndian.Uint64(buf[at+8 : at+16])
		if u > math.MaxInt64 {
			return nil, false
		}
		size = int64(u)
		header = 16
	} else if size == 0 {
		size = int64(limit - at)
	}
	if size < header || size > int64(limit-at) {
		return nil, false
	}
	return &box{buf: buf, at: at, bodyAt: at + int(header), end: at + int(size), typ: fourCC(buf[at+4 : at+8])}, true
}

func (b *box) bodyCount() int { return b.end - b.bodyAt }

func (b *box) children(start int) []*box {
	out := make([]*box, 0)
	at := b.bodyAt + start
	if start < 0 || at > b.end {
		return out
	}
	for at < b.end {
		child, ok := parseBox(b.buf, at, b.end)
		if !ok {
			break
		}
		out = append(out, child)
		at = child.end
	}
	return out
}

func (b *box) find(path ...string) *box {
	current := b
	for _, typ := range path {
		var found *box
		for _, child := range current.children(0) {
			if child.typ == typ {
				found = child
				break
			}
		}
		if found == nil {
			return nil
		}
		current = found
	}
	return current
}

func (b *box) rename(typ string) {
	if len(typ) != 4 || b.at+8 > len(b.buf) {
		return
	}
	copy(b.buf[b.at+4:b.at+8], typ)
}

func be16(b []byte, i int) int {
	return int(binary.BigEndian.Uint16(b[i : i+2]))
}

func be32(b []byte, i int) int64 {
	return int64(binary.BigEndian.Uint32(b[i : i+4]))
}

func fourCC(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	return string(b[:4])
}

func min64(a, b int64) int64 {
	if a < b { return a }
	return b
}
func max64(a, b int64) int64 {
	if a > b { return a }
	return b
}
func minInt(a, b int) int {
	if a < b { return a }
	return b
}
func maxInt(a, b int) int {
	if a > b { return a }
	return b
}
