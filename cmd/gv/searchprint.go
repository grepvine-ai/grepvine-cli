package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// The escape sequences follow ripgrep's default colors: paths magenta, line
// numbers green, matched text bold red.
const (
	ansiReset = "\x1b[0m"
	ansiPath  = "\x1b[35m"
	ansiLine  = "\x1b[32m"
	ansiMatch = "\x1b[1;31m"
)

// searchPrinter writes search results the way ripgrep does. On a terminal the
// results are grouped under their document key, one heading per document,
// separated by blank lines. Anywhere else every line of a result is one
// `key:line:text` line, which is what scripts parse; a result whose match
// spans lines prints each of its lines that way, numbered in turn. Colors
// follow the --color flag; in auto mode they are on only for a terminal that
// has not set NO_COLOR or TERM=dumb.
//
// Context lines requested with -A, -B or -C follow grep's layout: a context
// line joins its fields with `-` where a matching line uses `:`, a line is
// printed once even when it is context to two matches or a match inside
// another's context, and `--` separates groups of lines that are not adjacent
// in the document. Results arrive in document order, so one result's trailing
// context is held back until the next result says whether it overlaps.
type searchPrinter struct {
	out     io.Writer
	color   bool
	heading bool
	// context is whether the search asked for context lines; it decides
	// whether group separators are printed at all.
	context bool
	lastKey string
	// lastLine is the last line number printed for lastKey, or zero.
	lastLine int
	// pending holds the trailing context of the last result, not yet printed.
	pending []httpapi.ContextLine
	started bool
	line    strings.Builder
}

func (a *app) newSearchPrinter(options searchOptions) (*searchPrinter, error) {
	color := false
	switch options.color {
	case "auto", "":
		color = a.stdoutTTY && a.getenv("NO_COLOR") == "" && a.getenv("TERM") != "dumb"
	case "always":
		color = true
	case "never":
	default:
		return nil, fmt.Errorf("--color must be auto, always or never")
	}
	return &searchPrinter{out: a.stdout, color: color, heading: a.stdoutTTY, context: options.before > 0 || options.after > 0}, nil
}

func (p *searchPrinter) print(result httpapi.QueryResult) {
	if p.started && result.Key != p.lastKey {
		p.flushPending(-1)
		p.lastLine = 0
		if p.heading {
			p.line.WriteByte('\n')
		} else if p.context {
			p.line.WriteString("--\n")
		}
	}
	if p.heading && (!p.started || result.Key != p.lastKey) {
		p.writeColored(ansiPath, result.Key)
		p.line.WriteByte('\n')
	}
	p.started = true
	p.lastKey = result.Key
	p.flushPending(result.LineNumber)
	for _, line := range result.LinesBefore {
		p.writeLine(line.LineNumber, func() { p.line.WriteString(line.Text) }, false)
	}
	// The text holds every line the result covers, from the line numbered
	// TextLineNumber; a text cut at its start begins on a later line than the
	// result, and each physical line prints with its own number.
	spans := textSpans(result)
	number := result.TextLineNumber
	for start := 0; ; number++ {
		end := strings.IndexByte(result.Text[start:], '\n')
		if end < 0 {
			end = len(result.Text)
		} else {
			end += start
		}
		lineStart, lineEnd := start, end
		p.writeLine(number, func() { p.writeText(result.Text, spans, lineStart, lineEnd) }, true)
		if end == len(result.Text) {
			break
		}
		start = end + 1
	}
	p.lastLine = max(p.lastLine, result.LastLineNumber)
	p.pending = result.LinesAfter
	_, _ = io.WriteString(p.out, p.line.String())
	p.line.Reset()
}

// finish prints the trailing context held back from the last result.
func (p *searchPrinter) finish() {
	p.flushPending(-1)
	_, _ = io.WriteString(p.out, p.line.String())
	p.line.Reset()
}

// flushPending prints the held trailing context up to, and not including,
// the line numbered before; a negative bound prints all of it. A held line at
// or past the next result is dropped: it is that result, or its own context,
// and prints from there.
func (p *searchPrinter) flushPending(before int) {
	for _, line := range p.pending {
		if before >= 0 && line.LineNumber >= before {
			break
		}
		p.writeLine(line.LineNumber, func() { p.line.WriteString(line.Text) }, false)
	}
	p.pending = nil
}

// writeLine prints one line of the current document unless it is already on
// the page, with a `--` separator when it does not follow the previous one.
func (p *searchPrinter) writeLine(number int, text func(), match bool) {
	if number <= p.lastLine {
		return
	}
	if p.context && p.lastLine > 0 && number > p.lastLine+1 {
		p.line.WriteString("--\n")
	}
	separator := byte(':')
	if !match {
		separator = '-'
	}
	if !p.heading {
		p.writeColored(ansiPath, p.lastKey)
		p.line.WriteByte(separator)
	}
	p.writeColored(ansiLine, strconv.Itoa(number))
	p.line.WriteByte(separator)
	text()
	p.line.WriteByte('\n')
	p.lastLine = number
}

// writeText prints text[start:end], one physical line of a result, with the
// parts of the result's spans that fall inside it highlighted.
func (p *searchPrinter) writeText(text string, spans []textSpan, start, end int) {
	if !p.color || len(spans) == 0 {
		p.line.WriteString(text[start:end])
		return
	}
	cursor := start
	for _, span := range spans {
		from, to := max(span.start, start), min(span.end, end)
		if from >= to {
			continue
		}
		p.line.WriteString(text[cursor:from])
		p.writeColored(ansiMatch, text[from:to])
		cursor = to
	}
	p.line.WriteString(text[cursor:end])
}

func (p *searchPrinter) writeColored(code, text string) {
	if !p.color {
		p.line.WriteString(text)
		return
	}
	p.line.WriteString(code)
	p.line.WriteString(text)
	p.line.WriteString(ansiReset)
}

type textSpan struct{ start, end int }

// textSpans converts a result's match offsets, which are absolute within the
// document, into ordered, non-overlapping byte ranges within its text. A
// match that runs past either edge of a truncated text is clipped to the part
// that is shown, and an empty match highlights nothing.
func textSpans(result httpapi.QueryResult) []textSpan {
	spans := make([]textSpan, 0, len(result.Matches))
	for _, match := range result.Matches {
		start := match.Offset - result.TextStartOffset
		end := start + match.Length
		start = max(start, 0)
		end = min(end, len(result.Text))
		if start < end {
			spans = append(spans, textSpan{start: start, end: end})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:0]
	for _, span := range spans {
		if last := len(merged) - 1; last >= 0 && span.start <= merged[last].end {
			merged[last].end = max(merged[last].end, span.end)
			continue
		}
		merged = append(merged, span)
	}
	return merged
}
