package sse

import "time"

// Clock is the single time source used by Pump for stall/keepalive timing and
// downstream write deadlines. Tests inject a manually advanced implementation.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Timer is the resettable one-shot timer created by Clock.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

// RealClock uses the process wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) NewTimer(d time.Duration) Timer {
	return realTimer{timer: time.NewTimer(d)}
}

type realTimer struct {
	timer *time.Timer
}

func (t realTimer) C() <-chan time.Time        { return t.timer.C }
func (t realTimer) Stop() bool                 { return t.timer.Stop() }
func (t realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }
