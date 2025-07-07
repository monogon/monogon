// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/integration"
	"go.uber.org/zap"
	"google.golang.org/grpc/grpclog"

	"source.monogon.dev/metropolis/node/core/consensus"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/test/util"
	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"
)

var (
	// cluster is a 3-member in-memory etcd cluster for testing.
	cluster *integration.Cluster
	// endpoints is a list of the three etcd members that make up the cluster above.
	endpoints []string
)

// dut is the design under test harness - in this case, a curator instance.
type dut struct {
	// endpoint of the etcd server that this instance is connected to. Each instance
	// connects to a different member of the etcd cluster so that we can easily
	// inject partitions between curator instances.
	endpoint string
	// instance is the curator Service instance itself.
	instance *Service

	// temporary directory in which the Curator's ephemeral directory is placed.
	// Needs to be cleaned up.
	temporary string
}

func (d *dut) cleanup() {
	os.RemoveAll(d.temporary)
}

// newDut creates a new dut harness for a curator instance, connected to a given
// etcd endpoint.
func newDut(ctx context.Context, lt *logtree.LogTree, t *testing.T, endpoint string, n *identity.NodeCredentials) *dut {
	t.Helper()
	// Create new etcd client to the given endpoint.
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:            []string{endpoint},
		DialTimeout:          1 * time.Second,
		DialKeepAliveTime:    1 * time.Second,
		DialKeepAliveTimeout: 1 * time.Second,
		Context:              ctx,
		Logger:               logtree.Zapify(lt.MustLeveledFor("client"), zap.WarnLevel),
	})
	if err != nil {
		t.Fatalf("clientv3.New: %v", err)
	}

	svc := New(Config{
		NodeCredentials: n,
		LeaderTTL:       time.Second,
		Consensus:       consensus.TestServiceHandle(t, cli),
	})
	if err := supervisor.Run(ctx, n.ID(), svc.Run); err != nil {
		t.Fatalf("Run %s: %v", n.ID(), err)
	}
	return &dut{
		endpoint: endpoint,
		instance: svc,
	}
}

// dutSet is a collection of duts keyed by endpoint to which they're connected.
// Since each dut is connected to a different endpoint in these tests, the
// endpoint is used as a unique identifier for each dut/instance.
type dutSet map[string]*dut

// dutUpdate is an update from a dut's Curator instance - either a new
// electionStatus or an error while retrieving it.
type dutUpdate struct {
	// endpoint to which this dut's Curator instance is connected.
	endpoint string
	// status received from the dut's Curator instance, or nil if err is set.
	status *electionStatus
	err    error
}

// dutSetStatus is a point-in-time snapshot of the electionStatus of Curator
// instances, keyed by endpoints in the same way as dutSet.
type dutSetStatus map[string]*electionStatus

// leaders returns a list of endpoints that currently see themselves as leaders.
func (d dutSetStatus) leaders() []string {
	var res []string
	for e, s := range d {
		if s.leader != nil {
			res = append(res, e)
		}
	}
	return res
}

// followers returns a list of endpoints that currently see themselves as
// followers.
func (d dutSetStatus) followers() []string {
	var res []string
	for e, s := range d {
		if s.follower != nil {
			res = append(res, e)
		}
	}
	return res
}

// wait blocks until the dutSetStatus of a given dutSet reaches some state (as
// implemented by predicate f).
func (s dutSet) wait(ctx context.Context, f func(s dutSetStatus) bool) (dutSetStatus, error) {
	ctx2, ctxC := context.WithCancel(ctx)
	defer ctxC()

	// dss is the dutSetStatus which we will keep updating with the electionStatus
	// of each dut's Curator as long as predicate f returns false.
	dss := make(dutSetStatus)

	// updC is a channel of updates from all dut's electionStatus watchers. The
	// dutUpdate type contains the endpoint to distinguish the source of each
	// update.
	updC := make(chan dutUpdate)

	// Run a watcher for each dut which sends that dut's newest available
	// electionStatus (or error) to updC.
	for e, d := range s {
		w := d.instance.status.Watch()
		go func(e string, w event.Watcher[*electionStatus]) {
			defer w.Close()
			for {
				s, err := w.Get(ctx2)
				if err != nil {
					updC <- dutUpdate{
						endpoint: e,
						err:      err,
					}
					return
				}
				updC <- dutUpdate{
					endpoint: e,
					status:   s,
				}
			}
		}(e, w)
	}

	// Keep updating dss with updates from updC and call f on every change.
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case u := <-updC:
			if u.err != nil {
				return nil, fmt.Errorf("from %q: %w", u.endpoint, u.err)
			}
			dss[u.endpoint] = u.status
		}

		if f(dss) {
			return dss, nil
		}
	}
}

// TestLeaderElectionStatus exercises the electionStatus watch/get functionality
// from the Curator code. It spawns a cluster of three curators and ensures all
// of them respond correctly to election, partitioning and subsequent
// re-election.
func TestLeaderElectionStatus(t *testing.T) {
	lt := logtree.New()
	logtree.PipeAllToTest(t, lt)

	ctx, ctxC := context.WithCancel(context.Background())
	cfg := integration.ClusterConfig{
		Size:                 3,
		GRPCKeepAliveMinTime: time.Millisecond,
		LoggerBuilder: func(memberName string) *zap.Logger {
			dn := logtree.DN("etcd." + memberName)
			return logtree.Zapify(lt.MustLeveledFor(dn), zap.WarnLevel)
		},
	}
	integration.BeforeTestExternal(t)
	grpclog.SetLoggerV2(logtree.GRPCify(lt.MustLeveledFor("grpc")))
	cluster = integration.NewCluster(t, &cfg)
	t.Cleanup(func() {
		ctxC()
		cluster.Terminate(t)
		cluster = nil
		endpoints = nil
	})
	endpoints = make([]string, 3)
	for i := range endpoints {
		endpoints[i] = cluster.Client(i).Endpoints()[0]
	}

	// Map from endpoint name to etcd member list index. Alongside with the
	// endpoints list, this is used to quickly look up endpoint<->member_num. Since
	// we only have one Curator instance per etcd member, we can use the instance's
	// etcd endpoint as a unique key to identify it.
	endpointToNum := map[string]int{
		endpoints[0]: 0,
		endpoints[1]: 1,
		endpoints[2]: 2,
	}

	// Start a new supervisor in which we create all curator DUTs.
	ephemeral := util.NewEphemeralClusterCredentials(t, 3)
	dutC := make(chan *dut)
	supervisor.TestHarness(t, func(ctx context.Context) error {
		for e, n := range endpointToNum {
			dutC <- newDut(ctx, lt, t, e, ephemeral.Nodes[n])
		}
		close(dutC)
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	})

	// Build dutSet, ie. map from endpoint to Curator DUT.
	duts := make(dutSet)
	for d := range dutC {
		duts[d.endpoint] = d
	}
	// Schedule cleanup for all DUTs.
	defer func() {
		for _, dut := range duts {
			dut.cleanup()
		}
	}()

	// Wait until we have a Curator leader.
	dss, err := duts.wait(ctx, func(dss dutSetStatus) bool {
		return len(dss.leaders()) == 1 && len(dss.followers()) == 2
	})
	if err != nil {
		t.Fatalf("waiting for dut set: %v", err)
	}
	leaderEndpoint := dss.leaders()[0]

	// Retrieve key and rev from Curator's leader. We will later test to ensure
	// these have changed when we switch to another leader and back.
	key := dss[leaderEndpoint].leader.lockKey
	rev := dss[leaderEndpoint].leader.lockRev
	leaderNodeID := duts[leaderEndpoint].instance.config.NodeCredentials.ID()
	leaderNum := endpointToNum[leaderEndpoint]

	// Ensure the leader/follower data in the electionStatus are as expected.
	for endpoint, status := range dss {
		if endpoint == leaderEndpoint {
			// The leader instance should not also be a follower.
			if status.follower != nil {
				t.Errorf("leader cannot also be a follower")
			}
		} else {
			// The follower instances should also not be leaders.
			if status.leader != nil {
				t.Errorf("instance %q is leader", endpoint)
			}
			follower := status.follower
			if follower == nil {
				t.Errorf("instance %q is not a follower", endpoint)
				continue
			}
			// The follower instances should point to the leader in their seen lock.
			if want, got := leaderNodeID, follower.lock.NodeId; want != got {
				t.Errorf("instance %q sees node id %q as follower, wanted %q", endpoint, want, got)
			}
		}
	}

	// Partition off leader's etcd instance from other instances.
	for n, member := range cluster.Members {
		if n == leaderNum {
			continue
		}
		cluster.Members[leaderNum].InjectPartition(t, member)
	}

	// Wait until we switch leaders
	dss, err = duts.wait(ctx, func(dss dutSetStatus) bool {
		// Ensure we've lost leadership on the initial leader.
		if i, ok := dss[leaderEndpoint]; !ok || i.leader != nil {
			return false
		}
		return len(dss.leaders()) == 1 && len(dss.followers()) == 1
	})
	if err != nil {
		t.Fatalf("waiting for dut set: %v", err)
	}
	newLeaderEndpoint := dss.leaders()[0]

	// Ensure the old instance is neither leader nor follower (signaling loss of
	// quorum).
	if want, got := false, dss[leaderEndpoint].leader != nil; want != got {
		t.Errorf("old leader's leadership is %v, wanted %v", want, got)
	}
	if want, got := false, dss[leaderEndpoint].follower != nil; want != got {
		t.Errorf("old leader's followership is %v, wanted %v", want, got)
	}

	// Get new leader's key and rev.
	newKey := dss[newLeaderEndpoint].leader.lockKey
	newRev := dss[newLeaderEndpoint].leader.lockRev
	newLeaderNodeID := duts[newLeaderEndpoint].instance.config.NodeCredentials.ID()

	if leaderEndpoint == newLeaderEndpoint {
		t.Errorf("leader endpoint didn't change (%q -> %q)", leaderEndpoint, newLeaderEndpoint)
	}
	if key == newKey {
		t.Errorf("leader election key didn't change (%q -> %q)", key, newKey)
	}
	if rev == newRev {
		t.Errorf("leader election rev didn't change (%d -> %d)", rev, newRev)
	}

	// Ensure the last node of the cluster (not the current leader and not the
	// previous leader) is now a follower pointing at the new leader.
	_, err = duts.wait(ctx, func(dss dutSetStatus) bool {
		for endpoint, status := range dss {
			switch endpoint {
			case leaderEndpoint:
				// Don't care about the old leader, it's still partitioned off.
				continue
			case newLeaderEndpoint:
				// Must be leader.
				if status.leader == nil {
					return false
				}
			default:
				// Other nodes must be following new leader.
				if status.follower == nil {
					return false
				}
				if status.follower.lock.NodeId != newLeaderNodeID {
					return false
				}
			}
		}
		return true
	})
	if err != nil {
		t.Fatalf("waiting for dut set: %v", err)
	}
}
