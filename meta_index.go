package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	metaIndexFileName        = ".meta_index.v1"
	metaIndexMagic           = "GBFCMIDX"
	metaIndexVersion         = uint32(2)
	metaIndexSaveMinInterval = 15 * time.Second
	metaIndexDirtyThreshold  = 256
)

func isInternalCacheFile(name string) bool {
	return strings.HasPrefix(name, "tmp-") || name == metaIndexFileName || strings.HasPrefix(name, ".meta_index.")
}

func appendLenBytes(dst []byte, s string) ([]byte, bool) {
	if len(s) > 0xFFFF {
		return dst, false
	}
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], uint16(len(s)))
	dst = append(dst, b[:]...)
	dst = append(dst, s...)
	return dst, true
}

func readLenString(data []byte, pos *int) (string, bool) {
	if *pos+2 > len(data) {
		return "", false
	}
	n := int(binary.LittleEndian.Uint16(data[*pos : *pos+2]))
	*pos += 2
	if *pos+n > len(data) {
		return "", false
	}
	s := string(data[*pos : *pos+n])
	*pos += n
	return s, true
}

type metaSnapshotItem struct {
	relPath string
	meta    *cachedMeta
}

func (p *proxy) bootstrapMetaIndex() {
	cacheRoot := p.getCacheRoot()
	var files, size, added int64
	if fileExists(cacheRoot) {
		_ = filepath.WalkDir(cacheRoot, func(filePath string, d fs.DirEntry, wErr error) error {
			if wErr != nil || d.IsDir() {
				return nil
			}
			info, iErr := d.Info()
			if iErr != nil {
				return nil
			}
			if isInternalCacheFile(d.Name()) {
				if strings.HasPrefix(d.Name(), "tmp-") && time.Since(info.ModTime()) > time.Hour {
					_ = os.Remove(filePath)
				}
				return nil
			}
			files++
			size += info.Size()
			if _, ok := p.metaIdx.Load(filePath); ok {
				return nil
			}
			f, oErr := os.Open(filePath)
			if oErr != nil {
				return nil
			}
			line, lErr := bufio.NewReaderSize(f, 1024).ReadBytes('\n')
			_ = f.Close()
			if lErr != nil {
				return nil
			}
			var m entryMeta
			if json.Unmarshal(line, &m) != nil || m.URL == "" {
				return nil
			}
			bodySize := info.Size() - int64(len(line))
			if bodySize <= 0 {
				return nil
			}
			p.metaIdx.Store(filePath, &cachedMeta{
				url:        m.URL,
				header:     m.Header.Clone(),
				headerLen:  int64(len(line)),
				bodySize:   bodySize,
				modTime:    info.ModTime(),
				lastAccess: info.ModTime(),
				looseQuery: m.LooseQuery,
			})
			added++
			return nil
		})
	}
	p.cachedFiles.Store(files)
	p.cachedBytes.Store(size)
	p.metaIdxReady.Store(true)
	if added > 0 || (files > 0 && !fileExists(filepath.Join(cacheRoot, metaIndexFileName))) {
		_ = p.saveMetaIndex()
	}
}

func (p *proxy) loadMetaIndex() (files, totalBytes int64, ok bool) {
	cacheRoot := p.getCacheRoot()
	indexPath := filepath.Join(cacheRoot, metaIndexFileName)
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return 0, 0, false
	}
	// Magic(8) + Version(4) + Count(4) + CRC32(4) = 20 字节最小长度
	if len(data) < 20 || string(data[:8]) != metaIndexMagic {
		log.Printf("元数据索引头异常，将自动重建索引：%s", indexPath)
		return 0, 0, false
	}
	ver := binary.LittleEndian.Uint32(data[8:12])
	if ver != 1 && ver != metaIndexVersion {
		log.Printf("元数据索引版本不匹配（%d），将自动重建索引", ver)
		return 0, 0, false
	}
	payloadEnd := len(data) - 4
	wantCRC := binary.LittleEndian.Uint32(data[payloadEnd:])
	if crc32.ChecksumIEEE(data[:payloadEnd]) != wantCRC {
		log.Printf("元数据索引校验和损坏，已自动回退到磁盘重建")
		return 0, 0, false
	}

	count := binary.LittleEndian.Uint32(data[12:16])
	pos := 16
	loaded := make([]struct {
		absPath string
		cm      *cachedMeta
	}, 0, count)

	fixedLen := 25
	if ver == 2 {
		fixedLen = 33
	}

	for i := uint32(0); i < count; i++ {
		relSlash, rOk := readLenString(data[:payloadEnd], &pos)
		if !rOk || relSlash == "" || strings.HasPrefix(relSlash, "/") || strings.Contains(relSlash, "..") {
			return 0, 0, false
		}
		rawURL, uOk := readLenString(data[:payloadEnd], &pos)
		if !uOk || rawURL == "" {
			return 0, 0, false
		}
		if pos+fixedLen > payloadEnd {
			return 0, 0, false
		}
		hdrLen := int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
		bodySize := int64(binary.LittleEndian.Uint64(data[pos+8 : pos+16]))
		modNano := int64(binary.LittleEndian.Uint64(data[pos+16 : pos+24]))
		modT := time.Unix(0, modNano)
		accessT := modT
		var flags byte
		if ver == 2 {
			accessNano := int64(binary.LittleEndian.Uint64(data[pos+24 : pos+32]))
			if accessNano > 0 {
				accessT = time.Unix(0, accessNano)
			}
			flags = data[pos+32]
		} else {
			flags = data[pos+24]
		}
		pos += fixedLen
		if hdrLen <= 0 || bodySize <= 0 {
			return 0, 0, false
		}

		if pos+2 > payloadEnd {
			return 0, 0, false
		}
		kCount := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
		pos += 2
		hdr := make(http.Header, kCount)
		for k := 0; k < kCount; k++ {
			hk, kOk := readLenString(data[:payloadEnd], &pos)
			if !kOk || pos+2 > payloadEnd {
				return 0, 0, false
			}
			vCount := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
			pos += 2
			vals := make([]string, 0, vCount)
			for v := 0; v < vCount; v++ {
				hv, vOk := readLenString(data[:payloadEnd], &pos)
				if !vOk {
					return 0, 0, false
				}
				vals = append(vals, hv)
			}
			hdr[hk] = vals
		}

		absPath := filepath.Join(cacheRoot, filepath.FromSlash(relSlash))
		loaded = append(loaded, struct {
			absPath string
			cm      *cachedMeta
		}{
			absPath: absPath,
			cm: &cachedMeta{
				url:        rawURL,
				header:     hdr,
				headerLen:  hdrLen,
				bodySize:   bodySize,
				modTime:    modT,
				lastAccess: accessT,
				looseQuery: (flags & 0x01) != 0,
			},
		})
		files++
		totalBytes += hdrLen + bodySize
	}
	if pos != payloadEnd {
		return 0, 0, false
	}

	p.metaIdx.Clear()
	for _, it := range loaded {
		p.metaIdx.Store(it.absPath, it.cm)
	}
	p.cachedFiles.Store(files)
	p.cachedBytes.Store(totalBytes)
	p.metaIdxReady.Store(true)
	p.metaIndexDirty.Store(false)
	p.metaIndexWrites.Store(0)
	return files, totalBytes, true
}

func (p *proxy) saveMetaIndex() error {
	rawRoot := strings.TrimSpace(p.getCacheRoot())
	if rawRoot == "" {
		return nil
	}
	cacheRoot := filepath.Clean(rawRoot)
	p.metaSaveMu.Lock()
	defer p.metaSaveMu.Unlock()

	var items []metaSnapshotItem
	p.metaIdx.Range(func(k, v any) bool {
		absPath, ok1 := k.(string)
		cm, ok2 := v.(*cachedMeta)
		if !ok1 || !ok2 || cm == nil || cm.url == "" || cm.headerLen <= 0 || cm.bodySize <= 0 {
			return true
		}
		rel, err := filepath.Rel(cacheRoot, absPath)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
		items = append(items, metaSnapshotItem{
			relPath: filepath.ToSlash(rel),
			meta:    cm,
		})
		return true
	})

	sort.Slice(items, func(i, j int) bool {
		return items[i].relPath < items[j].relPath
	})

	var buf bytes.Buffer
	buf.Grow(16 + len(items)*168 + 4)
	buf.WriteString(metaIndexMagic)
	var hdr4 [4]byte
	binary.LittleEndian.PutUint32(hdr4[:], metaIndexVersion)
	buf.Write(hdr4[:])
	countPos := buf.Len()
	buf.Write(hdr4[:])

	var written uint32
	for _, it := range items {
		start := buf.Len()
		var tmp []byte
		ok := false
		if tmp, ok = appendLenBytes(tmp, it.relPath); !ok {
			continue
		}
		if tmp, ok = appendLenBytes(tmp, it.meta.url); !ok {
			continue
		}
		acc := it.meta.lastAccess
		if acc.IsZero() {
			acc = it.meta.modTime
		}
		var fixed [33]byte
		binary.LittleEndian.PutUint64(fixed[0:8], uint64(it.meta.headerLen))
		binary.LittleEndian.PutUint64(fixed[8:16], uint64(it.meta.bodySize))
		binary.LittleEndian.PutUint64(fixed[16:24], uint64(it.meta.modTime.UnixNano()))
		binary.LittleEndian.PutUint64(fixed[24:32], uint64(acc.UnixNano()))
		if it.meta.looseQuery {
			fixed[32] = 0x01
		}
		tmp = append(tmp, fixed[:]...)

		if len(it.meta.header) > 0xFFFF {
			continue
		}
		var kc [2]byte
		binary.LittleEndian.PutUint16(kc[:], uint16(len(it.meta.header)))
		tmp = append(tmp, kc[:]...)

		keys := make([]string, 0, len(it.meta.header))
		for hk := range it.meta.header {
			keys = append(keys, hk)
		}
		sort.Strings(keys)
		badHdr := false
		for _, hk := range keys {
			if tmp, ok = appendLenBytes(tmp, hk); !ok {
				badHdr = true
				break
			}
			vals := it.meta.header[hk]
			if len(vals) > 0xFFFF {
				badHdr = true
				break
			}
			var vc [2]byte
			binary.LittleEndian.PutUint16(vc[:], uint16(len(vals)))
			tmp = append(tmp, vc[:]...)
			for _, hv := range vals {
				if tmp, ok = appendLenBytes(tmp, hv); !ok {
					badHdr = true
					break
				}
			}
			if badHdr {
				break
			}
		}
		if badHdr {
			buf.Truncate(start)
			continue
		}
		buf.Write(tmp)
		written++
	}

	raw := buf.Bytes()
	binary.LittleEndian.PutUint32(raw[countPos:countPos+4], written)
	sum := crc32.ChecksumIEEE(raw)
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], sum)
	buf.Write(crcBuf[:])

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return fmt.Errorf("创建缓存目录失败：%w", err)
	}
	tmpPath := filepath.Join(cacheRoot, metaIndexFileName+".tmp")
	finalPath := filepath.Join(cacheRoot, metaIndexFileName)
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	p.metaIndexDirty.Store(false)
	p.metaIndexWrites.Store(0)
	p.metaLastSaved.Store(time.Now().UnixNano())
	p.metaIdxReady.Store(true)
	return nil
}

func (p *proxy) markMetaIndexDirty() {
	p.metaIndexDirty.Store(true)
	if p.metaIndexWrites.Add(1) < metaIndexDirtyThreshold {
		return
	}
	if last := p.metaLastSaved.Load(); last > 0 && time.Since(time.Unix(0, last)) < metaIndexSaveMinInterval {
		return
	}
	if p.metaSaveScheduled.CompareAndSwap(false, true) {
		p.metaIndexWrites.Store(0)
		go func() {
			defer p.metaSaveScheduled.Store(false)
			_ = p.saveMetaIndex()
		}()
	}
}

func (p *proxy) flushMetaIndex() {
	if !p.metaIndexDirty.Load() {
		cacheRoot := p.getCacheRoot()
		if p.cachedFiles.Load() == 0 || fileExists(filepath.Join(cacheRoot, metaIndexFileName)) {
			return
		}
	}
	_ = p.saveMetaIndex()
}

func (p *proxy) startMetaIndexFlusher() {
	for range time.Tick(60 * time.Second) {
		if p.metaIndexDirty.Load() {
			_ = p.saveMetaIndex()
		}
	}
}
