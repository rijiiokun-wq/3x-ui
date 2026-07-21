package service

import "sync"

// trafficCollectionGate closes the gap between reading Xray's resetting
// counters and submitting the corresponding database delta.  A cycle repair
// takes the same gate before entering the strict traffic writer, so a delta
// captured before a reset cannot be applied after that reset commits.
var trafficCollectionGate sync.Mutex

// LockTrafficCollection returns an idempotency-free unlock function for the
// narrow GetXrayTraffic -> AddTraffic critical section.  Callers must follow
// the global order gate -> traffic writer -> inbound lock -> DB transaction.
func LockTrafficCollection() func() {
	trafficCollectionGate.Lock()
	return trafficCollectionGate.Unlock
}
