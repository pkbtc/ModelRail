package main

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type principalContextKey struct{}

func principalFromContext(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalContextKey{}).(Principal)
	return principal
}

type tokenBucket struct {
	tokens   float64
	updated  time.Time
	lastSeen time.Time
}

type RateLimiter struct {
	mu           sync.Mutex
	capacity     float64
	refillPerSec float64
	buckets      map[string]*tokenBucket
}

func NewRateLimiter(perMinute int) *RateLimiter {
	return &RateLimiter{
		capacity:     float64(perMinute),
		refillPerSec: float64(perMinute) / 60,
		buckets:      make(map[string]*tokenBucket),
	}
}

func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) > 5000 {
		for id, bucket := range l.buckets {
			if now.Sub(bucket.lastSeen) > 10*time.Minute {
				delete(l.buckets, id)
			}
		}
	}
	bucket := l.buckets[key]
	if bucket == nil {
		bucket = &tokenBucket{tokens: l.capacity, updated: now, lastSeen: now}
		l.buckets[key] = bucket
	}
	bucket.tokens = math.Min(l.capacity, bucket.tokens+now.Sub(bucket.updated).Seconds()*l.refillPerSec)
	bucket.updated = now
	bucket.lastSeen = now
	if bucket.tokens >= 1 {
		bucket.tokens--
		return true, 0
	}
	wait := time.Duration(math.Ceil((1 - bucket.tokens) / l.refillPerSec * float64(time.Second)))
	return false, wait
}

func authenticate(store CredentialStore, limiter *RateLimiter, requiredScope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) < 32 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		principal, err := store.Authenticate(ctx, parts[1])
		cancel()
		if err != nil {
			if err == ErrInvalidAPIKey {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "missing or invalid API key")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "authentication service unavailable")
			return
		}
		if !principal.HasScope(requiredScope) {
			writeError(w, http.StatusForbidden, "API key does not have the required scope")
			return
		}
		allowed, retryAfter := limiter.Allow(principal.KeyID)
		if !allowed {
			seconds := int(math.Ceil(retryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, http.StatusTooManyRequests, "request rate limit exceeded")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		next.ServeHTTP(w, r)
	})
}
