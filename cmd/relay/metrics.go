package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// dirStats instruments one copy direction of one splice.
//
// This is the measurement that decides viability. A relay that never blocks on a
// downstream write is a relay whose flow control is working: the pressure reached
// the agent. A relay that blocks briefly and rarely is fine. A relay that blocks
// for hundreds of milliseconds, or whose in-flight byte count grows without bound,
// is a relay that has quietly become a buffer — which is the failure this spike
// exists to find.
type dirStats struct {
	name string

	bytes      atomic.Int64 // bytes forwarded
	writes     atomic.Int64 // completed writes
	inFlight   atomic.Int64 // bytes read from source, not yet handed to dest
	peakFlight atomic.Int64

	// Write blocking.
	blockedNanos atomic.Int64 // total time spent inside dest.Write
	stalls       atomic.Int64 // writes slower than the stall threshold
	stallNanos   atomic.Int64
	worstStall   atomic.Int64

	// Read blocking: how long we waited for the source to produce bytes. High read
	// block time with low write block time means the sender is the bottleneck, i.e.
	// backpressure reached it. That is the good outcome and it needs its own number,
	// or a fast relay and a starved one look identical.
	readNanos atomic.Int64
	reads     atomic.Int64
}

func (d *dirStats) recordWrite(n int, blocked time.Duration, threshold time.Duration) {
	d.bytes.Add(int64(n))
	d.writes.Add(1)
	d.blockedNanos.Add(int64(blocked))
	if blocked > threshold {
		d.stalls.Add(1)
		d.stallNanos.Add(int64(blocked))
		for {
			worst := d.worstStall.Load()
			if int64(blocked) <= worst || d.worstStall.CompareAndSwap(worst, int64(blocked)) {
				return
			}
		}
	}
}

func (d *dirStats) recordRead(n int, blocked time.Duration) {
	d.reads.Add(1)
	d.readNanos.Add(int64(blocked))
	f := d.inFlight.Add(int64(n))
	for {
		peak := d.peakFlight.Load()
		if f <= peak || d.peakFlight.CompareAndSwap(peak, f) {
			return
		}
	}
}

func (d *dirStats) releaseInFlight(n int) { d.inFlight.Add(-int64(n)) }

func (d *dirStats) summary() string {
	stalls := d.stalls.Load()
	var avgStall time.Duration
	if stalls > 0 {
		avgStall = time.Duration(d.stallNanos.Load() / stalls)
	}
	writes := d.writes.Load()
	var avgWrite time.Duration
	if writes > 0 {
		avgWrite = time.Duration(d.blockedNanos.Load() / writes)
	}
	reads := d.reads.Load()
	var avgRead time.Duration
	if reads > 0 {
		avgRead = time.Duration(d.readNanos.Load() / reads)
	}
	return fmt.Sprintf("%s bytes=%d writes=%d avg_write_block=%s stalls=%d avg_stall=%s worst_stall=%s "+
		"peak_inflight=%d avg_read_wait=%s total_write_block=%s total_read_wait=%s",
		d.name, d.bytes.Load(), writes, avgWrite.Round(time.Microsecond), stalls,
		avgStall.Round(time.Microsecond), time.Duration(d.worstStall.Load()).Round(time.Microsecond),
		d.peakFlight.Load(), avgRead.Round(time.Microsecond),
		time.Duration(d.blockedNanos.Load()).Round(time.Millisecond),
		time.Duration(d.readNanos.Load()).Round(time.Millisecond))
}

// sessionStats aggregates every direction of one spliced session.
type sessionStats struct {
	ClientToUpstream dirStats
	UpstreamToClient dirStats

	DatagramsUp     atomic.Int64
	DatagramsDown   atomic.Int64
	DatagramsTooBig atomic.Int64
	DatagramDropped atomic.Int64

	StreamsBidiUp   atomic.Int64 // client-initiated bidi mirrored upstream
	StreamsUniUp    atomic.Int64 // client-initiated uni mirrored upstream
	StreamsBidiDown atomic.Int64 // upstream-initiated bidi mirrored down
	StreamsUniDown  atomic.Int64 // upstream-initiated uni mirrored down

	ResetsMirrored    atomic.Int64
	ResetsSubstituted atomic.Int64 // codes we could not represent; see wterr

	mu       sync.Mutex
	firstErr error
}

func (s *sessionStats) noteErr(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.firstErr == nil {
		s.firstErr = err
	}
	s.mu.Unlock()
}

func (s *sessionStats) summary() string {
	return fmt.Sprintf("streams(bidi_up=%d uni_up=%d bidi_down=%d uni_down=%d) "+
		"datagrams(up=%d down=%d too_big=%d dropped=%d) resets(mirrored=%d substituted=%d)\n    %s\n    %s",
		s.StreamsBidiUp.Load(), s.StreamsUniUp.Load(), s.StreamsBidiDown.Load(), s.StreamsUniDown.Load(),
		s.DatagramsUp.Load(), s.DatagramsDown.Load(), s.DatagramsTooBig.Load(), s.DatagramDropped.Load(),
		s.ResetsMirrored.Load(), s.ResetsSubstituted.Load(),
		s.ClientToUpstream.summary(), s.UpstreamToClient.summary())
}
