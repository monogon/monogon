// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

// This is a standalone tconsole test application which works independently from
// a Metropolis instance. It's intended to be used during tconsole development to
// make the iteration cycle faster (not needing to boot up a whole node just to
// test the console).

import (
	"context"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"os"
	"os/signal"
	"time"

	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/roleserve"
	"source.monogon.dev/metropolis/node/core/tconsole"
	cpb "source.monogon.dev/metropolis/proto/common"
	"source.monogon.dev/osbase/event/memory"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"
)

func main() {
	var netV memory.Value[*node.NetStatus]
	var rolesV memory.Value[*cpb.NodeRoles]
	var curV memory.Value[*roleserve.CuratorConnection]

	lt := logtree.New()
	config := tconsole.Config{
		Terminal:    tconsole.TerminalGeneric,
		LogTree:     lt,
		Network:     &netV,
		Roles:       &rolesV,
		CuratorConn: &curV,
	}
	tc, err := tconsole.New(config, "/proc/self/fd/0")
	if err != nil {
		log.Fatalf("tconsole.New: %v", err)
	}

	ctx, ctxC := context.WithCancel(context.Background())
	go func() {
		<-tc.Quit
		ctxC()
	}()

	delay := func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}

	signal.Ignore(os.Interrupt)
	supervisor.New(ctx, func(ctx context.Context) error {
		supervisor.Run(ctx, "tconsole", tc.Run)
		supervisor.Run(ctx, "log-dawdle", func(ctx context.Context) error {
			for {
				supervisor.Logger(ctx).Infof("It is currently: %s", time.Now().Format(time.DateTime))
				if err := delay(ctx, time.Second); err != nil {
					return err
				}
			}
		})
		supervisor.Run(ctx, "net-dawdle", func(ctx context.Context) error {
			supervisor.Signal(ctx, supervisor.SignalHealthy)
			for {
				if err := delay(ctx, time.Millisecond*1000); err != nil {
					return err
				}
				netV.Set(&node.NetStatus{
					ExternalAddress: net.ParseIP(fmt.Sprintf("203.0.113.%d", mrand.Intn(256))),
				})
			}
		})
		supervisor.Run(ctx, "roles-dawdle", func(ctx context.Context) error {
			supervisor.Signal(ctx, supervisor.SignalHealthy)
			for {
				if err := delay(ctx, time.Millisecond*1200); err != nil {
					return err
				}
				nr := &cpb.NodeRoles{}
				if mrand.Intn(2) == 0 {
					nr.KubernetesWorker = &cpb.NodeRoles_KubernetesWorker{}
				}
				if mrand.Intn(2) == 0 {
					nr.ConsensusMember = &cpb.NodeRoles_ConsensusMember{}
				}
				if mrand.Intn(2) == 0 {
					nr.KubernetesController = &cpb.NodeRoles_KubernetesController{}
				}
				rolesV.Set(nr)
			}
		})
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		<-ctx.Done()
		return ctx.Err()
	}, supervisor.WithExistingLogtree(lt), supervisor.WithPropagatePanic)
	<-ctx.Done()
	tc.Cleanup()
}
