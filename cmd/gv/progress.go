package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

const (
	// progressInterval is how often the status line is redrawn.
	progressInterval = 100 * time.Millisecond
	// progressTau is the time constant of the smoothed rates. A rate averaged
	// over the whole run barely moves, so a server that asks the client to
	// slow down would not show up on the line at all; a rate averaged over a
	// second or two swings with every batch that lands, because a batch is
	// thousands of files arriving at once, and drags the estimate with it.
	// This is long enough to hold the estimate steady between batches and
	// short enough that a pause is visible within a few seconds of starting.
	progressTau = 5 * time.Second
	// progressCells is the width of the bar. The whole line is kept inside
	// eighty columns so a redraw never wraps and leaves a stale tail behind.
	progressCells = 12
)

// spinnerFrames animate the scanning line. The listing has no total to measure
// against until it ends, so the line shows that it is alive and how much it
// has found rather than a fraction it cannot know.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// progress renders the live status line of an upload on a terminal.
//
// Everything the upload prints while it runs goes through it, so a warning
// never lands in the middle of a redraw, and a server asking the client to
// slow down changes the line instead of scrolling it away. On anything that is
// not a terminal the bar draws nothing and only the messages come out, which
// keeps redirected output and test output clean.
type progress struct {
	out     io.Writer
	enabled bool
	stop    chan struct{}
	stopped chan struct{}

	// noun names what the line counts: files, or the documents of an
	// import.
	noun string

	mu         sync.Mutex
	started    time.Time
	scanning   bool
	filesFound int64
	filesSent  int64
	bytesSent  int64
	// An import does not know how many documents its files hold, so it
	// measures how much of their bytes it has sent instead.
	bySource     bool
	sourceTotal  int64
	sourceSent   int64
	inDeletion   bool
	deleteTotal  int64
	filesDeleted int64
	// json, when set, receives one line per document the service refused.
	// totals, when set, receives what the command did when it finishes.
	json        io.Writer
	totals      *transferSummary
	failed      int64
	pausedUntil time.Time
	width       int

	sampled       time.Time
	sampledFound  int64
	sampledSent   int64
	sampledBytes  int64
	sampledSource int64
	foundRate     float64
	sentRate      float64
	byteRate      float64
	sourceRate    float64
}

// newStatus is the progress of the command now running, which reports its
// totals and refused documents on stdout under --json.
func (a *app) newStatus() *progress {
	status := newProgress(a.stderr, a.isTTY)
	if a.jsonOutput {
		status.json = a.stdout
		a.totals = transferSummary{}
		status.totals = &a.totals
	}
	return status
}

// encodeJSONLine writes value as one line of JSON. Like the generated
// marshalers, it leaves <, > and & as they are.
func encodeJSONLine(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// reportTotals prints the totals of a command run with --json, as its last
// line. A command that failed before it wrote anything prints none, so a
// line of zeros never stands for a run that did not happen.
func (a *app) reportTotals(err error) error {
	if a.jsonOutput && (err == nil || a.totals != (transferSummary{})) {
		_ = encodeJSONLine(a.stdout, a.totals)
	}
	return err
}

func newProgress(out io.Writer, enabled bool) *progress {
	return &progress{
		out: out, enabled: enabled, scanning: true, started: time.Now(), noun: "file",
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

// start begins redrawing the status line.
func (p *progress) start() {
	if !p.enabled {
		close(p.stopped)
		return
	}
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case now := <-ticker.C:
				p.mu.Lock()
				p.sample(now)
				p.draw(p.line(now))
				p.mu.Unlock()
			}
		}
	}()
}

// finish takes the status line off the screen and replaces it with one line
// reporting what the upload did.
func (p *progress) finish() {
	close(p.stop)
	<-p.stopped
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.totals != nil {
		*p.totals = transferSummary{Uploaded: p.filesSent, Bytes: p.bytesSent, Deleted: p.filesDeleted, Failed: p.failed}
	}
	if !p.enabled {
		return
	}
	p.clear()
	if p.filesSent == 0 && p.filesDeleted == 0 {
		return
	}
	parts := make([]string, 0, 2)
	if p.filesSent > 0 {
		parts = append(parts, fmt.Sprintf("Uploaded %s %s · %s", formatCount(p.filesSent), plural(p.filesSent, p.noun), formatSize(float64(p.bytesSent))))
	}
	if p.filesDeleted > 0 {
		parts = append(parts, fmt.Sprintf("Deleted %s %s", formatCount(p.filesDeleted), plural(p.filesDeleted, "file")))
	}
	fmt.Fprintf(p.out, "%s in %s\n", strings.Join(parts, " · "), formatDuration(time.Since(p.started)))
}

func plural(count int64, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}

// deleting switches the line to the deletion bar that follows the uploads of
// a sync.
func (p *progress) deleting(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scanning, p.inDeletion, p.deleteTotal = false, true, total
}

// deleted records one completed batch of deletions.
func (p *progress) deleted(files int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filesDeleted += int64(files)
	if p.filesDeleted > p.deleteTotal {
		p.deleteTotal = p.filesDeleted
	}
}

// scanned records what the walk has found so far. It takes the directory
// counts to match the walk's callback, but the line does not use them: how
// many directories remain says nothing about how many files they hold.
func (p *progress) scanned(_, _, files int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filesFound = files
}

// scanFinished switches the line to the upload bar, whose total is now known
// exactly because the source was counted before any of it was uploaded.
func (p *progress) scanFinished(files int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filesFound, p.scanning = files, false
}

// measureSource switches the line to measure an import by the bytes of its
// source, of which there are total.
func (p *progress) measureSource(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scanning, p.bySource, p.sourceTotal = false, true, total
}

// sent records one completed batch, and the bytes of an import's source it
// was read from.
func (p *progress) sent(files int, bytes, source int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sourceSent += source
	p.filesSent += int64(files)
	if p.filesSent > p.filesFound {
		// The source grew after it was counted. Keep the line honest rather
		// than report more than a whole.
		p.filesFound = p.filesSent
	}
	p.bytesSent += bytes
}

// reportPause records that the server asked the client to wait until the given
// time, and reports whether the status line will show it. The line shows it as
// a label and as the rate falling on its own, so a deferred batch needs no
// notice of its own. Nothing draws when the output is not a terminal, so a
// redirected run still gets the notice in its log.
func (p *progress) reportPause(until time.Time) bool {
	if !p.enabled {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if until.After(p.pausedUntil) {
		p.pausedUntil = until
	}
	return true
}

// transferSummary is the last line of the JSON output of upload and sync.
// Only documents the service stored or deleted are counted.
type transferSummary struct {
	Uploaded int64 `json:"uploaded"`
	Bytes    int64 `json:"bytes"`
	Deleted  int64 `json:"deleted"`
	Failed   int64 `json:"failed"`
}

// rejected counts a document the service did not change and, with --json,
// reports its result on stdout.
func (p *progress) rejected(result httpapi.MutationResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed++
	if p.json != nil {
		_ = writeJSONLine(p.json, &result)
	}
}

// printf writes one message above the status line.
func (p *progress) printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintf(p.out, format, args...)
}

func (p *progress) sample(now time.Time) {
	if p.sampled.IsZero() {
		p.sampled = now
		p.sampledFound = p.filesFound
		p.sampledSent, p.sampledBytes, p.sampledSource = p.filesSent, p.bytesSent, p.sourceSent
		return
	}
	elapsed := now.Sub(p.sampled).Seconds()
	if elapsed <= 0 {
		return
	}
	// Weighting by the elapsed time keeps the smoothing honest when a redraw
	// is late, so the rate reflects the clock and not the tick count.
	weight := 1 - math.Exp(-elapsed/progressTau.Seconds())
	p.foundRate += weight * (float64(p.filesFound-p.sampledFound)/elapsed - p.foundRate)
	p.sentRate += weight * (float64(p.filesSent-p.sampledSent)/elapsed - p.sentRate)
	p.byteRate += weight * (float64(p.bytesSent-p.sampledBytes)/elapsed - p.byteRate)
	p.sourceRate += weight * (float64(p.sourceSent-p.sampledSource)/elapsed - p.sourceRate)
	p.sampled = now
	p.sampledFound = p.filesFound
	p.sampledSent, p.sampledBytes, p.sampledSource = p.filesSent, p.bytesSent, p.sourceSent
}

func (p *progress) line(now time.Time) string {
	if p.scanning && p.filesSent > 0 {
		// An S3 source uploads while it is still being counted, so the
		// count so far is only a lower bound on the total.
		return fmt.Sprintf("Uploading %c %s/%s+ · %s/s · %s/s",
			spinner(now.Sub(p.started)), formatCount(p.filesSent), formatCount(p.filesFound),
			formatShort(p.sentRate), formatSize(p.byteRate))
	}
	if p.scanning {
		// Nothing is uploading yet and the total is not known until the
		// listing ends, so the line counts up instead of filling a bar.
		return fmt.Sprintf("Scanning  %c %s %s found · %s/s",
			spinner(now.Sub(p.started)), formatTally(p.filesFound), plural(p.filesFound, "file"),
			formatShort(p.foundRate))
	}
	if p.inDeletion {
		done := fraction(p.filesDeleted, p.deleteTotal)
		return fmt.Sprintf("Deleting  %s %s · %s/%s", bar(done), percent(done), formatCount(p.filesDeleted), formatCount(p.deleteTotal))
	}
	label := "Uploading"
	eta := remaining(p.filesFound-p.filesSent, p.sentRate)
	if p.bySource {
		eta = remaining(p.sourceTotal-p.sourceSent, p.sourceRate)
	}
	if now.Before(p.pausedUntil) {
		label = "Throttled"
		eta = -1
	}
	if p.bySource {
		done := fraction(p.sourceSent, p.sourceTotal)
		return fmt.Sprintf("%s %s %s · %s docs · %s/s · %s/s · ETA %s",
			label, bar(done), percent(done), formatCount(p.filesSent),
			formatShort(p.sentRate), formatSize(p.byteRate), formatDuration(eta))
	}
	done := fraction(p.filesSent, p.filesFound)
	return fmt.Sprintf("%s %s %s · %s/%s · %s/s · %s/s · ETA %s",
		label, bar(done), percent(done),
		formatCount(p.filesSent), formatCount(p.filesFound),
		formatShort(p.sentRate), formatSize(p.byteRate),
		formatDuration(eta))
}

func (p *progress) draw(line string) {
	runes := len([]rune(line))
	padding := max(p.width-runes, 0)
	fmt.Fprintf(p.out, "\r%s%s", line, strings.Repeat(" ", padding))
	p.width = runes
}

func (p *progress) clear() {
	if p.width == 0 {
		return
	}
	fmt.Fprintf(p.out, "\r%s\r", strings.Repeat(" ", p.width))
	p.width = 0
}

func fraction(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return min(float64(done)/float64(total), 1)
}

func remaining(outstanding int64, rate float64) time.Duration {
	if outstanding <= 0 || rate < 0.5 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return -1
	}
	nanoseconds := float64(outstanding) / rate * float64(time.Second)
	if nanoseconds >= float64(int64(1<<63-1)) {
		return -1
	}
	return time.Duration(nanoseconds)
}

func bar(value float64) string {
	filled := int(value*progressCells + 0.5)
	return "▕" + strings.Repeat("█", filled) + strings.Repeat("░", progressCells-filled) + "▏"
}

// spinner picks the frame for the given time since the line started, so the
// animation advances with the clock and not with how often the line is drawn.
func spinner(elapsed time.Duration) rune {
	frame := max(int(elapsed/progressInterval)%len(spinnerFrames), 0)
	return spinnerFrames[frame]
}

func percent(value float64) string { return fmt.Sprintf("%3.0f%%", value*100) }

// formatCount renders an exact count below ten thousand and an abbreviated one
// above it, so the line keeps a stable width as the numbers grow.
func formatCount(value int64) string {
	switch {
	case value < 10_000:
		return fmt.Sprint(value)
	case value < 1_000_000:
		return fmt.Sprintf("%.0fK", float64(value)/1e3)
	case value < 1_000_000_000:
		return fmt.Sprintf("%.2fM", float64(value)/1e6)
	default:
		return fmt.Sprintf("%.2fB", float64(value)/1e9)
	}
}

// formatTally renders a count exactly, with thousands separators, so a count
// that is climbing visibly moves on every redraw.
func formatTally(value int64) string {
	digits := fmt.Sprint(value)
	if value < 0 {
		return digits
	}
	var out strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

func formatShort(value float64) string {
	switch {
	case value < 1_000:
		return fmt.Sprintf("%.0f", value)
	case value < 1_000_000:
		return fmt.Sprintf("%.1fK", value/1e3)
	default:
		return fmt.Sprintf("%.1fM", value/1e6)
	}
}

func formatSize(value float64) string {
	switch {
	case value < 1<<10:
		return fmt.Sprintf("%.0f B", value)
	case value < 1<<20:
		return fmt.Sprintf("%.0f KiB", value/(1<<10))
	case value < 1<<30:
		return fmt.Sprintf("%.1f MiB", value/(1<<20))
	default:
		return fmt.Sprintf("%.2f GiB", value/(1<<30))
	}
}

func formatDuration(value time.Duration) string {
	if value < 0 {
		return "--"
	}
	if value < time.Second {
		return fmt.Sprintf("%.1fs", value.Seconds())
	}
	value = value.Round(time.Second)
	switch {
	case value < time.Minute:
		return fmt.Sprintf("%ds", int(value.Seconds()))
	case value < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(value.Minutes()), int(value.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(value.Hours()), int(value.Minutes())%60)
	}
}
