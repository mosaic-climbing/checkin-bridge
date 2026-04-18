package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGet_Miss(t *testing.T) {
	c := newHTMLCache()
	body, hit := c.Get("nonexistent")
	if hit || body != nil {
		t.Errorf("Expected miss on nonexistent key, got body=%v hit=%v", body, hit)
	}
}

func TestSet_ThenGet_Hit(t *testing.T) {
	c := newHTMLCache()
	key := "test-key"
	expectedBody := []byte("<p>test</p>")

	c.Set(key, expectedBody, 1*time.Second)
	body, hit := c.Get(key)

	if !hit {
		t.Errorf("Expected hit after Set, got miss")
	}
	if string(body) != string(expectedBody) {
		t.Errorf("Expected body %q, got %q", expectedBody, body)
	}
}

func TestGet_AfterTTL_Miss(t *testing.T) {
	c := newHTMLCache()
	key := "ttl-test"
	body := []byte("<p>expires</p>")

	c.Set(key, body, 10*time.Millisecond)

	// Should be a hit immediately
	_, hit := c.Get(key)
	if !hit {
		t.Errorf("Expected hit immediately after Set")
	}

	// Wait for TTL to expire
	time.Sleep(20 * time.Millisecond)

	// Should be a miss now
	_, hit = c.Get(key)
	if hit {
		t.Errorf("Expected miss after TTL expiry")
	}
}

func TestInvalidate_ForcesMiss(t *testing.T) {
	c := newHTMLCache()
	key := "invalidate-test"
	body := []byte("<p>invalidated</p>")

	c.Set(key, body, 1*time.Hour)
	_, hit := c.Get(key)
	if !hit {
		t.Errorf("Expected hit before Invalidate")
	}

	c.Invalidate()

	_, hit = c.Get(key)
	if hit {
		t.Errorf("Expected miss after Invalidate")
	}
}

func TestInvalidate_NewSetWorks(t *testing.T) {
	c := newHTMLCache()
	key := "new-set-test"

	c.Set(key, []byte("<p>old</p>"), 1*time.Hour)
	c.Invalidate()

	// After invalidation, a new Set should work and be gettable
	newBody := []byte("<p>new</p>")
	c.Set(key, newBody, 1*time.Hour)

	body, hit := c.Get(key)
	if !hit {
		t.Errorf("Expected hit on fresh Set after Invalidate")
	}
	if string(body) != string(newBody) {
		t.Errorf("Expected new body %q, got %q", newBody, body)
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	c := newHTMLCache()
	const numGoroutines = 10
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	var readCount, writeCount, invalidateCount atomic.Int32

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				switch j % 3 {
				case 0:
					key := "key-" + string(rune(id%5))
					c.Set(key, []byte("body"), 1*time.Second)
					writeCount.Add(1)
				case 1:
					key := "key-" + string(rune(id%5))
					c.Get(key)
					readCount.Add(1)
				case 2:
					c.Invalidate()
					invalidateCount.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()

	if readCount.Load() == 0 || writeCount.Load() == 0 || invalidateCount.Load() == 0 {
		t.Logf("Completed: reads=%d, writes=%d, invalidates=%d",
			readCount.Load(), writeCount.Load(), invalidateCount.Load())
	}
}
