// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package tconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/roleserve"
	cpb "source.monogon.dev/metropolis/proto/common"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"
)

type page interface {
	render(*Console)
	processEvent(*Console, tcell.Event)
}

type Config struct {
	Terminal    Terminal
	LogTree     *logtree.LogTree
	Network     event.Value[*node.NetStatus]
	Roles       event.Value[*cpb.NodeRoles]
	CuratorConn event.Value[*roleserve.CuratorConnection]
}

// Console is a Terminal Console (TConsole), a user-interactive informational
// display visible on the TTY of a running Metropolis instance.
type Console struct {
	// Quit will be closed when the user press CTRL-C. A new channel will be created
	// on each New call.
	Quit    chan struct{}
	ttyPath string
	tty     tcell.Tty
	screen  tcell.Screen
	width   int
	height  int
	// palette chosen for the given terminal.
	palette palette
	// activePage expressed within [0...num pages). The number/layout of pages is
	// constructed dynamically in Run.
	activePage int

	config    Config
	logReader *logtree.LogReader
}

// New creates a new Console, taking over the TTY at the given path. The given
// Terminal type selects between a Linux terminal (VTY) and a generic terminal
// for testing.
//
// network, roles, curatorConn point to various Metropolis subsystems that are
// used to populate the console data.
func New(config Config, ttyPath string) (*Console, error) {
	reader, err := config.LogTree.Read(
		"",
		logtree.WithChildren(),
		logtree.WithStream(),
		logtree.WithBacklog(logtree.BacklogAllAvailable),
	)
	if err != nil {
		return nil, fmt.Errorf("lt.Read: %w", err)
	}

	tty, err := tcell.NewDevTtyFromDev(ttyPath)
	if err != nil {
		return nil, err
	}
	screen, err := tcell.NewTerminfoScreenFromTty(tty)
	if err != nil {
		return nil, err
	}
	if err := screen.Init(); err != nil {
		return nil, err
	}
	screen.SetStyle(tcell.StyleDefault)

	var pal palette
	switch config.Terminal {
	case TerminalLinux:
		pal = paletteLinux
		tty.Write([]byte(pal.setupLinuxConsole()))
	case TerminalGeneric:
		pal = paletteGeneric
	}

	width, height := screen.Size()

	return &Console{
		ttyPath:    ttyPath,
		tty:        tty,
		screen:     screen,
		width:      width,
		height:     height,
		palette:    pal,
		Quit:       make(chan struct{}),
		activePage: 0,
		config:     config,
		logReader:  reader,
	}, nil
}

// Cleanup should be called when the console exits. This is only used in testing,
// the Metropolis console always runs.
func (c *Console) Cleanup() {
	c.screen.Fini()
	c.logReader.Close()
}

func (c *Console) processEvent(ev tcell.Event) {
	switch ev := ev.(type) {
	case *tcell.EventKey:
		if ev.Key() == tcell.KeyCtrlC {
			close(c.Quit)
		}
		if ev.Key() == tcell.KeyTab {
			c.activePage += 1
		}
	case *tcell.EventResize:
		c.width, c.height = ev.Size()
	}
}

// Run blocks while displaying the Console to the user.
func (c *Console) Run(ctx context.Context) error {
	// Build channel for console event processing.
	evC := make(chan tcell.Event)
	evQuitC := make(chan struct{})
	defer close(evQuitC)
	go c.screen.ChannelEvents(evC, evQuitC)

	// Pipe event values into channels.
	netAddrC := make(chan *node.NetStatus)
	rolesC := make(chan *cpb.NodeRoles)
	curatorConnC := make(chan *roleserve.CuratorConnection)
	if err := supervisor.Run(ctx, "netpipe", event.Pipe(c.config.Network, netAddrC)); err != nil {
		return err
	}
	if err := supervisor.Run(ctx, "rolespipe", event.Pipe(c.config.Roles, rolesC)); err != nil {
		return err
	}
	if err := supervisor.Run(ctx, "curatorpipe", event.Pipe(c.config.CuratorConn, curatorConnC)); err != nil {
		return err
	}
	supervisor.Signal(ctx, supervisor.SignalHealthy)

	// Per-page data.
	pageStatus := pageStatus{
		netAddr:     "Waiting...",
		roles:       "Waiting...",
		id:          "Waiting...",
		fingerprint: "Waiting...",
	}
	pageLogs := pageLogs{}
	// Fetch backlog
	for _, le := range c.logReader.Backlog {
		pageLogs.appendLine(le)
	}

	// Page references and names.
	pages := []page{
		&pageStatus,
		&pageLogs,
	}
	pageNames := []string{
		"Status", "Logs",
	}

	// Ticker used to maintain redraws at minimum 10Hz, to eg. update the clock in
	// the status bar.
	tickerDraw := time.NewTicker(time.Second / 10)
	defer tickerDraw.Stop()

	// Ticker used to fully resync the screen every 10 seconds, in case something
	// scribbled over the TTY.
	tickerSync := time.NewTicker(time.Second * 10)
	defer tickerSync.Stop()

	for {
		// Draw active page.
		c.activePage %= len(pages)
		page := pages[c.activePage]
		page.render(c)

		// Draw status bar.
		c.statusBar(c.activePage, pageNames...)

		// Sync to screen.
		c.screen.Show()

		select {
		case <-tickerDraw.C:
		case <-tickerSync.C:
			c.screen.Sync()
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-evC:
			c.processEvent(ev)
			page.processEvent(c, ev)
		case t := <-netAddrC:
			pageStatus.netAddr = t.ExternalAddress.String()
		case t := <-rolesC:
			var rlist []string
			if t.ConsensusMember != nil {
				rlist = append(rlist, "ConsensusMember")
			}
			if t.KubernetesController != nil {
				rlist = append(rlist, "KubernetesController")
			}
			if t.KubernetesWorker != nil {
				rlist = append(rlist, "KubernetesWorker")
			}
			pageStatus.roles = strings.Join(rlist, ", ")
			if pageStatus.roles == "" {
				pageStatus.roles = "none"
			}
		case t := <-curatorConnC:
			pageStatus.id = t.Credentials.ID()
			cert := t.Credentials.ClusterCA()
			sum := sha256.New()
			sum.Write(cert.Raw)
			pageStatus.fingerprint = hex.EncodeToString(sum.Sum(nil))
		case le := <-c.logReader.Stream:
			pageLogs.appendLine(le)
		}
	}
}
