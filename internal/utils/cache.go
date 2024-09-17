package utils

import (
	"sync"
	"time"
)

type CacheItem struct {
	value      string
	expiration time.Time
}

type Cache struct {
	data  map[string]CacheItem
	mutex sync.Mutex
	ttl   time.Duration
}

func NewCache(ttl time.Duration) *Cache {
	return &Cache{
		data: make(map[string]CacheItem),
		ttl:  ttl,
	}
}

func (c *Cache) Set(key string, value string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.data[key] = CacheItem{
		value:      value,
		expiration: time.Now().Add(c.ttl),
	}
}

func (c *Cache) Get(key string) (string, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	item, exists := c.data[key]
	if !exists || time.Now().After(item.expiration) {
		delete(c.data, key)
		return "", false
	}
	return item.value, true
}
