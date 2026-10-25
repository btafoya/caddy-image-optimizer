package imageoptimizer

import (
	"container/list"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// diskCache stores variants as files named <hash>.<ext> under <dir>/<hash[:2]>/.
// An in-memory LRU index tracks sizes; it is rebuilt from file mtimes at open.
// Writes are temp-file + rename, so readers never see partial files.
type diskCache struct {
	dir string
	max int64

	mu    sync.Mutex
	ll    *list.List // front = most recently used
	items map[string]*list.Element
	total int64
}

type cacheEntry struct {
	name    string
	size    int64
	touched time.Time
}

func openCache(dir string, max int64) (*diskCache, error) {
	tmp := filepath.Join(dir, ".tmp")
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	c := &diskCache{dir: dir, max: max, ll: list.New(), items: map[string]*list.Element{}}

	var found []cacheEntry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		found = append(found, cacheEntry{d.Name(), info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].touched.Before(found[j].touched) })
	for _, e := range found { // oldest first, so the newest ends up at the front
		e := e
		c.items[e.name] = c.ll.PushFront(&e)
		c.total += e.size
	}
	c.evict()
	return c, nil
}

func (c *diskCache) path(name string) string { return filepath.Join(c.dir, name[:2], name) }

// get returns the file path for name and marks it recently used.
func (c *diskCache) get(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[name]
	if !ok {
		return "", false
	}
	p := c.path(name)
	if _, err := os.Stat(p); err != nil { // deleted behind our back
		c.remove(el)
		return "", false
	}
	c.ll.MoveToFront(el)
	// Persist recency for the next startup scan, at most once a minute per entry.
	if e := el.Value.(*cacheEntry); time.Since(e.touched) > time.Minute {
		e.touched = time.Now()
		_ = os.Chtimes(p, e.touched, e.touched)
	}
	return p, true
}

// put stores data and evicts least-recently-used entries over the size cap.
func (c *diskCache) put(name string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(c.dir, ".tmp"), "w-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return "", werr
	}
	p := c.path(name)
	if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[name]; ok {
		c.remove(el, false)
	}
	c.items[name] = c.ll.PushFront(&cacheEntry{name, int64(len(data)), time.Now()})
	c.total += int64(len(data))
	c.evict()
	return p, nil
}

// remove drops an entry from the index and, unless keepFile is given, from disk.
func (c *diskCache) remove(el *list.Element, keepFile ...bool) {
	e := c.ll.Remove(el).(*cacheEntry)
	delete(c.items, e.name)
	c.total -= e.size
	if len(keepFile) == 0 {
		_ = os.Remove(c.path(e.name))
	}
}

// evict runs under c.mu (or at open). Over the cap, shrink to 90% of it.
// ponytail: inline on put (a miss already paid for a transform); background
// sweeper if delete latency ever shows up. Always keeps the newest entry.
func (c *diskCache) evict() {
	if c.total <= c.max {
		return
	}
	for c.total > c.max/10*9 && c.ll.Len() > 1 {
		c.remove(c.ll.Back())
	}
}
