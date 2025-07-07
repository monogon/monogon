// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/testutil"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/integration"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"

	"source.monogon.dev/osbase/event"
	"source.monogon.dev/osbase/logtree"
)

var (
	cluster   *integration.Cluster
	endpoints []string
)

// TestMain brings up a 3 node etcd cluster for tests to use.
func TestMain(m *testing.M) {
	// This logtree's data is not output anywhere.
	lt := logtree.New()

	cfg := integration.ClusterConfig{
		Size:                 3,
		GRPCKeepAliveMinTime: time.Millisecond,
		LoggerBuilder: func(memberName string) *zap.Logger {
			dn := logtree.DN("etcd." + memberName)
			return logtree.Zapify(lt.MustLeveledFor(dn), zap.WarnLevel)
		},
	}
	tb, cancel := testutil.NewTestingTBProthesis("curator")
	defer cancel()
	flag.Parse()
	integration.BeforeTestExternal(tb)
	grpclog.SetLoggerV2(logtree.GRPCify(lt.MustLeveledFor("grpc")))
	cluster = integration.NewCluster(tb, &cfg)
	endpoints = make([]string, 3)
	for i := range endpoints {
		endpoints[i] = cluster.Client(i).Endpoints()[0]
	}

	v := m.Run()
	cluster.Terminate(tb)
	os.Exit(v)
}

// setRaceWg creates a new WaitGroup and sets the given watcher to wait on this
// WG after it performs the initial retrieval of a value from etcd, but before
// it starts the watcher. This is used to test potential race conditions
// present between these two steps.
func setRaceWg[T any](w event.Watcher[T]) *sync.WaitGroup {
	var wg sync.WaitGroup
	w.(*watcher[T]).testRaceWG = &wg
	return &wg
}

// setSetupWg creates a new WaitGroup and sets the given watcher to wait on
// thie WG after an etcd watch channel is created. This is used in tests to
// ensure that the watcher is fully created before it is tested.
func setSetupWg[T any](w event.Watcher[T]) *sync.WaitGroup {
	var wg sync.WaitGroup
	w.(*watcher[T]).testSetupWG = &wg
	return &wg
}

// testClient is an etcd connection to the test cluster.
type testClient struct {
	client *clientv3.Client
}

func newTestClient(t *testing.T) *testClient {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:            endpoints,
		DialTimeout:          1 * time.Second,
		DialKeepAliveTime:    1 * time.Second,
		DialKeepAliveTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("clientv3.New: %v", err)
	}

	return &testClient{
		client: cli,
	}
}

func (d *testClient) close() {
	d.client.Close()
}

// setEndpoints configures which endpoints (from {0,1,2}) the testClient is
// connected to.
func (d *testClient) setEndpoints(nums ...uint) {
	var eps []string
	for _, num := range nums {
		eps = append(eps, endpoints[num])
	}
	d.client.SetEndpoints(eps...)
}

// put uses the testClient to store key with a given string value in etcd. It
// contains retry logic that will block until the put is successful.
func (d *testClient) put(t *testing.T, key, value string) {
	t.Helper()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	for {
		ctxT, ctxC := context.WithTimeout(ctx, 100*time.Millisecond)
		_, err := d.client.Put(ctxT, key, value)
		ctxC()
		if err == nil {
			return
		}
		if errors.Is(err, ctxT.Err()) {
			log.Printf("Retrying after %v", err)
			continue
		}
		// Retry on etcd unavailability - this will happen in this code as the
		// etcd cluster repeatedly loses quorum.
		var eerr rpctypes.EtcdError
		if errors.As(err, &eerr) && eerr.Code() == codes.Unavailable {
			log.Printf("Retrying after %v", err)
			continue
		}
		t.Fatalf("Put: %v", err)
	}

}

// remove uses the testClient to remove the given key from etcd. It contains
// retry logic that will block until the removal is successful.
func (d *testClient) remove(t *testing.T, key string) {
	t.Helper()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	_, err := d.client.Delete(ctx, key)
	if err == nil {
		return
	}
	t.Fatalf("Delete: %v", err)
}

// expect runs a Get on the given Watcher, ensuring the returned value is a
// given string.
func expect(t *testing.T, w event.Watcher[StringAt], value string) {
	t.Helper()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	got, err := w.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got, want := got.Value, value; got != want {
		t.Errorf("Wanted value %q, got %q", want, got)
	}
}

// expectTimeout ensures that the given watcher blocks on a Get call for at
// least 100 milliseconds. This is used by tests to attempt to verify that the
// watcher Get is fully blocked, but can cause false positives (eg. when Get
// blocks for 101 milliseconds). Thus, this function should be used sparingly
// and in tests that perform other baseline behaviour checks alongside this
// test.
func expectTimeout[T any](t *testing.T, w event.Watcher[T]) {
	t.Helper()
	ctx, ctxC := context.WithTimeout(context.Background(), 100*time.Millisecond)
	got, err := w.Get(ctx)
	ctxC()

	if !errors.Is(err, ctx.Err()) {
		t.Fatalf("Expected timeout error, got %v, %v", got, err)
	}
}

// wait wraps a watcher into a channel of strings, ensuring that the watcher
// never errors on Get calls and always returns strings.
func wait(t *testing.T, w event.Watcher[StringAt]) (chan string, func()) {
	t.Helper()
	ctx, ctxC := context.WithCancel(context.Background())

	c := make(chan string)

	go func() {
		for {
			got, err := w.Get(ctx)
			if err != nil && errors.Is(err, ctx.Err()) {
				return
			}
			if err != nil {
				t.Errorf("Get: %v", err)
				close(c)
				return
			}
			c <- got.Value
		}
	}()

	return c, ctxC
}

// TestSimple exercises the simplest possible interaction with a watched value.
func TestSimple(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-simple"
	value := NewValue(tc.client, k, DecoderStringAt)
	tc.put(t, k, "one")

	watcher := value.Watch()
	defer watcher.Close()
	expect(t, watcher, "one")

	tc.put(t, k, "two")
	expect(t, watcher, "two")

	tc.put(t, k, "three")
	tc.put(t, k, "four")
	tc.put(t, k, "five")
	tc.put(t, k, "six")

	q, cancel := wait(t, watcher)
	// Test will hang here if the above value does not receive the set "six".
	log.Printf("a")
	for el := range q {
		log.Printf("%q", el)
		if el == "six" {
			break
		}
	}
	log.Printf("b")
	cancel()
}

// stringAtGet performs a Get from a Watcher, expecting a stringAt and updating
// the given map with the retrieved value.
func stringAtGet(ctx context.Context, t *testing.T, w event.Watcher[StringAt], m map[string]string) {
	t.Helper()

	vr, err := w.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	m[vr.Key] = vr.Value
}

// TestSimpleRange exercises the simplest behaviour of a ranged watcher,
// retrieving updaates via Get in a fully blocking fashion.
func TestSimpleRange(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	ks := "test-simple-range/"
	ke := "test-simple-range0"
	value := NewValue(tc.client, ks, DecoderStringAt, Range(ke))
	tc.put(t, ks+"a", "one")
	tc.put(t, ks+"b", "two")
	tc.put(t, ks+"c", "three")
	tc.put(t, ks+"b", "four")

	w := value.Watch()
	defer w.Close()

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	res := make(map[string]string)
	stringAtGet(ctx, t, w, res)
	stringAtGet(ctx, t, w, res)
	stringAtGet(ctx, t, w, res)

	tc.put(t, ks+"a", "five")
	tc.put(t, ks+"e", "six")

	stringAtGet(ctx, t, w, res)
	stringAtGet(ctx, t, w, res)

	for _, te := range []struct {
		k, w string
	}{
		{ks + "a", "five"},
		{ks + "b", "four"},
		{ks + "c", "three"},
		{ks + "e", "six"},
	} {
		if want, got := te.w, res[te.k]; want != got {
			t.Errorf("res[%q]: wanted %q, got %q", te.k, want, got)
		}
	}
}

// TestCancel ensures that watchers can resume after being canceled.
func TestCancel(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-cancel"
	value := NewValue(tc.client, k, DecoderStringAt)
	tc.put(t, k, "one")

	watcher := value.Watch()
	defer watcher.Close()
	expect(t, watcher, "one")

	ctx, ctxC := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := watcher.Get(ctx)
		errs <- err
	}()
	ctxC()
	if want, got := ctx.Err(), <-errs; !errors.Is(got, want) {
		t.Fatalf("Wanted err %v, got %v", want, got)
	}

	// Successfully canceled watch, resuming should continue to work.
	q, cancel := wait(t, watcher)
	defer cancel()

	tc.put(t, k, "two")
	if want, got := "two", <-q; want != got {
		t.Fatalf("Wanted val %q, got %q", want, got)
	}
}

// TestCancelOnGet ensures that a context cancellation on an initial Get (which
// translates to an etcd Get in a backoff loop) doesn't block.
func TestCancelOnGet(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-cancel-on-get"
	value := NewValue(tc.client, k, DecoderStringAt)
	watcher := value.Watch()
	tc.put(t, k, "one")

	// Cause partition between client endpoint and rest of cluster. Any read/write
	// operations will now hang.
	tc.setEndpoints(0)
	cluster.Members[0].InjectPartition(t, cluster.Members[1], cluster.Members[2])
	// Let raft timeouts expire so that the leader is aware a partition has occurred
	// and stops serving data if it is not part of a quorum anymore.
	//
	// Otherwise, if Member[0] was the leader, there will be a window of opportunity
	// during which it will continue to serve read data even though it has been
	// partitioned off. This is an effect of how etcd handles linearizable reads:
	// they go through the leader, but do not go through raft.
	//
	// The value is the default etcd leader timeout (1s) + some wiggle room.
	time.Sleep(time.Second + time.Millisecond*100)

	// Perform the initial Get(), which should attempt to retrieve a KV entry from
	// the etcd service. This should hang. Unfortunately, there's no easy way to do
	// this without an arbitrary sleep hoping that the client actually gets to the
	// underlying etcd.Get call. This can cause false positives (eg. false 'pass'
	// results) in this test.
	ctx, ctxC := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := watcher.Get(ctx)
		errs <- err
	}()
	time.Sleep(time.Second)

	// Now that the etcd.Get is hanging, cancel the context.
	ctxC()
	// And now unpartition the cluster, resuming reads.
	cluster.Members[0].RecoverPartition(t, cluster.Members[1], cluster.Members[2])

	// The etcd.Get() call should've returned with a context cancellation.
	err := <-errs
	switch {
	case err == nil:
		t.Errorf("watcher.Get() returned no error, wanted context error")
	case errors.Is(err, ctx.Err()):
		// Okay.
	default:
		t.Errorf("watcher.Get() returned %v, wanted context error", err)
	}
}

// TestClientReconnect forces a 'reconnection' of an active watcher from a
// running member to another member, by stopping the original member and
// explicitly reconnecting the client to other available members.
//
// This doe not reflect a situation expected during Metropolis runtime, as we
// do not expect splits between an etcd client and its connected member
// (instead, all etcd clients only connect to their local member). However, it
// is still an important safety test to perform, and it also exercies the
// equivalent behaviour of an etcd client re-connecting for any other reason.
func TestClientReconnect(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()
	tc.setEndpoints(0)

	k := "test-client-reconnect"
	value := NewValue(tc.client, k, DecoderStringAt)
	tc.put(t, k, "one")

	watcher := value.Watch()
	defer watcher.Close()
	expect(t, watcher, "one")

	q, cancel := wait(t, watcher)
	defer cancel()

	cluster.Members[0].Stop(t)
	defer cluster.Members[0].Restart(t)
	cluster.WaitLeader(t)

	tc.setEndpoints(1, 2)
	tc.put(t, k, "two")

	if want, got := "two", <-q; want != got {
		t.Fatalf("Watcher received incorrect data after client restart, wanted %q, got %q", want, got)
	}
}

// TestClientPartition forces a temporary partition of the etcd member while a
// watcher is running, updates the value from across the partition, and undoes
// the partition.
// The partition is expected to be entirely transparent to the watcher.
func TestClientPartition(t *testing.T) {
	tcOne := newTestClient(t)
	defer tcOne.close()
	tcOne.setEndpoints(0)

	tcRest := newTestClient(t)
	defer tcRest.close()
	tcRest.setEndpoints(1, 2)

	k := "test-client-partition"
	valueOne := NewValue(tcOne.client, k, DecoderStringAt)
	watcherOne := valueOne.Watch()
	defer watcherOne.Close()
	valueRest := NewValue(tcRest.client, k, DecoderStringAt)
	watcherRest := valueRest.Watch()
	defer watcherRest.Close()

	tcRest.put(t, k, "a")
	expect(t, watcherOne, "a")
	expect(t, watcherRest, "a")

	cluster.Members[0].InjectPartition(t, cluster.Members[1], cluster.Members[2])

	tcRest.put(t, k, "b")
	expect(t, watcherRest, "b")
	expectTimeout(t, watcherOne)

	cluster.Members[0].RecoverPartition(t, cluster.Members[1], cluster.Members[2])

	expect(t, watcherOne, "b")
	tcRest.put(t, k, "c")
	expect(t, watcherOne, "c")
	expect(t, watcherRest, "c")

}

// TestEarlyUse exercises the correct behaviour of the value watcher on a value
// that is not yet set.
func TestEarlyUse(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-early-use"

	value := NewValue(tc.client, k, DecoderStringAt)
	watcher := value.Watch()
	defer watcher.Close()

	wg := setSetupWg(watcher)
	wg.Add(1)
	q, cancel := wait(t, watcher)
	defer cancel()

	wg.Done()

	tc.put(t, k, "one")

	if want, got := "one", <-q; want != got {
		t.Fatalf("Expected %q, got %q", want, got)
	}
}

// TestRemove exercises the basic functionality of handling deleted values.
func TestRemove(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-remove"
	tc.put(t, k, "one")

	value := NewValue(tc.client, k, DecoderStringAt)
	watcher := value.Watch()
	defer watcher.Close()

	expect(t, watcher, "one")
	tc.remove(t, k)
	expect(t, watcher, "")
}

// TestRemoveRange exercises the behaviour of a Get on a ranged watcher when a
// value is removed.
func TestRemoveRange(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	ks := "test-remove-range/"
	ke := "test-remove-range0"
	value := NewValue(tc.client, ks, DecoderStringAt, Range(ke))
	tc.put(t, ks+"a", "one")
	tc.put(t, ks+"b", "two")
	tc.put(t, ks+"c", "three")
	tc.put(t, ks+"b", "four")
	tc.remove(t, ks+"c")

	w := value.Watch()
	defer w.Close()

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	res := make(map[string]string)
	stringAtGet(ctx, t, w, res)
	stringAtGet(ctx, t, w, res)

	for _, te := range []struct {
		k, w string
	}{
		{ks + "a", "one"},
		{ks + "b", "four"},
		{ks + "c", ""},
	} {
		if want, got := te.w, res[te.k]; want != got {
			t.Errorf("res[%q]: wanted %q, got %q", te.k, want, got)
		}
	}
}

// TestEmptyRace forces the watcher to retrieve an empty value from the K/V
// store at first, and establishing the watch channel after a new value has
// been stored in the same place.
func TestEmptyRace(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-remove-race"
	tc.put(t, k, "one")
	tc.remove(t, k)

	value := NewValue(tc.client, k, DecoderStringAt)
	watcher := value.Watch()
	defer watcher.Close()

	wg := setRaceWg(watcher)
	wg.Add(1)
	q, cancel := wait(t, watcher)
	defer cancel()

	tc.put(t, k, "two")
	wg.Done()

	if want, got := "two", <-q; want != got {
		t.Fatalf("Watcher received incorrect data after client restart, wanted %q, got %q", want, got)
	}
}

type errOrInt struct {
	val int64
	err error
}

// TestDecoder exercises the BytesDecoder functionality of the watcher, by
// creating a value with a decoder that only accepts string-encoded integers
// that are divisible by three. The test then proceeds to put a handful of
// values into etcd, ensuring that undecodable values correctly return an error
// on Get, but that the watcher continues to work after the error has been
// returned.
func TestDecoder(t *testing.T) {
	decoderDivisibleByThree := func(_, value []byte) (int64, error) {
		num, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("not a valid number")
		}
		if (num % 3) != 0 {
			return 0, fmt.Errorf("not divisible by 3")
		}
		return num, nil
	}

	tc := newTestClient(t)
	defer tc.close()

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	k := "test-decoder"
	value := NewValue(tc.client, k, decoderDivisibleByThree)
	watcher := value.Watch()
	defer watcher.Close()
	tc.put(t, k, "3")
	_, err := watcher.Get(ctx)
	if err != nil {
		t.Fatalf("Initial Get: %v", err)
	}

	// Stream updates into arbitrarily-bounded test channel.
	queue := make(chan errOrInt, 100)
	go func() {
		for {
			res, err := watcher.Get(ctx)
			if err != nil && errors.Is(err, ctx.Err()) {
				return
			}
			if err != nil {
				queue <- errOrInt{
					err: err,
				}
			} else {
				queue <- errOrInt{
					val: res,
				}
			}
		}
	}()

	var wantList []*int64
	wantError := func(val string) {
		wantList = append(wantList, nil)
		tc.put(t, k, val)
	}
	wantValue := func(val string, decoded int64) {
		wantList = append(wantList, &decoded)
		tc.put(t, k, val)
	}

	wantError("")
	wantValue("9", 9)
	wantError("foo")
	wantValue("18", 18)
	wantError("10")
	wantValue("27", 27)
	wantValue("36", 36)

	for i, want := range wantList {
		q := <-queue
		if want == nil && q.err == nil {
			t.Fatalf("Case %d: wanted error, got no error and value %d", i, q.val)
		}
		if want != nil && (*want) != q.val {
			t.Fatalf("Case %d: wanted value %d, got error %v and value %d", i, *want, q.err, q.val)
		}
	}
}

// TestBacklog ensures that the watcher can handle a large backlog of changes
// in etcd that the client didnt' keep up with, and that whatever final state
// is available to the client when it actually gets to calling Get().
func TestBacklog(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	k := "test-backlog"
	value := NewValue(tc.client, k, DecoderStringAt)
	watcher := value.Watch()
	defer watcher.Close()

	tc.put(t, k, "initial")
	expect(t, watcher, "initial")

	for i := 0; i < 1000; i++ {
		tc.put(t, k, fmt.Sprintf("val-%d", i))
	}

	ctx, ctxC := context.WithTimeout(context.Background(), time.Second)
	defer ctxC()
	for {
		valB, err := watcher.Get(ctx)
		if err != nil {
			t.Fatalf("Get() returned error before expected final value: %v", err)
		}
		if valB.Value == "val-999" {
			break
		}
	}
}

// TestBacklogRange ensures that the ranged etcd watcher can handle a large
// backlog of changes in etcd that the client didn't keep up with.
func TestBacklogRange(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()

	ks := "test-backlog-range/"
	ke := "test-backlog-range0"
	value := NewValue(tc.client, ks, DecoderStringAt, Range(ke))
	w := value.Watch()
	defer w.Close()

	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			tc.put(t, ks+"a", fmt.Sprintf("val-%d", i))
		} else {
			tc.put(t, ks+"b", fmt.Sprintf("val-%d", i))
		}
	}

	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	res := make(map[string]string)
	stringAtGet(ctx, t, w, res)
	stringAtGet(ctx, t, w, res)

	for _, te := range []struct {
		k, w string
	}{
		{ks + "a", "val-98"},
		{ks + "b", "val-99"},
	} {
		if want, got := te.w, res[te.k]; want != got {
			t.Errorf("res[%q]: wanted %q, got %q", te.k, want, got)
		}
	}
}

// TestBacklogOnly exercises the BacklogOnly option for non-ranged watchers,
// which effectively makes any Get operation non-blocking (but also showcases
// that unless a Get without BacklogOnly is issues, no new data will appear by
// itself in the watcher - which is an undocumented implementation detail of the
// option).
func TestBacklogOnly(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	k := "test-backlog-only"
	tc.put(t, k, "initial")

	value := NewValue(tc.client, k, DecoderStringAt)
	watcher := value.Watch()
	defer watcher.Close()

	d, err := watcher.Get(ctx, event.BacklogOnly[StringAt]())
	if err != nil {
		t.Fatalf("First Get failed: %v", err)
	}
	if want, got := "initial", d.Value; want != got {
		t.Fatalf("First Get: wanted value %q, got %q", want, got)
	}

	// As expected, next call to Get with BacklogOnly fails - there truly is no new
	// updates to emit.
	_, err = watcher.Get(ctx, event.BacklogOnly[StringAt]())
	if want, got := event.ErrBacklogDone, err; !errors.Is(got, want) {
		t.Fatalf("Second Get: wanted %v, got %v", want, got)
	}

	// Implementation detail: even though there is a new value ('second'),
	// BacklogOnly will still return ErrBacklogDone.
	tc.put(t, k, "second")
	_, err = watcher.Get(ctx, event.BacklogOnly[StringAt]())
	if want, got := event.ErrBacklogDone, err; !errors.Is(got, want) {
		t.Fatalf("Third Get: wanted %v, got %v", want, got)
	}

	// ... However, a Get  without BacklogOnly will return the new value.
	d, err = watcher.Get(ctx)
	if err != nil {
		t.Fatalf("Fourth Get failed: %v", err)
	}
	if want, got := "second", d.Value; want != got {
		t.Fatalf("Fourth Get: wanted value %q, got %q", want, got)
	}
}

// TestBacklogOnlyRange exercises the BacklogOnly option for ranged watchers,
// showcasing how it expected to be used for keeping up with the external state
// of a range by synchronizing to a local map.
func TestBacklogOnlyRange(t *testing.T) {
	tc := newTestClient(t)
	defer tc.close()
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	ks := "test-backlog-only-range/"
	ke := "test-backlog-only-range0"

	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			tc.put(t, ks+"a", fmt.Sprintf("val-%d", i))
		} else {
			tc.put(t, ks+"b", fmt.Sprintf("val-%d", i))
		}
	}

	value := NewValue(tc.client, ks, DecoderStringAt, Range(ke))
	w := value.Watch()
	defer w.Close()

	// Collect results into a map from key to value.
	res := make(map[string]string)

	// Run first Get - this is the barrier defining what's part of the backlog.
	g, err := w.Get(ctx, event.BacklogOnly[StringAt]())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	res[g.Key] = g.Value

	// These won't be part of the backlog.
	tc.put(t, ks+"a", "val-100")
	tc.put(t, ks+"b", "val-101")

	// Retrieve the rest of the backlog until ErrBacklogDone is returned.
	nUpdates := 1
	for {
		g, err := w.Get(ctx, event.BacklogOnly[StringAt]())
		if errors.Is(err, event.ErrBacklogDone) {
			break
		}
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		nUpdates += 1
		res[g.Key] = g.Value
	}

	// The backlog should've been compacted to just two entries at their newest
	// state.
	if want, got := 2, nUpdates; want != got {
		t.Fatalf("wanted backlog in %d updates, got it in %d", want, got)
	}

	for _, te := range []struct {
		k, w string
	}{
		{ks + "a", "val-98"},
		{ks + "b", "val-99"},
	} {
		if want, got := te.w, res[te.k]; want != got {
			t.Errorf("res[%q]: wanted %q, got %q", te.k, want, got)
		}
	}
}
