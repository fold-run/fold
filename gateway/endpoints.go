package gateway

import (
	"sync"
	"time"
)

// endpointPool balances an upstream's replica endpoints. MCP is sessionful,
// so balancing happens where sessions are born: each new connection (the
// shared root session, or a per-client bridged session) picks the next
// healthy endpoint round-robin, and a connect failure ejects that endpoint
// for a cooldown before it is retried. Ejection is per gateway instance —
// endpoint health is an observation about this instance's network path, not
// shared fleet state.
type endpointPool struct {
	urls     []string
	cooldown time.Duration

	mu        sync.Mutex
	next      int
	downUntil map[string]time.Time
}

func newEndpointPool(urls []string, cooldown time.Duration) *endpointPool {
	return &endpointPool{urls: urls, cooldown: cooldown, downUntil: map[string]time.Time{}}
}

// candidates returns every endpoint in connect-attempt order: healthy ones
// first, rotated round-robin, then cooling ones (least value last). Cooling
// endpoints stay in the list so an upstream whose every replica is ejected
// is still tried rather than declared dead on stale information.
func (p *endpointPool) candidates() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	healthy := make([]string, 0, len(p.urls))
	var cooling []string
	for i := range p.urls {
		u := p.urls[(p.next+i)%len(p.urls)]
		if p.downUntil[u].After(now) {
			cooling = append(cooling, u)
		} else {
			healthy = append(healthy, u)
		}
	}
	p.next = (p.next + 1) % len(p.urls)
	return append(healthy, cooling...)
}

// markDown ejects an endpoint for the cooldown after a connect failure,
// reporting whether this newly took it out of rotation.
func (p *endpointPool) markDown(url string) (ejected bool) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	ejected = !p.downUntil[url].After(now)
	p.downUntil[url] = now.Add(p.cooldown)
	return ejected
}

// markUp clears an endpoint's ejection after a successful connect,
// reporting whether this newly returned it to rotation.
func (p *endpointPool) markUp(url string) (restored bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	restored = p.downUntil[url].After(time.Now())
	delete(p.downUntil, url)
	return restored
}

// all returns every endpoint regardless of health (probe targets).
func (p *endpointPool) all() []string { return p.urls }

// endpointStatus is one endpoint's balancer view, surfaced in /healthz and
// the endpoint-health metric.
type endpointStatus struct {
	URL     string `json:"url,omitempty"`
	Healthy bool   `json:"healthy"`
}

func (p *endpointPool) snapshot(includeURL bool) []endpointStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]endpointStatus, len(p.urls))
	for i, u := range p.urls {
		out[i] = endpointStatus{Healthy: !p.downUntil[u].After(now)}
		if includeURL {
			out[i].URL = u
		}
	}
	return out
}
