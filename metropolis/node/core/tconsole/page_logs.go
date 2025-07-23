// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package tconsole

import (
	"github.com/gdamore/tcell/v2"

	"source.monogon.dev/osbase/logtree"
)

// maxLines defines the maximum number of log lines that should be stored
// at any point.
const maxLines = 1024

// pageLogs encompasses all data to be shown within the logs page.
type pageLogs struct {
	// log lines, simple deque with the newest log lines appended to the end.
	lines []*logtree.LogEntry

	// log lines for scrollback
	scrollbackBuffer []*logtree.LogEntry
}

func (p *pageLogs) appendLine(le *logtree.LogEntry) {
	p.lines = append(p.lines, le)
	p.compactData(maxLines)
}

// compactData ensures that there's no more lines stored than maxlines by
// discarding the oldest lines.
func (p *pageLogs) compactData(maxlines int) {
	if extra := len(p.lines) - maxlines; extra > 0 {
		p.lines = p.lines[extra:]
	}
}

// render renders the logs page to the user.
func (p *pageLogs) render(c *Console) {
	c.screen.Clear()
	sty1 := tcell.StyleDefault.Background(c.color(colorPink)).Foreground(c.color(colorBlack))
	sty2 := tcell.StyleDefault.Background(c.color(colorBlue)).Foreground(c.color(colorBlack))

	// Draw frame.
	c.fillRectangle(0, c.width, 0, c.height, sty2)
	c.fillRectangle(1, c.width-1, 1, c.height-2, sty1)

	// Inner log area size.
	nlines := (c.height - 2) - 1
	linelen := (c.width - 1) - 1

	// Discard everything outside of our visible window
	p.compactData(nlines)

	lines := p.lines
	if p.scrollbackBuffer != nil {
		lines = p.scrollbackBuffer
	}

	for y := 0; y < nlines; y++ {
		if y < len(lines) {
			line := lines[y].String()
			if len(line) > linelen {
				line = line[:linelen]
			}
			c.drawText(1, 1+y, line, sty1)
		}
	}
}

func (p *pageLogs) processEvent(c *Console, ev tcell.Event) {
	// Inner log area size.
	nlines := (c.height - 2) - 1

	var scrollInput int
	switch ev := ev.(type) {
	case *tcell.EventKey:
		switch ev.Key() {
		case tcell.KeyEnd:
			scrollInput = 0
		case tcell.KeyUp:
			scrollInput = -1
		case tcell.KeyDown:
			scrollInput = 1
		case tcell.KeyPgUp:
			scrollInput = -nlines
		case tcell.KeyPgDn:
			scrollInput = nlines
		default:
			return
		}
	}

	p.processScrollInput(c.config.LogTree, scrollInput, nlines)
}

func (p *pageLogs) processScrollInput(lt *logtree.LogTree, scrollInput int, nlines int) {
	// Disable scrollback if the screen is not full or
	// the user wants to exit it.
	if len(p.lines) < nlines || scrollInput == 0 {
		p.scrollbackBuffer = nil
		return
	}

	// The position of the most recent line
	maxPos := p.lines[len(p.lines)-1].Position

	// Fetch our current scrollback position from either the scrollback buffer or
	// or the most recent streamed line.
	var oldPos int
	if p.scrollbackBuffer != nil {
		oldPos = p.scrollbackBuffer[len(p.scrollbackBuffer)-1].Position
	} else {
		oldPos = maxPos
	}

	// If our inputs scroll past the latest streamed line, disable it.
	if oldPos+scrollInput > maxPos {
		p.scrollbackBuffer = nil
		return
	}

	// Update and limit the scroll position to the most recent line and
	// at least a full screen.
	newPos := min(maxPos, max(nlines, oldPos+scrollInput))

	// Fetch the actual scrollback from the journal if we moved the scrollback
	// position.
	p.scrollbackBuffer = fetchScrollback(lt, nlines, newPos)
}

func fetchScrollback(logTree *logtree.LogTree, nlines int, position int) []*logtree.LogEntry {
	reader, err := logTree.Read(
		"",
		logtree.WithChildren(),
		logtree.WithBacklog(nlines),
		// Add an offset of one to the position, as we are fetching messages before
		// the given position, skipping the given position entirely.
		logtree.WithStartPosition(position+1, logtree.ReadDirectionBefore),
	)
	// This should not happen as only invalid argument combinations are capable
	// of returning an error.
	if err != nil {
		panic("unreachable")
	}

	return reader.Backlog
}
