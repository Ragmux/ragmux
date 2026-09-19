package provider

import (
	"container/list"
	"sync"
	"time"
)

// imageCache is a small in-memory LRU of fetched images. A multi-turn
// conversation resends the same image part on every turn, so without it a
// long chat downloads the same bytes once per message — the cost the inlining
// adapters pay for not being able to forward a URL.
//
// It is deliberately not a disk cache: it holds only what one process is
// working on and is gone on restart. It is keyed on the URL alone, so an
// image whose content changes within the TTL keeps serving the old bytes.
type imageCache struct {
	mu sync.Mutex
	// order holds *imageEntry, most recently used at the front.
	order      *list.List
	entries    map[string]*list.Element
	maxEntries int
	maxBytes   int64
	ttl        time.Duration
	bytes      int64
}

type imageEntry struct {
	url       string
	mediaType string
	data      string
	bytes     int64
	expires   time.Time
}

// NewImageCache builds the cache an ImageFetcher stores fetched images in.
// The type stays unexported because nothing outside the package has any use
// for it beyond assigning it to ImageFetcher.Cache; the byte ceiling is not a
// setting, only the entry count and the TTL are.
func NewImageCache(maxEntries int, ttl time.Duration) *imageCache {
	return newImageCache(maxEntries, 0, ttl)
}

// newImageCache builds a cache; a non-positive argument takes the default
// (64 entries, 64 MiB, 10 minutes).
func newImageCache(maxEntries int, maxBytes int64, ttl time.Duration) *imageCache {
	c := &imageCache{order: list.New(), entries: map[string]*list.Element{},
		maxEntries: maxEntries, maxBytes: maxBytes, ttl: ttl}
	if c.maxEntries <= 0 {
		c.maxEntries = 64
	}
	if c.maxBytes <= 0 {
		c.maxBytes = 64 << 20
	}
	if c.ttl <= 0 {
		c.ttl = 10 * time.Minute
	}
	return c
}

// get is nil-safe: a disabled cache simply never hits.
func (c *imageCache) get(url string) (string, string, bool) {
	if c == nil {
		return "", "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[url]
	if !ok {
		return "", "", false
	}
	e := el.Value.(*imageEntry)
	if time.Now().After(e.expires) {
		c.remove(el)
		return "", "", false
	}
	c.order.MoveToFront(el)
	return e.mediaType, e.data, true
}

func (c *imageCache) put(url, mediaType, data string) {
	if c == nil {
		return
	}
	size := int64(len(data))
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[url]; ok {
		c.remove(el)
	}
	el := c.order.PushFront(&imageEntry{url: url, mediaType: mediaType, data: data,
		bytes: size, expires: time.Now().Add(c.ttl)})
	c.entries[url] = el
	c.bytes += size
	for c.order.Len() > c.maxEntries || c.bytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.remove(back)
	}
}

// remove drops one element; the caller holds the lock.
func (c *imageCache) remove(el *list.Element) {
	e := el.Value.(*imageEntry)
	c.order.Remove(el)
	delete(c.entries, e.url)
	c.bytes -= e.bytes
}
