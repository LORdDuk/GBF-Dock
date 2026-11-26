package main

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

const (
	defaultRAMCacheMaxBytes     int64 = 96 << 20  // 96 MB RAM 热缓存总上限
	defaultRAMCacheMaxItemBytes int64 = 512 << 10 // 单文件 <= 512 KB 才准入 RAM，防止大音频冲刷高频脚本与小图
)

type ramEntry struct {
	key        string
	url        string
	header     http.Header
	body       []byte
	modTime    time.Time
	lastAccess time.Time
	looseQuery bool
	size       int64
	protected  bool
	elem       *list.Element
}

// slruCache 轻量级分段 LRU（Probation 25% + Protected 75%），单锁实现，避免复杂分片引入并发维护成本。
type slruCache struct {
	mu           sync.Mutex
	maxBytes     int64
	maxItemBytes int64
	probLimit    int64
	protLimit    int64
	probBytes    int64
	protBytes    int64
	probList     *list.List
	protList     *list.List
	items        map[string]*ramEntry
}

func newSLRUCache(maxBytes, maxItemBytes int64) *slruCache {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if maxItemBytes < 0 {
		maxItemBytes = 0
	}
	protLimit := (maxBytes * 3) / 4
	probLimit := maxBytes - protLimit
	return &slruCache{
		maxBytes:     maxBytes,
		maxItemBytes: maxItemBytes,
		probLimit:    probLimit,
		protLimit:    protLimit,
		probList:     list.New(),
		protList:     list.New(),
		items:        make(map[string]*ramEntry),
	}
}

func (c *slruCache) removeLocked(e *ramEntry) {
	if e == nil {
		return
	}
	if e.protected {
		c.protList.Remove(e.elem)
		c.protBytes -= e.size
	} else {
		c.probList.Remove(e.elem)
		c.probBytes -= e.size
	}
	delete(c.items, e.key)
	e.elem = nil
}

func (c *slruCache) enforceBudgetLocked() {
	for c.protBytes > c.protLimit && c.protList.Len() > 0 {
		back := c.protList.Back()
		if back == nil {
			break
		}
		v := back.Value.(*ramEntry)
		c.protList.Remove(back)
		c.protBytes -= v.size
		v.protected = false
		v.elem = c.probList.PushFront(v)
		c.probBytes += v.size
	}
	for c.probBytes+c.protBytes > c.maxBytes && c.probList.Len() > 0 {
		back := c.probList.Back()
		if back == nil {
			break
		}
		c.removeLocked(back.Value.(*ramEntry))
	}
	for c.probBytes+c.protBytes > c.maxBytes && c.protList.Len() > 0 {
		back := c.protList.Back()
		if back == nil {
			break
		}
		c.removeLocked(back.Value.(*ramEntry))
	}
}

func (c *slruCache) Get(key string) (*ramEntry, bool) {
	if c == nil || c.maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	e, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	if e.protected {
		c.protList.MoveToFront(e.elem)
	} else {
		c.probList.Remove(e.elem)
		c.probBytes -= e.size
		e.protected = true
		e.elem = c.protList.PushFront(e)
		c.protBytes += e.size
		c.enforceBudgetLocked()
	}
	snap := &ramEntry{
		key:        e.key,
		url:        e.url,
		header:     e.header.Clone(),
		body:       e.body, // body 在写入时已做不可变副本拷贝
		modTime:    e.modTime,
		lastAccess: e.lastAccess,
		looseQuery: e.looseQuery,
		size:       e.size,
	}
	c.mu.Unlock()
	return snap, true
}

func (c *slruCache) Put(key, rawURL string, hdr http.Header, body []byte, modTime time.Time, looseQuery bool) {
	if c == nil {
		return
	}
	sz := int64(len(body))
	if sz <= 0 || c.maxBytes <= 0 || c.maxItemBytes <= 0 || sz > c.maxItemBytes || sz > c.maxBytes {
		c.Delete(key)
		return
	}
	bodyCopy := append([]byte(nil), body...)
	hdrCopy := hdr.Clone()

	c.mu.Lock()
	if old, exists := c.items[key]; exists {
		c.removeLocked(old)
	}
	e := &ramEntry{
		key:        key,
		url:        rawURL,
		header:     hdrCopy,
		body:       bodyCopy,
		modTime:    modTime,
		lastAccess: time.Now(),
		looseQuery: looseQuery,
		size:       sz,
		protected:  false,
	}
	e.elem = c.probList.PushFront(e)
	c.probBytes += sz
	c.items[key] = e
	c.enforceBudgetLocked()
	c.mu.Unlock()
}

func (c *slruCache) Touch(key string, modTime time.Time) {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		e.modTime = modTime
		if modTime.After(e.lastAccess) {
			e.lastAccess = modTime
		}
	}
	c.mu.Unlock()
}

func (c *slruCache) TouchAccess(key string, accessTime time.Time) {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	c.mu.Lock()
	if e, ok := c.items[key]; ok && accessTime.After(e.lastAccess) {
		e.lastAccess = accessTime
	}
	c.mu.Unlock()
}

func (c *slruCache) Delete(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		c.removeLocked(e)
	}
	c.mu.Unlock()
}

func (c *slruCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.probList.Init()
	c.protList.Init()
	c.items = make(map[string]*ramEntry)
	c.probBytes = 0
	c.protBytes = 0
	c.mu.Unlock()
}

func (c *slruCache) Stats() (items int, usedBytes, maxBytes int64) {
	if c == nil {
		return 0, 0, 0
	}
	c.mu.Lock()
	items = len(c.items)
	usedBytes = c.probBytes + c.protBytes
	maxBytes = c.maxBytes
	c.mu.Unlock()
	return items, usedBytes, maxBytes
}
