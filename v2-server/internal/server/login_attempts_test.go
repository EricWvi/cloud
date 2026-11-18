package server

import (
	"testing"
	"time"
)

// A flood of unfinished logins can only push out the oldest attempts; a new login always starts.
func TestLoginAttemptsEvictInsteadOfRefusing(t *testing.T) {
	a := newLoginAttempts()
	now := time.Now()
	first := a.add(loginAttempt{state: "first", expires: now.Add(time.Minute)}, now)
	expired := a.add(loginAttempt{state: "expired", expires: now.Add(time.Second)}, now)
	for i := 0; i < maxLoginAttempts; i++ {
		a.add(loginAttempt{state: "flood", expires: now.Add(loginAttemptTTL)}, now.Add(2*time.Second))
	}
	if len(a.items) != maxLoginAttempts {
		t.Fatalf("table holds %d attempts", len(a.items))
	}
	if _, ok := a.items[expired]; ok {
		t.Fatal("expired attempt kept")
	}
	if _, ok := a.items[first]; ok {
		t.Fatal("oldest live attempt not evicted when full")
	}
	latest := a.add(loginAttempt{state: "latest", expires: now.Add(loginAttemptTTL + time.Second)}, now.Add(3*time.Second))
	if at, ok := a.take(latest, now.Add(3*time.Second)); !ok || at.state != "latest" {
		t.Fatal("a new login could not start on a full table")
	}
	if _, ok := a.take(latest, now.Add(3*time.Second)); ok {
		t.Fatal("attempt used twice")
	}
}
