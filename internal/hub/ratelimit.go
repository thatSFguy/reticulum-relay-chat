package hub

// tokenBucket is a per-session rate limiter. Capacity equals the
// configured RateLimitMsgsPerMin; it refills at perMin/60 tokens per
// second. Every inbound packet costs 1.0 token.
type tokenBucket struct {
	capacity float64
	refill   float64 // tokens per second
	tokens   float64
	lastMs   int64
}

// newTokenBucket builds a full bucket. A non-positive perMin disables
// rate limiting (allow always reports true).
func newTokenBucket(perMin int, nowMs int64) *tokenBucket {
	if perMin <= 0 {
		return &tokenBucket{}
	}
	cap := float64(perMin)
	return &tokenBucket{
		capacity: cap,
		refill:   cap / 60.0,
		tokens:   cap,
		lastMs:   nowMs,
	}
}

// allow refills the bucket for the elapsed time, then attempts to spend
// one token. It returns false (and spends nothing) when the bucket is
// exhausted. A disabled bucket (capacity 0) always returns true.
func (b *tokenBucket) allow(nowMs int64) bool {
	if b.capacity <= 0 {
		return true
	}
	if nowMs > b.lastMs {
		elapsed := float64(nowMs-b.lastMs) / 1000.0
		b.tokens += elapsed * b.refill
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastMs = nowMs
	}
	if b.tokens < 1.0 {
		return false
	}
	b.tokens -= 1.0
	return true
}
