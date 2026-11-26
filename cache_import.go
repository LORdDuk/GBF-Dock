package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type importPreview struct {
	Path           string `json:"path"`
	ResolvedPath   string `json:"resolvedPath"`
	Format         string `json:"format"`
	ValidFiles     int64  `json:"validFiles"`
	ValidBytes     int64  `json:"validBytes"`
	NewFiles       int64  `json:"newFiles"`
	UpdatableFiles int64  `json:"updatableFiles"`
	SkippedFiles   int64  `json:"skippedFiles"`
	InvalidFiles   int64  `json:"invalidFiles"`
	WillCopyBytes  int64  `json:"willCopyBytes"`
}

type importResult struct {
	ResolvedPath string `json:"resolvedPath"`
	Format       string `json:"format"`
	Added        int64  `json:"added"`
	Updated      int64  `json:"updated"`
	Skipped      int64  `json:"skipped"`
	Invalid      int64  `json:"invalid"`
	CopiedBytes  int64  `json:"copiedBytes"`
	Cancelled    bool   `json:"cancelled,omitempty"`
	Message      string `json:"message"`
}

type importProgress struct {
	Running        bool          `json:"running"`
	Done           bool          `json:"done"`
	Cancelled      bool          `json:"cancelled"`
	Error          string        `json:"error,omitempty"`
	Stage          string        `json:"stage"`
	Processed      int64         `json:"processed"`
	Total          int64         `json:"total"`
	Added          int64         `json:"added"`
	Updated        int64         `json:"updated"`
	Skipped        int64         `json:"skipped"`
	Invalid        int64         `json:"invalid"`
	CopiedBytes    int64         `json:"copiedBytes"`
	TotalCopyBytes int64         `json:"totalCopyBytes"`
	Result         *importResult `json:"result,omitempty"`
}

type inspectedImport struct {
	srcPath    string
	url        string
	hdr        http.Header
	looseQuery bool
	headerLen  int64
	bodySize   int64
	modTime    time.Time
	isRaw      bool
}

func normalizeImportStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "skip":
		return "skip"
	case "overwrite":
		return "overwrite"
	default:
		return "newer"
	}
}

func canonicalCleanPath(p string) string {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return ""
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return filepath.Clean(trimmed)
	}
	if realPath, rErr := filepath.EvalSymlinks(abs); rErr == nil {
		abs = realPath
	}
	return filepath.Clean(abs)
}

func sameCleanPath(a, b string) bool {
	ca := canonicalCleanPath(a)
	cb := canonicalCleanPath(b)
	if ca == "" || cb == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func isSubOrEqualPath(base, target string) bool {
	if runtime.GOOS == "windows" {
		base = strings.ToLower(base)
		target = strings.ToLower(target)
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isSameOrNestedPath(a, b string) bool {
	ca := canonicalCleanPath(a)
	cb := canonicalCleanPath(b)
	if ca == "" || cb == "" {
		return false
	}
	return isSubOrEqualPath(ca, cb) || isSubOrEqualPath(cb, ca)
}

func extractVersionTimestamp(rawURL string) int64 {
	if m := versionSegment.FindString(rawURL); len(m) >= 12 {
		if v, err := strconv.ParseInt(strings.Trim(m, "/"), 10, 64); err == nil {
			return v
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		for _, vals := range u.Query() {
			for _, val := range vals {
				if len(val) == 10 && val[0] == '1' {
					if v, pErr := strconv.ParseInt(val, 10, 64); pErr == nil {
						return v
					}
				}
			}
		}
	}
	return 0
}

func isIgnoredRawImportExt(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	if low == "" || low == "thumbs.db" || low == ".ds_store" || strings.HasPrefix(low, "tmp-") {
		return true
	}
	switch strings.ToLower(filepath.Ext(low)) {
	case ".tmp", ".bak", ".part", ".crdownload", ".exe", ".dll", ".cmd", ".bat", ".ps1",
		".zip", ".7z", ".rar", ".lnk", ".ini", ".db", ".md", ".txt", ".log", ".crt", ".key":
		return true
	}
	return false
}

func isTwoHexName(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func hasNativeCacheBuckets(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	checkedFiles := 0
	for _, e := range entries {
		if !e.IsDir() || !isTwoHexName(e.Name()) {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			if f.IsDir() || isInternalCacheFile(f.Name()) {
				continue
			}
			fp := filepath.Join(dir, e.Name(), f.Name())
			fd, oErr := os.Open(fp)
			if oErr != nil {
				continue
			}
			line, rErr := bufio.NewReaderSize(fd, 1024).ReadBytes('\n')
			_ = fd.Close()
			var m entryMeta
			if rErr == nil && json.Unmarshal(line, &m) == nil && m.URL != "" {
				return true
			}
			checkedFiles++
			if checkedFiles >= 8 {
				return false
			}
		}
	}
	return false
}

func hasRawCDNSubdirs(dir string) bool {
	if assetHost.MatchString(filepath.Base(dir)) {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && assetHost.MatchString(e.Name()) {
			return true
		}
	}
	return false
}

func resolveImportRoot(input, activeCacheDir string) (resolved string, isRawCDN bool, format string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false, "", errors.New("请先选择或输入旧缓存目录路径")
	}
	clean := canonicalCleanPath(trimmed)
	st, statErr := os.Stat(clean)
	if statErr != nil || !st.IsDir() {
		return "", false, "", fmt.Errorf("目录不存在或无法访问：%s", trimmed)
	}
	if activeCacheDir != "" && isSameOrNestedPath(clean, activeCacheDir) {
		return "", false, "", errors.New("不能导入当前正在使用的缓存目录（或其父目录/子目录）")
	}

	subCache := filepath.Join(clean, "cache")
	if cst, cErr := os.Stat(subCache); cErr == nil && cst.IsDir() {
		if activeCacheDir != "" && isSameOrNestedPath(subCache, activeCacheDir) {
			return "", false, "", errors.New("不能导入当前正在使用的缓存目录")
		}
		if hasNativeCacheBuckets(subCache) {
			return subCache, false, "GBF-Dock / 旧版原生缓存", nil
		}
		if hasRawCDNSubdirs(subCache) {
			return subCache, true, "原始 CDN 目录结构（兼容查询串）", nil
		}
	}

	if hasNativeCacheBuckets(clean) {
		return clean, false, "GBF-Dock / 旧版原生缓存", nil
	}
	if hasRawCDNSubdirs(clean) {
		return clean, true, "原始 CDN 目录结构（兼容查询串）", nil
	}
	return clean, false, "未识别到有效缓存", nil
}

func inspectImportFile(root, filePath string, isRawCDN bool) (inspectedImport, bool) {
	info, err := os.Stat(filePath)
	if err != nil || info.IsDir() || isInternalCacheFile(info.Name()) {
		return inspectedImport{}, false
	}
	if !isRawCDN {
		f, oErr := os.Open(filePath)
		if oErr != nil {
			return inspectedImport{}, false
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
		if lErr != nil || m.URL == "" {
			return inspectedImport{}, false
		}
		u, uErr := url.Parse(m.URL)
		if uErr != nil || !assetHost.MatchString(u.Hostname()) {
			return inspectedImport{}, false
		}
		bodySize := info.Size() - int64(len(line))
		if bodySize <= 0 {
			return inspectedImport{}, false
		}
		if ok, _ := validateAssetBytes(u.Path, m.Header, head[:hn], bodySize <= int64(hn)); !ok {
			return inspectedImport{}, false
		}
		return inspectedImport{
			srcPath:    filePath,
			url:        m.URL,
			hdr:        m.Header.Clone(),
			looseQuery: m.LooseQuery,
			headerLen:  int64(len(line)),
			bodySize:   bodySize,
			modTime:    info.ModTime(),
			isRaw:      false,
		}, true
	}

	if isIgnoredRawImportExt(info.Name()) || info.Size() <= 0 {
		return inspectedImport{}, false
	}
	rel, err := filepath.Rel(root, filePath)
	if err != nil {
		return inspectedImport{}, false
	}
	slashRel := filepath.ToSlash(rel)
	var host, urlPath string
	rootBase := filepath.Base(root)
	if assetHost.MatchString(rootBase) {
		host = rootBase
		urlPath = "/" + strings.TrimLeft(slashRel, "/")
	} else {
		h, rest, ok := strings.Cut(slashRel, "/")
		if !ok || !assetHost.MatchString(h) || rest == "" {
			return inspectedImport{}, false
		}
		host = h
		urlPath = "/" + strings.TrimLeft(rest, "/")
	}
	if urlPath == "/" {
		return inspectedImport{}, false
	}
	rawURL := "https://" + host + urlPath
	hdr := http.Header{}
	if ct := mime.TypeByExtension(path.Ext(urlPath)); ct != "" {
		hdr.Set("Content-Type", ct)
	}

	f, oErr := os.Open(filePath)
	if oErr != nil {
		return inspectedImport{}, false
	}
	var head [512]byte
	hn, _ := io.ReadFull(f, head[:])
	_ = f.Close()
	if ok, _ := validateAssetBytes(urlPath, hdr, head[:hn], info.Size() <= int64(hn)); !ok {
		return inspectedImport{}, false
	}
	return inspectedImport{
		srcPath:    filePath,
		url:        rawURL,
		hdr:        hdr,
		looseQuery: true,
		headerLen:  0,
		bodySize:   info.Size(),
		modTime:    info.ModTime(),
		isRaw:      true,
	}, true
}

func (p *proxy) readExistingCacheMeta(dstPath string) (rawURL string, modTime time.Time, ok bool) {
	if v, loaded := p.metaIdx.Load(dstPath); loaded {
		cm := v.(*cachedMeta)
		if st, err := os.Stat(dstPath); err == nil && st.Size() == cm.headerLen+cm.bodySize {
			return cm.url, st.ModTime(), true
		}
	}
	st, err := os.Stat(dstPath)
	if err != nil {
		return "", time.Time{}, false
	}
	f, err := os.Open(dstPath)
	if err != nil {
		return "", time.Time{}, false
	}
	defer f.Close()
	line, lErr := bufio.NewReaderSize(f, 1024).ReadBytes('\n')
	if lErr != nil {
		return "", time.Time{}, false
	}
	var m entryMeta
	if json.Unmarshal(line, &m) != nil || m.URL == "" {
		return "", time.Time{}, false
	}
	return m.URL, st.ModTime(), true
}

func shouldReplaceExisting(strategy, srcURL string, srcMod time.Time, dstPath string, p *proxy) string {
	dstURL, dstMod, exists := p.readExistingCacheMeta(dstPath)
	if !exists {
		return "add"
	}
	switch normalizeImportStrategy(strategy) {
	case "skip":
		return "skip"
	case "overwrite":
		return "update"
	default: // "newer"
		srcVer := extractVersionTimestamp(srcURL)
		dstVer := extractVersionTimestamp(dstURL)
		if srcVer > 0 && dstVer > 0 && srcVer != dstVer {
			if srcVer > dstVer {
				return "update"
			}
			return "skip"
		}
		if srcVer > 0 && dstVer == 0 {
			return "update"
		}
		if normalizeCDNShardURL(srcURL) != normalizeCDNShardURL(dstURL) && srcMod.After(dstMod) {
			return "update"
		}
		return "skip"
	}
}

func (p *proxy) previewImportCache(inputPath, strategy string) (importPreview, error) {
	strat := normalizeImportStrategy(strategy)
	resolved, isRawCDN, format, err := resolveImportRoot(inputPath, p.getCacheRoot())
	if err != nil {
		return importPreview{}, err
	}
	rep := importPreview{
		Path:         inputPath,
		ResolvedPath: resolved,
		Format:       format,
	}
	if format == "未识别到有效缓存" {
		return rep, nil
	}

	_ = filepath.WalkDir(resolved, func(fp string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() || isInternalCacheFile(d.Name()) {
			return nil
		}
		if isRawCDN && isIgnoredRawImportExt(d.Name()) {
			return nil
		}
		item, ok := inspectImportFile(resolved, fp, isRawCDN)
		if !ok {
			rep.InvalidFiles++
			return nil
		}
		rep.ValidFiles++
		rep.ValidBytes += item.bodySize
		dst := p.cachePath(item.url)
		switch shouldReplaceExisting(strat, item.url, item.modTime, dst, p) {
		case "add":
			rep.NewFiles++
			rep.WillCopyBytes += item.bodySize
		case "update":
			rep.UpdatableFiles++
			rep.WillCopyBytes += item.bodySize
		default:
			rep.SkippedFiles++
		}
		return nil
	})
	return rep, nil
}

func (p *proxy) copyCacheEntryAtomic(dst string, item inspectedImport) (writtenBytes int64, err error) {
	srcFile, err := os.Open(item.srcPath)
	if err != nil {
		return 0, err
	}
	defer srcFile.Close()

	if item.headerLen > 0 {
		if _, err := srcFile.Seek(item.headerLen, io.SeekStart); err != nil {
			return 0, err
		}
	}

	hdr := http.Header{}
	copyHeader(hdr, item.hdr, "Set-Cookie", "Date", "Age")
	metaBytes, err := json.Marshal(entryMeta{
		URL:        item.url,
		Header:     hdr,
		LooseQuery: item.looseQuery,
	})
	if err != nil {
		return 0, err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "tmp-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	headerLine := append(metaBytes, '\n')
	if _, err := tmp.Write(headerLine); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	copiedBody, err := io.Copy(tmp, io.LimitReader(srcFile, item.bodySize))
	if err != nil || copiedBody != item.bodySize {
		_ = tmp.Close()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}

	p.usageMu.Lock()
	var oldSize int64
	existed := false
	if st, statErr := os.Stat(dst); statErr == nil {
		existed = true
		oldSize = st.Size()
	}
	renameErr := os.Rename(tmp.Name(), dst)
	if renameErr == nil {
		_ = os.Chtimes(dst, item.modTime, item.modTime)
		if st, statErr := os.Stat(dst); statErr == nil {
			p.cachedBytes.Add(st.Size() - oldSize)
			if !existed {
				p.cachedFiles.Add(1)
			}
			p.metaIdx.Store(dst, &cachedMeta{
				url:        item.url,
				header:     hdr.Clone(),
				headerLen:  int64(len(headerLine)),
				bodySize:   item.bodySize,
				modTime:    item.modTime,
				lastAccess: item.modTime,
				looseQuery: item.looseQuery,
			})
			if p.ram != nil {
				p.ram.Delete(dst)
			}
			p.markMetaIndexDirty()
			writtenBytes = st.Size()
		}
	}
	p.usageMu.Unlock()
	if renameErr != nil {
		return 0, renameErr
	}
	return writtenBytes, nil
}

func (p *proxy) mergeCacheFrom(ctx context.Context, inputPath, strategy string, onProgress func(importProgress)) (importResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	strat := normalizeImportStrategy(strategy)
	resolved, isRawCDN, format, err := resolveImportRoot(inputPath, p.getCacheRoot())
	if err != nil {
		return importResult{}, err
	}
	res := importResult{
		ResolvedPath: resolved,
		Format:       format,
	}
	if format == "未识别到有效缓存" {
		res.Message = "未在该目录检测到有效的 GBF 静态资源缓存。"
		return res, nil
	}

	var prog importProgress
	prog.Running = true
	prog.Stage = "正在扫描待并入文件…"
	if onProgress != nil {
		onProgress(prog)
	}

	type queuedItem struct {
		dst    string
		action string
		item   inspectedImport
	}
	var queue []queuedItem
	walkErr := filepath.WalkDir(resolved, func(fp string, d fs.DirEntry, wErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if wErr != nil || d.IsDir() || isInternalCacheFile(d.Name()) {
			return nil
		}
		if isRawCDN && isIgnoredRawImportExt(d.Name()) {
			return nil
		}
		item, ok := inspectImportFile(resolved, fp, isRawCDN)
		if !ok {
			res.Invalid++
			prog.Invalid = res.Invalid
			return nil
		}
		dst := p.cachePath(item.url)
		act := shouldReplaceExisting(strat, item.url, item.modTime, dst, p)
		if act == "skip" {
			res.Skipped++
			prog.Skipped = res.Skipped
			return nil
		}
		queue = append(queue, queuedItem{dst: dst, action: act, item: item})
		prog.TotalCopyBytes += item.bodySize
		return nil
	})

	if walkErr != nil && errors.Is(walkErr, context.Canceled) {
		res.Cancelled = true
		res.Message = "已取消扫描，未修改当前缓存。"
		return res, nil
	}

	prog.Total = int64(len(queue))
	prog.Stage = "正在写入并合并缓存…"
	if onProgress != nil {
		onProgress(prog)
	}

	for i, q := range queue {
		if ctx.Err() != nil {
			res.Cancelled = true
			break
		}
		if _, copyErr := p.copyCacheEntryAtomic(q.dst, q.item); copyErr == nil {
			if q.action == "add" {
				res.Added++
				prog.Added = res.Added
			} else {
				res.Updated++
				prog.Updated = res.Updated
			}
			res.CopiedBytes += q.item.bodySize
			prog.CopiedBytes = res.CopiedBytes
		} else {
			res.Invalid++
			prog.Invalid = res.Invalid
		}
		prog.Processed = int64(i + 1)
		if onProgress != nil && (i%8 == 0 || i+1 == len(queue)) {
			onProgress(prog)
		}
	}

	p.scanCacheUsage()
	if res.Added+res.Updated > 0 {
		go func() {
			_ = p.saveMetaIndex()
		}()
	}
	if res.Cancelled {
		res.Message = fmt.Sprintf("已取消并入（已安全保留本次已完成的 %d 个新增、%d 个更新文件）。", res.Added, res.Updated)
	} else {
		res.Message = fmt.Sprintf("缓存并入完成：新增 %d 个，更新 %d 个，跳过 %d 个，过滤坏档 %d 个（写入 %.1f MB）。",
			res.Added, res.Updated, res.Skipped, res.Invalid, float64(res.CopiedBytes)/(1024*1024))
	}
	return res, nil
}

func (p *proxy) detectOldCacheDirs() []importPreview {
	activeCache := canonicalCleanPath(p.getCacheRoot())
	var searchRoots []string
	if p.dir != "" {
		parent := filepath.Dir(canonicalCleanPath(p.dir))
		if parent != "" {
			searchRoots = append(searchRoots, parent)
		}
	}
	searchRoots = append(searchRoots, userCommonFolders()...)

	candidates := make(map[string]struct{})
	dirsVisited := 0
	const maxDirs = 180

	addCand := func(dir string) {
		clean := canonicalCleanPath(dir)
		if clean == "" || isSameOrNestedPath(clean, activeCache) {
			return
		}
		if runtime.GOOS == "windows" {
			candidates[strings.ToLower(clean)] = struct{}{}
		} else {
			candidates[clean] = struct{}{}
		}
	}

	var candPaths []string
	seenRoot := make(map[string]bool)
	for _, root := range searchRoots {
		rc := canonicalCleanPath(root)
		if rc == "" {
			continue
		}
		key := rc
		if runtime.GOOS == "windows" {
			key = strings.ToLower(rc)
		}
		if seenRoot[key] {
			continue
		}
		seenRoot[key] = true

		entries, err := os.ReadDir(rc)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || dirsVisited >= maxDirs {
				continue
			}
			dirsVisited++
			sub := filepath.Join(rc, e.Name())
			lowName := strings.ToLower(e.Name())
			if strings.Contains(lowName, "gbf") || strings.Contains(lowName, "granblue") || strings.Contains(lowName, "dock") || lowName == "cache" || assetHost.MatchString(e.Name()) {
				candPaths = append(candPaths, sub)
				continue
			}
			// 再向下检查一层是否直接放着 cache/ 或域名文件夹
			subEntries, sErr := os.ReadDir(sub)
			if sErr != nil {
				continue
			}
			for _, se := range subEntries {
				if !se.IsDir() || dirsVisited >= maxDirs {
					continue
				}
				dirsVisited++
				sn := strings.ToLower(se.Name())
				if sn == "cache" || strings.Contains(sn, "gbf") || assetHost.MatchString(se.Name()) {
					candPaths = append(candPaths, sub)
					break
				}
			}
		}
	}
	_ = addCand

	seenResolved := make(map[string]bool)
	var out []importPreview
	for _, cp := range candPaths {
		prev, err := p.previewImportCache(cp, "newer")
		if err != nil || prev.ValidFiles <= 0 {
			continue
		}
		rk := canonicalCleanPath(prev.ResolvedPath)
		if runtime.GOOS == "windows" {
			rk = strings.ToLower(rk)
		}
		if seenResolved[rk] {
			continue
		}
		seenResolved[rk] = true
		out = append(out, prev)
		if len(out) >= 12 {
			break
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ValidFiles != out[j].ValidFiles {
			return out[i].ValidFiles > out[j].ValidFiles
		}
		return out[i].ValidBytes > out[j].ValidBytes
	})
	return out
}

func (a *app) startImportJob(path, strategy string) error {
	a.importMu.Lock()
	if a.importState.Running {
		a.importMu.Unlock()
		return errors.New("已有缓存并入任务正在进行中，请等待完成或先取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.importCancel = cancel
	a.importState = importProgress{
		Running: true,
		Stage:   "准备扫描旧缓存目录…",
	}
	a.importMu.Unlock()

	go func() {
		defer func() {
			cancel()
			a.importMu.Lock()
			a.importCancel = nil
			a.importMu.Unlock()
		}()
		res, err := a.p.mergeCacheFrom(ctx, path, strategy, func(p importProgress) {
			a.importMu.Lock()
			a.importState = p
			a.importMu.Unlock()
		})
		a.importMu.Lock()
		if err != nil {
			a.importState.Running = false
			a.importState.Done = true
			a.importState.Error = err.Error()
			a.importState.Stage = "并入失败"
			a.importMu.Unlock()
			log.Printf("操作失败（cache-import-merge）：%v", err)
			return
		}
		a.importState.Running = false
		a.importState.Done = true
		a.importState.Cancelled = res.Cancelled
		a.importState.Added = res.Added
		a.importState.Updated = res.Updated
		a.importState.Skipped = res.Skipped
		a.importState.Invalid = res.Invalid
		a.importState.CopiedBytes = res.CopiedBytes
		a.importState.Result = &res
		if res.Cancelled {
			a.importState.Stage = "已取消"
		} else {
			a.importState.Stage = "并入完成"
		}
		a.importMu.Unlock()
		if res.Message != "" {
			log.Print(res.Message)
		}
	}()
	return nil
}

func (a *app) getImportProgress() importProgress {
	a.importMu.Lock()
	defer a.importMu.Unlock()
	cp := a.importState
	if a.importState.Result != nil {
		r := *a.importState.Result
		cp.Result = &r
	}
	return cp
}

func (a *app) cancelImportJob() bool {
	a.importMu.Lock()
	cancel := a.importCancel
	running := a.importState.Running
	a.importMu.Unlock()
	if running && cancel != nil {
		cancel()
		return true
	}
	return false
}
