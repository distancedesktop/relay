// Package procstat reads this process's own CPU time, so the relay and the load
// client can report what the splice cost without an external profiler.
//
// /proc/self/stat is used rather than syscall.Getrusage because it also exposes
// the thread count, which is the other thing worth watching in a relay that
// spawns goroutines per stream.
package procstat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CPU is a snapshot of process CPU consumption.
type CPU struct {
	UserSeconds   float64
	SystemSeconds float64
	Threads       int
}

// Total is user + system CPU seconds.
func (c CPU) Total() float64 { return c.UserSeconds + c.SystemSeconds }

// Read samples /proc/self/stat.
func Read() (CPU, error) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return CPU{}, err
	}
	// comm can contain spaces and parentheses, so split after the last ')'.
	line := string(b)
	i := strings.LastIndexByte(line, ')')
	if i < 0 || i+2 >= len(line) {
		return CPU{}, fmt.Errorf("procstat: unparseable /proc/self/stat")
	}
	// After comm, field 3 is state; utime is field 14 overall, i.e. index 11 here.
	fields := strings.Fields(line[i+2:])
	if len(fields) < 18 {
		return CPU{}, fmt.Errorf("procstat: only %d fields after comm", len(fields))
	}
	utime, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		return CPU{}, fmt.Errorf("procstat: utime: %w", err)
	}
	stime, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		return CPU{}, fmt.Errorf("procstat: stime: %w", err)
	}
	threads, err := strconv.Atoi(fields[17])
	if err != nil {
		threads = 0
	}
	// USER_HZ is 100 on every Linux/amd64 and arm64 kernel in practice; there is no
	// portable way to read it without cgo, and being wrong by a constant factor
	// would be obvious in the numbers.
	const userHZ = 100.0
	return CPU{
		UserSeconds:   utime / userHZ,
		SystemSeconds: stime / userHZ,
		Threads:       threads,
	}, nil
}

// TotalSeconds is Read().Total() with errors swallowed, for report fields where a
// zero is an acceptable "not measured".
func TotalSeconds() float64 {
	c, err := Read()
	if err != nil {
		return 0
	}
	return c.Total()
}
