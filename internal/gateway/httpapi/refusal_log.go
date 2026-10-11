package httpapi

import (
	"net"
	"strconv"
	"sync"
	"time"

	"selfmind/internal/platform/log"
)

// refusalLog writes the warnings for refused peers without letting peers that
// keep asking fill the daemon's log. Two limits apply, and both hold when the
// other does not:
//
//   - a peer is warned about once per kind of refusal per interval, and its next
//     warning says how many it held back;
//   - all peers together get at most maxRefusalWarningsPerInterval warnings per
//     interval, because a process that varies its address (or many processes) is a
//     new peer on every request. What the ceiling held back is reported in one
//     summary line when the interval ends, written with the next refusal.
//
// The table is bounded too. The zero value is ready to use.
type refusalLog struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*refusalEntry

	// The window of the ceiling: when it began, how many warnings it let through
	// and how many refusals it held back.
	windowStart      time.Time
	windowWarned     int
	windowSuppressed int
}

type refusalEntry struct {
	last       time.Time
	suppressed int
}

const (
	refusalWarnInterval = time.Minute
	maxRefusalPeers     = 256
	// maxRefusalWarningsPerInterval is the most warning lines the daemon writes
	// about refusals in an interval, whoever the peers are. The summary line that
	// follows a window that held some back is outside it: one line per interval.
	maxRefusalWarningsPerInterval = 20
)

func (l *refusalLog) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *refusalLog) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// warn logs message unless key was already warned about within the interval, or
// the interval's ceiling across all peers has been reached.
func (l *refusalLog) warn(key, message string, args ...any) {
	l.mu.Lock()
	now := l.clock()
	if l.entries == nil {
		l.entries = map[string]*refusalEntry{}
	}
	heldBack := 0
	if now.Sub(l.windowStart) >= refusalWarnInterval {
		heldBack = l.windowSuppressed
		l.windowStart, l.windowWarned, l.windowSuppressed = now, 0, 0
	}
	entry := l.entries[key]
	suppressed, write := 0, true
	switch {
	case entry != nil && now.Sub(entry.last) < refusalWarnInterval:
		entry.suppressed++
		write = false
	case l.windowWarned >= maxRefusalWarningsPerInterval:
		// Held back by the ceiling: this peer is not recorded as warned, and takes
		// no table space, so it is heard once the window has passed.
		l.windowSuppressed++
		write = false
	default:
		l.windowWarned++
		if entry != nil {
			suppressed = entry.suppressed
		} else {
			if len(l.entries) >= maxRefusalPeers {
				l.evict(now)
			}
			entry = &refusalEntry{}
			l.entries[key] = entry
		}
		entry.last, entry.suppressed = now, 0
	}
	l.mu.Unlock()
	if heldBack > 0 {
		log.Warn("gateway: refusal warnings were held back by the per-interval ceiling",
			"suppressed", heldBack, "ceiling", maxRefusalWarningsPerInterval, "interval", refusalWarnInterval.String())
	}
	if !write {
		return
	}
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	log.Warn(message, args...)
}

// evict makes room: peers whose interval has passed are forgotten, and if that
// is not enough the whole table is. What is forgotten is only a count of
// suppressed warnings and the right to be silent: the next refusal from such a
// peer warns once more.
func (l *refusalLog) evict(now time.Time) {
	for key, entry := range l.entries {
		if now.Sub(entry.last) >= refusalWarnInterval {
			delete(l.entries, key)
		}
	}
	if len(l.entries) >= maxRefusalPeers {
		clear(l.entries)
	}
}

// refusalKey names a peer for the warning limit: a socket peer by its process, a
// TCP peer by its address without the port, which changes on every connection.
func refusalKey(kind string, peer LocalPeer, remoteAddr string) string {
	if peer.Socket {
		return kind + "|pid:" + strconv.Itoa(peer.PID)
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return kind + "|" + host
}
