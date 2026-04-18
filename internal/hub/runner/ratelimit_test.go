package runner

import "testing"

func TestRateLimiterBurstThenBlock(t *testing.T) {
	rl := newRateLimiter(5) // 5/min
	allowed := 0
	for i := 0; i < 10; i++ {
		if rl.allow("h1") {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("want 5 burst tokens, got %d", allowed)
	}
}

func TestRateLimiterPerKey(t *testing.T) {
	rl := newRateLimiter(2)
	rl.allow("a")
	rl.allow("a")
	if rl.allow("a") {
		t.Fatal("a should be exhausted")
	}
	if !rl.allow("b") {
		t.Fatal("b is independent")
	}
}
