package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// ui serialises terminal output so the progress line and log lines don't
// garble each other. Logs go to stderr; real output (lists, dry-run) to stdout.
type ui struct {
	mu      sync.Mutex
	tty     bool
	showing bool
}

var term = newUI()

func newUI() *ui {
	fi, err := os.Stderr.Stat()
	return &ui{tty: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

// logf prints one line. When not attached to a terminal (cron, systemd) every
// line gets a timestamp so the log is useful on its own.
func (u *ui) logf(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
	msg := fmt.Sprintf(format, args...)
	if u.tty {
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// status redraws a single progress line (terminals only).
func (u *ui) status(line string) {
	if !u.tty {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(os.Stderr, "\r%-78s", line)
	u.showing = true
}

func (u *ui) clear() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
}

func (u *ui) clearLocked() {
	if u.showing {
		fmt.Fprintf(os.Stderr, "\r%78s\r", "")
		u.showing = false
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
