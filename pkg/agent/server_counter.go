package agent

import "k8s.io/klog/v2"

// ServerCounter is the interface for determining the server count.
type ServerCounter interface {
	Count() int
}

// responseBasedCounter determines server count based on server responses only.
type responseBasedCounter struct {
	cs *ClientSet
}

// NewResponseBasedCounter creates a new responseBasedCounter.
func NewResponseBasedCounter(cs *ClientSet) ServerCounter {
	return &responseBasedCounter{cs: cs}
}

func (rbc *responseBasedCounter) Count() int {
	serverCount := rbc.cs.lastReceivedServerCount
	if serverCount == 0 {
		return 1 // Fallback
	}
	return serverCount
}

// AggregateServerCounter determines server count based on leases and/or server responses.
type aggregateServerCounter struct {
	leaseCounter    ServerCounter // may be nil
	responseCounter ServerCounter
	source          string
}

// NewAggregateServerCounter creates a new aggregateServerCounter.
func NewAggregateServerCounter(leaseCounter, responseCounter ServerCounter, source string) ServerCounter {
	return &aggregateServerCounter{
		leaseCounter:    leaseCounter,
		responseCounter: responseCounter,
		source:          source,
	}
}

func (asc *aggregateServerCounter) Count() int {
	var serverCount int
	var countSourceLabel string

	countFromResponses := asc.responseCounter.Count()

	// If lease counting is not enabled, we can only use responses.
	if asc.leaseCounter == nil {
		serverCount = countFromResponses
		countSourceLabel = fromResponses
		if serverCount == 0 {
			countSourceLabel = fromFallback
		}
		klog.V(4).InfoS("Determined server count", "serverCount", serverCount, "source", countSourceLabel)
		return serverCount
	}

	countFromLeases := asc.leaseCounter.Count()

	switch asc.source {
	case "max":
		serverCount = countFromLeases
		countSourceLabel = fromLeases
		if countFromResponses > serverCount {
			serverCount = countFromResponses
			countSourceLabel = fromResponses
		}
	default: // "default" or empty string
		serverCount = countFromLeases
		countSourceLabel = fromLeases
	}

	if serverCount == 0 {
		serverCount = 1
		countSourceLabel = fromFallback
	}

	klog.V(4).InfoS("Determined server count", "serverCount", serverCount, "source", countSourceLabel)
	return serverCount
}
