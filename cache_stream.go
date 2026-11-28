package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxAssetEvents = 80

type assetEvent struct {
	ID    int64  `json:"id"`
	TS    int64  `json:"ts"`
	Kind  string `json:"kind"` // "ram" | "hit" | "304" | "merge" | "miss" | "err"
	Cat   string `json:"cat"`  // "image" | "audio" | "script" | "data" | "other"
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type activityRing struct {
	mu    sync.Mutex
	seq   int64
	buf   [maxAssetEvents]assetEvent
	head  int
	count int
}

func shortAssetPath(rawURL string) string {
	p := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		p = u.Path
	} else if q := strings.IndexByte(p, '?'); q >= 0 {
		p = p[:q]
	}
	p = versionSegment.ReplaceAllString(p, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 96 {
		p = "…/" + p[len(p)-92:]
	}
	return p
}

func (r *activityRing) add(kind, cat, assetPath string, nBytes int64) {
	r.mu.Lock()
	r.seq++
	ev := assetEvent{
		ID:    r.seq,
		TS:    time.Now().Unix(),
		Kind:  kind,
		Cat:   cat,
		Path:  assetPath,
		Bytes: nBytes,
	}
	r.buf[r.head] = ev
	r.head = (r.head + 1) % maxAssetEvents
	if r.count < maxAssetEvents {
		r.count++
	}
	r.mu.Unlock()
}

func (r *activityRing) snapshot() []assetEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return []assetEvent{}
	}
	out := make([]assetEvent, r.count)
	for i := 0; i < r.count; i++ {
		idx := (r.head - 1 - i + maxAssetEvents) % maxAssetEvents
		out[i] = r.buf[idx]
	}
	return out
}

func (p *proxy) recordEvent(rawURL string, hdr http.Header, kind string, nBytes int64) {
	p.events.add(kind, classifyAssetURL(rawURL, hdr), shortAssetPath(rawURL), nBytes)
}

type cachedMeta struct {
	url        string
	header     http.Header
	headerLen  int64
	bodySize   int64
	modTime    time.Time
	lastAccess time.Time
	looseQuery bool
}

func isOpenRangeFromZero(rangeHdr string) bool {
	return strings.EqualFold(strings.TrimSpace(rangeHdr), "bytes=0-")
}

type cacheCategoryStat struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

type cacheBreakdownReport struct {
	Categories []cacheCategoryStat `json:"categories"`
	TotalBytes int64               `json:"totalBytes"`
	TotalFiles int64               `json:"totalFiles"`
}

var cacheCategoryOrder = []struct {
	key   string
	label string
}{
	{key: "image", label: "角色立绘与图像"},
	{key: "audio", label: "音频与语音"},
	{key: "script", label: "脚本与战斗动画"},
	{key: "data", label: "配置与字体数据"},
	{key: "other", label: "其他静态资源"},
}

func cacheCategoryLabel(key string) (string, bool) {
	for _, c := range cacheCategoryOrder {
		if c.key == key {
			return c.label, true
		}
	}
	return "", false
}

func classifyAssetURL(rawURL string, hdr http.Header) string {
	uPath := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		uPath = u.Path
	} else if q := strings.IndexByte(uPath, '?'); q >= 0 {
		uPath = uPath[:q]
	}
	ext := strings.ToLower(path.Ext(uPath))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".bmp", ".ico":
		return "image"
	case ".mp3", ".ogg", ".m4a", ".wav", ".aac", ".flac":
		return "audio"
	case ".js", ".mjs", ".cjs", ".css", ".wasm", ".atlas", ".skel":
		return "script"
	case ".json", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".xml", ".txt", ".csv":
		return "data"
	}
	ct := strings.ToLower(hdr.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.Contains(ct, "javascript") || strings.Contains(ct, "css"):
		return "script"
	case strings.Contains(ct, "json") || strings.Contains(ct, "font"):
		return "data"
	}
	return "other"
}

func isTransientNetErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{
		"eof",
		"connection reset",
		"forcibly closed",
		"broken pipe",
		"server closed idle connection",
		"client connection lost",
		"stream error",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// isFallbackEligibleErr 判断错误是否适合将失效的旧 CDN 分片（prd-game-a1~a5）自动回退到主域名重试。
func isFallbackEligibleErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if isTransientNetErr(err) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{
		"no such host",
		"lookup ",
		"connection refused",
		"connectex",
		"handshake",
		"tls:",
		"timeout",
		"unreachable",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

func urlsMatchForCache(cachedURL, target string, looseQuery bool) bool {
	cNorm := normalizeCDNShardURL(cachedURL)
	tNorm := normalizeCDNShardURL(target)
	if cNorm == tNorm {
		return true
	}
	if !looseQuery {
		return false
	}
	cBase, _, _ := strings.Cut(cNorm, "?")
	tBase, _, _ := strings.Cut(tNorm, "?")
	return cBase != "" && cBase == tBase
}

func inmMatches(inm, etag string) bool {
	if inm == "" || etag == "" {
		return false
	}
	trimTag := func(s string) string {
		return strings.TrimPrefix(strings.TrimSpace(s), "W/")
	}
	want := trimTag(etag)
	for _, part := range strings.Split(inm, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || trimTag(p) == want {
			return true
		}
	}
	return false
}

// isImageMagic 检查是否为常见浏览器图像格式的合法文件头（PNG / JPEG / GIF / WebP），兼容扩展名与真实编码不一致的历史资源。
func isImageMagic(head []byte) bool {
	if len(head) >= 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return true
	}
	if len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF {
		return true
	}
	if len(head) >= 6 && (bytes.Equal(head[:6], []byte("GIF87a")) || bytes.Equal(head[:6], []byte("GIF89a"))) {
		return true
	}
	if len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")) {
		return true
	}
	return false
}

// validateAssetBytes 校验静态资源长度与文件魔数，拦截 0 字节损坏件及被劫持返回的 HTML 错误页。
func validateAssetBytes(urlPath string, hdr http.Header, head []byte, atEOF bool) (bool, string) {
	if atEOF && len(head) == 0 {
		return false, "空响应 (0 字节)"
	}

	ext := strings.ToLower(path.Ext(urlPath))
	trimHead := bytes.TrimSpace(bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF}))

	if ext != ".html" && ext != ".htm" {
		ct := strings.ToLower(hdr.Get("Content-Type"))
		if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
			return false, "疑似错误网页 (" + ct + ")"
		}
		low := strings.ToLower(string(trimHead))
		if strings.HasPrefix(low, "<!doctype html") || strings.HasPrefix(low, "<html") ||
			strings.HasPrefix(low, "<head") || strings.HasPrefix(low, "<body") {
			return false, "响应正文为 HTML 错误页"
		}
	}

	if len(head) == 0 {
		return true, ""
	}

	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		if !isImageMagic(head) {
			return false, "图片文件头校验失败"
		}
	case ".woff":
		if len(head) < 4 || !bytes.Equal(head[:4], []byte("wOFF")) {
			return false, "WOFF 文件头校验失败"
		}
	case ".woff2":
		if len(head) < 4 || !bytes.Equal(head[:4], []byte("wOF2")) {
			return false, "WOFF2 文件头校验失败"
		}
	case ".ogg":
		if len(head) < 4 || !bytes.Equal(head[:4], []byte("OggS")) {
			return false, "OGG 文件头校验失败"
		}
	case ".json":
		if len(trimHead) == 0 || (trimHead[0] != '{' && trimHead[0] != '[') {
			return false, "JSON 语法头校验失败"
		}
	}
	return true, ""
}

// streamFetchAndStore 边下边传（Stream-Through）：上游到达首块有效数据即推送给浏览器，下载完整无误后原子入库。
// 当客户端带 Range: bytes=0- 且上游提供明确 Content-Length 时同样边下边回 206；遇到失效分片或瞬时中断会回退重试一次。
func (p *proxy) streamFetchAndStore(ctx context.Context, w http.ResponseWriter, r *http.Request, target, cacheFile string, openRangeZero bool) (bool, error) {
	primary, fallback := p.resolveUpstreamTargets(target)
	var resp *http.Response
	var headBuf [512]byte
	var n int
	var readErr error
	var atEOF bool

	for attempt := 0; attempt < 2; attempt++ {
		curTarget := primary
		if attempt > 0 && fallback != "" {
			curTarget = fallback
		}
		req, err := upstreamGet(ctx, curTarget, r.Header)
		if err != nil {
			return false, err
		}
		if attempt > 0 {
			req.Close = true
		}
		resp, err = p.tr.RoundTrip(req)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil {
				if fallback != "" && isFallbackEligibleErr(err) {
					p.markShardDead(extractHostFromURL(primary))
					log.Printf("旧分片 %s 不可达，已自动回退主域名 %s 拉取（%v）", extractHostFromURL(primary), extractHostFromURL(fallback), err)
					continue
				}
				if fallback == "" && isTransientNetErr(err) {
					if p.tr != nil {
						p.tr.CloseIdleConnections()
					}
					log.Printf("回源连接中断，正在重试一次：%s（%v）", curTarget, err)
					continue
				}
			}
			return false, err
		}

		if !cacheable(resp) {
			defer resp.Body.Close()
			p.passthrough.Add(1)
			relay(w, resp)
			return true, nil
		}

		n = 0
		readErr = nil
		for n < 16 {
			k, rErr := resp.Body.Read(headBuf[n:])
			n += k
			if rErr != nil {
				readErr = rErr
				break
			}
		}
		atEOF = readErr == io.EOF
		truncatedZero := atEOF && n == 0 && resp.ContentLength != 0
		if (readErr != nil && !atEOF && isFallbackEligibleErr(readErr)) || truncatedZero {
			_ = resp.Body.Close()
			if attempt == 0 && ctx.Err() == nil {
				if fallback != "" {
					p.markShardDead(extractHostFromURL(primary))
					log.Printf("旧分片 %s 首包异常，已自动回退主域名 %s 拉取", extractHostFromURL(primary), extractHostFromURL(fallback))
					continue
				}
				if readErr == nil || isTransientNetErr(readErr) {
					if p.tr != nil {
						p.tr.CloseIdleConnections()
					}
					log.Printf("首包读取中断，正在重建连接重试一次：%s", curTarget)
					continue
				}
			}
			if readErr != nil && !atEOF {
				return false, readErr
			}
			if resp.ContentLength > 0 {
				return false, io.ErrUnexpectedEOF
			}
			// 未知长度但两次均为 0 字节，落入正常空响应校验放行
			resp = &http.Response{
				StatusCode:    resp.StatusCode,
				Header:        resp.Header,
				ContentLength: resp.ContentLength,
				Body:          io.NopCloser(bytes.NewReader(nil)),
			}
			break
		}
		if readErr != nil && !atEOF {
			_ = resp.Body.Close()
			return false, readErr
		}
		break
	}
	defer resp.Body.Close()

	if ok, reason := validateAssetBytes(r.URL.Path, resp.Header, headBuf[:n], atEOF); !ok {
		p.passthrough.Add(1)
		log.Printf("跳过异常响应（%s），不写入缓存：%s", reason, target)
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if n > 0 {
			_, _ = w.Write(headBuf[:n])
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if !atEOF {
			_, _ = io.Copy(w, resp.Body)
		}
		return true, nil
	}

	cacheHdr := http.Header{}
	copyHeader(cacheHdr, resp.Header, "Set-Cookie", "Date", "Age")
	metaBytes, err := json.Marshal(entryMeta{URL: target, Header: cacheHdr})
	if err != nil {
		return false, err
	}

	var tmp *os.File
	if mkErr := os.MkdirAll(filepath.Dir(cacheFile), 0o755); mkErr == nil {
		tmp, _ = os.CreateTemp(filepath.Dir(cacheFile), "tmp-*")
	}
	var tmpErr error
	headerLineLen := int64(len(metaBytes) + 1)
	totalBody := int64(n)
	keepRAM := p.ram != nil && p.ram.maxItemBytes > 0 && (resp.ContentLength < 0 || resp.ContentLength <= p.ram.maxItemBytes)
	var ramBuf bytes.Buffer
	if keepRAM && n > 0 {
		if int64(n) <= p.ram.maxItemBytes {
			ramBuf.Write(headBuf[:n])
		} else {
			keepRAM = false
		}
	}
	if tmp != nil {
		if _, wErr := tmp.Write(append(metaBytes, '\n')); wErr != nil {
			tmpErr = wErr
		} else if n > 0 {
			if _, wErr := tmp.Write(headBuf[:n]); wErr != nil {
				tmpErr = wErr
			}
		}
	}

	streamToClient := !openRangeZero || resp.ContentLength > 0
	var fl http.Flusher
	clientFailed := !streamToClient
	if streamToClient {
		copyHeader(w.Header(), resp.Header)
		if openRangeZero {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", resp.ContentLength-1, resp.ContentLength))
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(resp.StatusCode)
		}
		fl, _ = w.(http.Flusher)
		if n > 0 {
			if _, wErr := w.Write(headBuf[:n]); wErr != nil {
				clientFailed = true
			} else if fl != nil {
				fl.Flush()
			}
		}
	}

	var upErr error
	if !atEOF {
		buf := make([]byte, 32*1024)
		for {
			k, rErr := resp.Body.Read(buf)
			if k > 0 {
				totalBody += int64(k)
				if keepRAM {
					if totalBody <= p.ram.maxItemBytes {
						ramBuf.Write(buf[:k])
					} else {
						keepRAM = false
						ramBuf.Reset()
					}
				}
				if tmp != nil && tmpErr == nil {
					if _, wErr := tmp.Write(buf[:k]); wErr != nil {
						tmpErr = wErr
					}
				}
				if !clientFailed {
					if _, wErr := w.Write(buf[:k]); wErr != nil {
						clientFailed = true
					} else if fl != nil {
						fl.Flush()
					}
				}
			}
			if rErr != nil {
				if rErr != io.EOF {
					upErr = rErr
				}
				break
			}
		}
	}

	if tmp != nil {
		if cErr := tmp.Close(); tmpErr == nil {
			tmpErr = cErr
		}
		validLen := totalBody > 0 && (resp.ContentLength < 0 || totalBody == resp.ContentLength)
		if upErr == nil && tmpErr == nil && validLen {
			p.usageMu.Lock()
			var oldSize int64
			existed := false
			if st, statErr := os.Stat(cacheFile); statErr == nil {
				existed = true
				oldSize = st.Size()
			}
			renameErr := os.Rename(tmp.Name(), cacheFile)
			if renameErr == nil {
				if st, statErr := os.Stat(cacheFile); statErr == nil {
					p.cachedBytes.Add(st.Size() - oldSize)
					if !existed {
						p.cachedFiles.Add(1)
					}
					now := time.Now()
					p.metaUpdateMu.Lock()
					p.metaIdx.Store(cacheFile, &cachedMeta{
						url:        target,
						header:     cacheHdr.Clone(),
						headerLen:  headerLineLen,
						bodySize:   totalBody,
						modTime:    st.ModTime(),
						lastAccess: now,
						looseQuery: false,
					})
					p.metaUpdateMu.Unlock()
					if p.ram != nil {
						if keepRAM && int64(ramBuf.Len()) == totalBody {
							p.ram.Put(cacheFile, target, cacheHdr, ramBuf.Bytes(), st.ModTime(), false)
						} else {
							p.ram.Delete(cacheFile)
						}
					}
					p.markMetaIndexDirty()
				}
			}
			p.usageMu.Unlock()
			if renameErr != nil {
				_ = os.Remove(tmp.Name())
			}
		} else {
			_ = os.Remove(tmp.Name())
		}
	}

	if !streamToClient {
		return false, upErr
	}
	if upErr == nil && totalBody > 0 {
		p.recordEvent(target, cacheHdr, "miss", totalBody)
	}
	return true, nil
}

func (p *proxy) touchWarm(cacheFile string, _ *cachedMeta) {
	now := time.Now()
	p.metaUpdateMu.Lock()
	if v, ok := p.metaIdx.Load(cacheFile); ok {
		cm := v.(*cachedMeta)
		if cm != nil && cm.headerLen > 0 && (cm.lastAccess.IsZero() || now.Sub(cm.lastAccess) >= 12*time.Hour) {
			p.metaIdx.Store(cacheFile, &cachedMeta{
				url:        cm.url,
				header:     cm.header,
				headerLen:  cm.headerLen,
				bodySize:   cm.bodySize,
				modTime:    cm.modTime, // 只记录最后确认/下载时间，绝不在此重置 TTL 计时
				lastAccess: now,
				looseQuery: cm.looseQuery,
			})
			p.markMetaIndexDirty()
		}
	}
	p.metaUpdateMu.Unlock()
	if p.ram != nil {
		p.ram.TouchAccess(cacheFile, now)
	}
}

func (p *proxy) checkFreshnessOnHit(target, cacheFile string, reqHdr, cachedHdr http.Header, verifiedAt, accessedAt time.Time) {
	if !verifiedAt.IsZero() && time.Since(verifiedAt) > p.ttl {
		go p.revalidate(target, cacheFile, reqHdr.Clone(), cachedHdr.Clone())
	}
	if accessedAt.IsZero() || time.Since(accessedAt) > 12*time.Hour {
		go p.touchWarm(cacheFile, nil)
	}
}

func (p *proxy) serveCached(w http.ResponseWriter, r *http.Request, target, cacheFile string, hit bool) bool {
	if p.serveCachedFile(w, r, target, cacheFile, hit) {
		return true
	}
	legacy := p.legacyCachePath(target)
	if legacy != cacheFile {
		return p.serveCachedFile(w, r, target, legacy, hit)
	}
	return false
}

func (p *proxy) serveCachedFile(w http.ResponseWriter, r *http.Request, target, cacheFile string, hit bool) bool {
	// 1. 优先命中 RAM SLRU 热缓存（绕开磁盘打开与安全软件实时扫描开销）
	if p.ram != nil {
		if entry, ok := p.ram.Get(cacheFile); ok && urlsMatchForCache(entry.url, target, entry.looseQuery) {
			etag := entry.header.Get("Etag")
			if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Range") == "" &&
				etag != "" && inmMatches(r.Header.Get("If-None-Match"), etag) {
				if hit {
					p.hits.Add(1)
					p.ramHits.Add(1)
					p.hits304.Add(1)
					p.recordEvent(target, entry.header, "304", 0)
					p.checkFreshnessOnHit(target, cacheFile, r.Header, entry.header, entry.modTime, entry.lastAccess)
				}
				for k, vv := range entry.header {
					w.Header()[k] = append([]string(nil), vv...)
				}
				w.WriteHeader(http.StatusNotModified)
				return true
			}
			if r.Method == http.MethodHead && r.Header.Get("Range") == "" {
				if hit {
					p.hits.Add(1)
					p.ramHits.Add(1)
					p.recordEvent(target, entry.header, "ram", 0)
					p.checkFreshnessOnHit(target, cacheFile, r.Header, entry.header, entry.modTime, entry.lastAccess)
				}
				for k, vv := range entry.header {
					w.Header()[k] = append([]string(nil), vv...)
				}
				w.Header().Set("Content-Length", strconv.FormatInt(entry.size, 10))
				w.WriteHeader(http.StatusOK)
				return true
			}
			if hit {
				p.hits.Add(1)
				p.ramHits.Add(1)
				p.recordEvent(target, entry.header, "ram", entry.size)
				p.checkFreshnessOnHit(target, cacheFile, r.Header, entry.header, entry.modTime, entry.lastAccess)
			}
			for k, vv := range entry.header {
				w.Header()[k] = append([]string(nil), vv...)
			}
			modTime, _ := http.ParseTime(entry.header.Get("Last-Modified"))
			p.servedBytes.Add(entry.size)
			http.ServeContent(w, r, "", modTime, bytes.NewReader(entry.body))
			return true
		}
	}

	var cm *cachedMeta
	if v, ok := p.metaIdx.Load(cacheFile); ok {
		cm = v.(*cachedMeta)
		if !urlsMatchForCache(cm.url, target, cm.looseQuery) {
			return false
		}
		etag := cm.header.Get("Etag")
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Range") == "" &&
			etag != "" && inmMatches(r.Header.Get("If-None-Match"), etag) {
			if hit {
				p.hits.Add(1)
				p.hits304.Add(1)
				p.recordEvent(target, cm.header, "304", 0)
				p.checkFreshnessOnHit(target, cacheFile, r.Header, cm.header, cm.modTime, cm.lastAccess)
			}
			for k, vv := range cm.header {
				w.Header()[k] = append([]string(nil), vv...)
			}
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		if r.Method == http.MethodHead && r.Header.Get("Range") == "" {
			if hit {
				p.hits.Add(1)
				p.recordEvent(target, cm.header, "hit", 0)
				p.checkFreshnessOnHit(target, cacheFile, r.Header, cm.header, cm.modTime, cm.lastAccess)
			}
			for k, vv := range cm.header {
				w.Header()[k] = append([]string(nil), vv...)
			}
			w.Header().Set("Content-Length", strconv.FormatInt(cm.bodySize, 10))
			w.WriteHeader(http.StatusOK)
			return true
		}
	}

	f, err := os.Open(cacheFile)
	if err != nil {
		p.metaIdx.Delete(cacheFile)
		if p.ram != nil {
			p.ram.Delete(cacheFile)
		}
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}

	if cm == nil || st.Size() < cm.headerLen || st.Size() != cm.headerLen+cm.bodySize {
		line, rErr := bufio.NewReader(f).ReadBytes('\n')
		var m entryMeta
		if rErr != nil || json.Unmarshal(line, &m) != nil || !urlsMatchForCache(m.URL, target, m.LooseQuery) {
			return false
		}
		bodySize := st.Size() - int64(len(line))
		if bodySize <= 0 {
			return false
		}
		cm = &cachedMeta{
			url:        m.URL,
			header:     m.Header.Clone(),
			headerLen:  int64(len(line)),
			bodySize:   bodySize,
			modTime:    st.ModTime(),
			lastAccess: st.ModTime(),
			looseQuery: m.LooseQuery,
		}
		p.metaIdx.Store(cacheFile, cm)
		p.markMetaIndexDirty()
	}

	etag := cm.header.Get("Etag")
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Range") == "" &&
		etag != "" && inmMatches(r.Header.Get("If-None-Match"), etag) {
		if hit {
			p.hits.Add(1)
			p.hits304.Add(1)
			p.recordEvent(target, cm.header, "304", 0)
			p.checkFreshnessOnHit(target, cacheFile, r.Header, cm.header, st.ModTime(), cm.lastAccess)
		}
		for k, vv := range cm.header {
			w.Header()[k] = append([]string(nil), vv...)
		}
		w.WriteHeader(http.StatusNotModified)
		return true
	}

	if hit {
		p.hits.Add(1)
		p.recordEvent(target, cm.header, "hit", cm.bodySize)
		p.checkFreshnessOnHit(target, cacheFile, r.Header, cm.header, st.ModTime(), cm.lastAccess)
	}

	for k, vv := range cm.header {
		w.Header()[k] = append([]string(nil), vv...)
	}
	modTime, _ := http.ParseTime(cm.header.Get("Last-Modified"))
	if p.ram != nil && cm.bodySize > 0 && cm.bodySize <= p.ram.maxItemBytes {
		memBody := make([]byte, cm.bodySize)
		if _, rErr := io.ReadFull(io.NewSectionReader(f, cm.headerLen, cm.bodySize), memBody); rErr == nil {
			p.ram.Put(cacheFile, cm.url, cm.header, memBody, cm.modTime, cm.looseQuery)
			p.servedBytes.Add(cm.bodySize)
			http.ServeContent(w, r, "", modTime, bytes.NewReader(memBody))
			return true
		}
	}
	body := io.NewSectionReader(f, cm.headerLen, cm.bodySize)
	p.servedBytes.Add(cm.bodySize)
	http.ServeContent(w, r, "", modTime, body)
	return true
}

// inspectCache 遍历本地缓存校验结构、体积与文件魔数，精准剔除损坏或被劫持的文件（不误删正常资源）。
func (p *proxy) inspectCache() (checked, removed, reclaimedBytes int64, err error) {
	cacheRoot := p.getCacheRoot()
	if !fileExists(cacheRoot) {
		return 0, 0, 0, nil
	}

	err = filepath.WalkDir(cacheRoot, func(filePath string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			return nil
		}
		if isInternalCacheFile(d.Name()) {
			if strings.HasPrefix(d.Name(), "tmp-") && time.Since(info.ModTime()) > 5*time.Minute {
				_ = os.Remove(filePath)
				reclaimedBytes += info.Size()
			}
			return nil
		}

		checked++
		f, oErr := os.Open(filePath)
		if oErr != nil {
			return nil
		}
		br := bufio.NewReaderSize(f, 1024)
		line, lErr := br.ReadBytes('\n')
		var m entryMeta
		var head [512]byte
		hn := 0
		if lErr == nil && json.Unmarshal(line, &m) == nil {
			hn, _ = io.ReadFull(br, head[:])
		}
		_ = f.Close()

		corrupt := lErr != nil || json.Unmarshal(line, &m) != nil
		bodySize := info.Size() - int64(len(line))
		if !corrupt {
			u, uErr := url.Parse(m.URL)
			if uErr != nil || bodySize <= 0 {
				corrupt = true
			} else {
				valid, _ := validateAssetBytes(u.Path, m.Header, head[:hn], bodySize <= int64(hn))
				if !valid {
					corrupt = true
				}
			}
		}

		if corrupt {
			p.usageMu.Lock()
			rmErr := os.Remove(filePath)
			p.usageMu.Unlock()
			if rmErr == nil {
				p.metaIdx.Delete(filePath)
				if p.ram != nil {
					p.ram.Delete(filePath)
				}
				p.markMetaIndexDirty()
				removed++
				reclaimedBytes += info.Size()
			}
		} else {
			p.metaIdx.Store(filePath, &cachedMeta{
				url:        m.URL,
				header:     m.Header.Clone(),
				headerLen:  int64(len(line)),
				bodySize:   bodySize,
				modTime:    info.ModTime(),
				lastAccess: info.ModTime(),
				looseQuery: m.LooseQuery,
			})
			p.markMetaIndexDirty()
		}
		return nil
	})

	p.scanCacheUsage()
	return checked, removed, reclaimedBytes, err
}

// scanCacheHealth 只读扫描缓存健康状况（不删文件）。useIndex=true 时复用内存索引中的已知健康条目以加快体检速度。
func (p *proxy) scanCacheHealth(useIndex bool) (checked, bad, badBytes int64) {
	cacheRoot := p.getCacheRoot()
	if !fileExists(cacheRoot) {
		return 0, 0, 0
	}
	_ = filepath.WalkDir(cacheRoot, func(filePath string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() || isInternalCacheFile(d.Name()) {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			return nil
		}
		checked++
		if useIndex {
			if v, ok := p.metaIdx.Load(filePath); ok {
				cm := v.(*cachedMeta)
				if info.Size() == cm.headerLen+cm.bodySize && info.ModTime().Equal(cm.modTime) {
					return nil
				}
			}
		}

		f, oErr := os.Open(filePath)
		if oErr != nil {
			return nil
		}
		br := bufio.NewReaderSize(f, 1024)
		line, lErr := br.ReadBytes('\n')
		var m entryMeta
		var head [512]byte
		hn := 0
		if lErr == nil && json.Unmarshal(line, &m) == nil {
			hn, _ = io.ReadFull(br, head[:])
		}
		_ = f.Close()

		corrupt := lErr != nil || json.Unmarshal(line, &m) != nil
		bodySize := info.Size() - int64(len(line))
		if !corrupt {
			u, uErr := url.Parse(m.URL)
			if uErr != nil || bodySize <= 0 {
				corrupt = true
			} else if valid, _ := validateAssetBytes(u.Path, m.Header, head[:hn], bodySize <= int64(hn)); !valid {
				corrupt = true
			}
		}
		if corrupt {
			bad++
			badBytes += info.Size()
		} else {
			p.metaIdx.Store(filePath, &cachedMeta{
				url:        m.URL,
				header:     m.Header.Clone(),
				headerLen:  int64(len(line)),
				bodySize:   bodySize,
				modTime:    info.ModTime(),
				lastAccess: info.ModTime(),
				looseQuery: m.LooseQuery,
			})
			p.markMetaIndexDirty()
		}
		return nil
	})
	return checked, bad, badBytes
}

func (p *proxy) readEntryMetaForBreakdown(filePath string, info fs.FileInfo) (rawURL string, hdr http.Header) {
	if v, ok := p.metaIdx.Load(filePath); ok {
		cm := v.(*cachedMeta)
		if info == nil || info.Size() == cm.headerLen+cm.bodySize {
			return cm.url, cm.header
		}
	}
	f, err := os.Open(filePath)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	line, lErr := bufio.NewReaderSize(f, 1024).ReadBytes('\n')
	if lErr != nil {
		return "", nil
	}
	var m entryMeta
	if json.Unmarshal(line, &m) != nil {
		return "", nil
	}
	if info != nil {
		bodySize := info.Size() - int64(len(line))
		if bodySize > 0 {
			p.metaIdx.Store(filePath, &cachedMeta{
				url:        m.URL,
				header:     m.Header.Clone(),
				headerLen:  int64(len(line)),
				bodySize:   bodySize,
				modTime:    info.ModTime(),
				lastAccess: info.ModTime(),
				looseQuery: m.LooseQuery,
			})
			p.markMetaIndexDirty()
		}
	}
	return m.URL, m.Header
}

// cacheBreakdown 按 5 类静态资源统计占用明细，优先命中内存索引避免重复磁盘打开。
func (p *proxy) cacheBreakdown() cacheBreakdownReport {
	stats := make(map[string]*cacheCategoryStat, len(cacheCategoryOrder))
	rep := cacheBreakdownReport{
		Categories: make([]cacheCategoryStat, 0, len(cacheCategoryOrder)),
	}
	for _, c := range cacheCategoryOrder {
		stats[c.key] = &cacheCategoryStat{Key: c.key, Label: c.label}
	}

	cacheRoot := p.getCacheRoot()
	if fileExists(cacheRoot) {
		_ = filepath.WalkDir(cacheRoot, func(filePath string, d fs.DirEntry, wErr error) error {
			if wErr != nil || d.IsDir() || isInternalCacheFile(d.Name()) {
				return nil
			}
			info, iErr := d.Info()
			if iErr != nil {
				return nil
			}
			rawURL, hdr := p.readEntryMetaForBreakdown(filePath, info)
			cat := classifyAssetURL(rawURL, hdr)
			st := stats[cat]
			if st == nil {
				st = stats["other"]
			}
			st.Files++
			st.Bytes += info.Size()
			rep.TotalFiles++
			rep.TotalBytes += info.Size()
			return nil
		})
	}
	for _, c := range cacheCategoryOrder {
		rep.Categories = append(rep.Categories, *stats[c.key])
	}
	return rep
}

func (p *proxy) clearCacheCategory(category string) (removed, reclaimedBytes int64, label string, err error) {
	label, ok := cacheCategoryLabel(category)
	if !ok {
		return 0, 0, "", fmt.Errorf("未知缓存分类：%s", category)
	}
	cacheRoot := p.getCacheRoot()
	if !fileExists(cacheRoot) {
		return 0, 0, label, nil
	}

	err = filepath.WalkDir(cacheRoot, func(filePath string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() || isInternalCacheFile(d.Name()) {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			return nil
		}
		rawURL, hdr := p.readEntryMetaForBreakdown(filePath, info)
		if classifyAssetURL(rawURL, hdr) != category {
			return nil
		}
		p.usageMu.Lock()
		rmErr := os.Remove(filePath)
		p.usageMu.Unlock()
		if rmErr == nil {
			p.metaIdx.Delete(filePath)
			if p.ram != nil {
				p.ram.Delete(filePath)
			}
			p.markMetaIndexDirty()
			removed++
			reclaimedBytes += info.Size()
		}
		return nil
	})

	p.scanCacheUsage()
	return removed, reclaimedBytes, label, err
}

// pruneColdCache 清理超过 maxAge 未访问过的冷门缓存文件，保留常用战斗/立绘资源。
func (p *proxy) pruneColdCache(maxAge time.Duration) (removed, reclaimedBytes int64, err error) {
	if maxAge <= 0 {
		return 0, 0, nil
	}
	cacheRoot := p.getCacheRoot()
	if !fileExists(cacheRoot) {
		return 0, 0, nil
	}

	err = filepath.WalkDir(cacheRoot, func(filePath string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() || isInternalCacheFile(d.Name()) {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			return nil
		}
		lastUsed := info.ModTime()
		if v, ok := p.metaIdx.Load(filePath); ok {
			if cm := v.(*cachedMeta); cm != nil && cm.lastAccess.After(lastUsed) {
				lastUsed = cm.lastAccess
			}
		}
		if time.Since(lastUsed) > maxAge {
			p.usageMu.Lock()
			rmErr := os.Remove(filePath)
			p.usageMu.Unlock()
			if rmErr == nil {
				p.metaIdx.Delete(filePath)
				if p.ram != nil {
					p.ram.Delete(filePath)
				}
				p.markMetaIndexDirty()
				removed++
				reclaimedBytes += info.Size()
			}
		}
		return nil
	})

	p.scanCacheUsage()
	return removed, reclaimedBytes, err
}
