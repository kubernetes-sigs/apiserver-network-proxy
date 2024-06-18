package servercounter

import (
	"testing"
	"time"
)

type MockServerCounter struct {
	NumCalls int
	Count    int
}

func (msc *MockServerCounter) CountServers() int {
	msc.NumCalls += 1
	return msc.Count
}

func TestCachedServerCounter(t *testing.T) {
	initialCount := 3
	mockCounter := &MockServerCounter{NumCalls: 0, Count: initialCount}

	cacheExpiry := time.Second // This can be tuned down to make this test run faster, just don't make it too small!
	cachedCounter := NewCachedServerCounter(mockCounter, cacheExpiry)

	if mockCounter.NumCalls != 1 {
		t.Errorf("inner server counter have been called once during cached counter creation, got %v calls isntead", mockCounter.NumCalls)
	}

	// Updates in the underlying ServerCounter should not matter until the cache expires.
	callAttemptsWithoutRefresh := 5
	for i := 0; i < callAttemptsWithoutRefresh; i++ {
		mockCounter.Count = 100 + i
		time.Sleep(cacheExpiry / time.Duration(callAttemptsWithoutRefresh) / time.Duration(2))
		got := cachedCounter.CountServers()
		if got != initialCount {
			t.Errorf("server count should not have been updated yet; wanted: %v, got %v", initialCount, got)
		} else if mockCounter.NumCalls != 1 {
			t.Errorf("inner server counter should not have been called yet; expected 1 call, got %v instead", mockCounter.NumCalls)
		}
	}

	// Once the cache expires, the cached count should update by calling the underlying
	// ServerCounter.
	mockCounter.Count = 5
	time.Sleep(cacheExpiry)
	got := cachedCounter.CountServers()
	if got != initialCount {
		t.Errorf("server count should have been updated yet; wanted: %v, got %v", mockCounter.Count, got)
	} else if mockCounter.NumCalls != 2 {
		t.Errorf("inner server counter should have been called one more time; expected 2 calls, got %v instead", mockCounter.NumCalls)
	}
}
