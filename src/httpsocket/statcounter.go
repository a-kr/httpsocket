package main

import (
	"log"
	"sync/atomic"
	"time"
)

// Счетчики числа событий в секунду, числа активных соединений/запросов, и ограничители
type StatCounter struct {
	parentCounter            *StatCounter
	unixtime                 int64
	connectionAttemptsPerSec int64
	connectionsPerSec        int64
	activeConnections        int64
	requestsPerSec           int64
	responsesPerSec          int64
	activeRequests           int64
}

func NewStatCounter(parentCounter *StatCounter) *StatCounter {
	sc := &StatCounter{}
	sc.parentCounter = parentCounter
	sc.Tick(time.Now().Unix())
	return sc
}

func (sc *StatCounter) TickingLoop() {
	for now := range time.Tick(1 * time.Second) {
		nowUnix := now.Unix()
		scCopy := sc.Tick(nowUnix)
		if scCopy.activeConnections == 0 && scCopy.requestsPerSec == 0 && scCopy.responsesPerSec == 0 {
			continue
		}
		log.Printf("New conns per sec: %d; Active conns: %d; RPS: %d; Handled RPS: %d; Active requests: %d",
			scCopy.connectionsPerSec, scCopy.activeConnections,
			scCopy.requestsPerSec, scCopy.responsesPerSec, scCopy.activeRequests)
	}
}

// Сбрасывает счетчики для начала новой секунды.
// Возвращает копию sc с замороженными на предыдущей секунде значениями.
func (sc *StatCounter) Tick(unixtime int64) *StatCounter {
	scCopy := &StatCounter{}
	scCopy.unixtime = atomic.SwapInt64(&sc.unixtime, unixtime)
	// counters
	scCopy.connectionAttemptsPerSec = atomic.SwapInt64(&sc.connectionAttemptsPerSec, 0)
	scCopy.connectionsPerSec = atomic.SwapInt64(&sc.connectionsPerSec, 0)
	scCopy.requestsPerSec = atomic.SwapInt64(&sc.requestsPerSec, 0)
	scCopy.responsesPerSec = atomic.SwapInt64(&sc.responsesPerSec, 0)
	// gauges
	scCopy.activeConnections = atomic.LoadInt64(&sc.activeConnections)
	scCopy.activeRequests = atomic.LoadInt64(&sc.activeRequests)
	return scCopy
}

func (sc *StatCounter) ConnectionAttempt() {
	atomic.AddInt64(&sc.connectionAttemptsPerSec, 1)
	if sc.parentCounter != nil {
		sc.parentCounter.ConnectionAttempt()
	}
}

func (sc *StatCounter) OpenedConnection() {
	atomic.AddInt64(&sc.connectionsPerSec, 1)
	atomic.AddInt64(&sc.activeConnections, 1)
	if sc.parentCounter != nil {
		sc.parentCounter.OpenedConnection()
	}
}

func (sc *StatCounter) ClosedConnection() {
	atomic.AddInt64(&sc.activeConnections, -1)
	if sc.parentCounter != nil {
		sc.parentCounter.ClosedConnection()
	}
}

func (sc *StatCounter) RequestStarted() {
	atomic.AddInt64(&sc.requestsPerSec, 1)
	atomic.AddInt64(&sc.activeRequests, 1)
	if sc.parentCounter != nil {
		sc.parentCounter.RequestStarted()
	}
}

func (sc *StatCounter) RequestFinished() {
	atomic.AddInt64(&sc.responsesPerSec, 1)
	atomic.AddInt64(&sc.activeRequests, -1)
	if sc.parentCounter != nil {
		sc.parentCounter.RequestFinished()
	}
}
