package servercounter

import (
	"time"

	"k8s.io/klog/v2"
)

// A CachedServerCounter wraps a ServerCounter to cache its server count value.
// Cache refreshes occur when CountServers() is called after a user-configurable
// cache expiration duration.
type CachedServerCounter struct {
	inner       ServerCounter
	cachedCount int
	expiration  time.Duration
	lastRefresh time.Time
}

// CountServers returns the last cached server count and updates the cached count
// if it has expired since the last call.
func (csc *CachedServerCounter) CountServers() int {
	// Refresh the cache if expiry time has passed since last call.
	if time.Now().Sub(csc.lastRefresh) >= csc.expiration {
		newCount := csc.inner.CountServers()
		if newCount != csc.cachedCount {
			klog.Infof("updated cached server count from %v to %v", csc.cachedCount, newCount)
		}
		csc.lastRefresh = time.Now()
	}

	return csc.cachedCount
}

// NewCachedServerCounter creates a new CachedServerCounter with a given expiration
// time wrapping the provided ServerCounter.
func NewCachedServerCounter(serverCounter ServerCounter, expiration time.Duration) *CachedServerCounter {
	return &CachedServerCounter{
		inner:       serverCounter,
		cachedCount: serverCounter.CountServers(),
		expiration:  expiration,
		lastRefresh: time.Now(),
	}
}
