package selection

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/iotaledger/hive.go/autopeering/peer"
	"github.com/iotaledger/hive.go/autopeering/peer/peertest"
	"github.com/iotaledger/hive.go/autopeering/salt"
	"github.com/iotaledger/hive.go/database/mapdb"
	"github.com/iotaledger/hive.go/events"
	"github.com/iotaledger/hive.go/identity"
	"github.com/stretchr/testify/assert"
)

const (
	testSaltLifetime   = time.Hour     // disable salt updates
	testUpdateInterval = 2 * graceTime // very short update interval to speed up tests
)

func TestMgrNoDuplicates(t *testing.T) {
	const (
		nNeighbors = 4
		nNodes     = 2*nNeighbors + 1
	)
	SetParameters(Parameters{
		OutboundNeighborSize:   nNeighbors,
		InboundNeighborSize:    nNeighbors,
		SaltLifetime:           testSaltLifetime,
		OutboundUpdateInterval: testUpdateInterval,
	})

	mgrMap := make(map[identity.ID]*manager)
	runTestNetwork(nNodes, mgrMap)

	for _, mgr := range mgrMap {
		assert.NotEmpty(t, mgr.getOutNeighbors())
		assert.NotEmpty(t, mgr.getInNeighbors())
		assert.Empty(t, getDuplicates(mgr.getNeighbors()))
	}
}

func TestEvents(t *testing.T) {
	// we want many drops/connects
	const (
		nNeighbors = 2
		nNodes     = 10
	)
	SetParameters(Parameters{
		OutboundNeighborSize:   nNeighbors,
		InboundNeighborSize:    nNeighbors,
		SaltLifetime:           3 * testUpdateInterval,
		OutboundUpdateInterval: testUpdateInterval,
	})

	e, teardown := newEventMock(t)
	defer teardown()
	mgrMap := make(map[identity.ID]*manager)
	runTestNetwork(nNodes, mgrMap)

	// the events should lead to exactly the same neighbors
	for _, mgr := range mgrMap {
		nc := e.m[mgr.getID()]
		assert.ElementsMatchf(t, mgr.getOutNeighbors(), getValues(nc.out),
			"out neighbors of %s do not match", mgr.getID())
		assert.ElementsMatch(t, mgr.getInNeighbors(), getValues(nc.in),
			"in neighbors of %s do not match", mgr.getID())
	}
}

func getValues(m map[identity.ID]*peer.Peer) []*peer.Peer {
	result := make([]*peer.Peer, 0, len(m))
	for _, p := range m {
		result = append(result, p)
	}
	return result
}

func runTestNetwork(n int, mgrMap map[identity.ID]*manager) {
	for i := 0; i < n; i++ {
		_ = newTestManager(fmt.Sprintf("%d", i), mgrMap)
	}
	for _, mgr := range mgrMap {
		mgr.start()
	}

	// give the managers time to potentially connect all other peers
	time.Sleep((time.Duration(n) - 1) * (outboundUpdateInterval + graceTime))

	// close all the managers
	for _, mgr := range mgrMap {
		mgr.close()
	}
}

func getDuplicates(peers []*peer.Peer) []*peer.Peer {
	seen := make(map[identity.ID]bool, len(peers))
	result := make([]*peer.Peer, 0, len(peers))

	for _, p := range peers {
		if !seen[p.ID()] {
			seen[p.ID()] = true
		} else {
			result = append(result, p)
		}
	}

	return result
}

type neighbors struct {
	out, in map[identity.ID]*peer.Peer
}

type eventMock struct {
	t    *testing.T
	lock sync.Mutex
	m    map[identity.ID]neighbors
}

func newEventMock(t *testing.T) (*eventMock, func()) {
	e := &eventMock{
		t: t,
		m: make(map[identity.ID]neighbors),
	}

	outgoingPeeringC := events.NewClosure(e.outgoingPeering)
	incomingPeeringC := events.NewClosure(e.incomingPeering)
	droppedC := events.NewClosure(e.dropped)

	Events.OutgoingPeering.Attach(outgoingPeeringC)
	Events.IncomingPeering.Attach(incomingPeeringC)
	Events.Dropped.Attach(droppedC)

	teardown := func() {
		Events.OutgoingPeering.Detach(outgoingPeeringC)
		Events.IncomingPeering.Detach(incomingPeeringC)
		Events.Dropped.Detach(droppedC)
	}
	return e, teardown
}

func (e *eventMock) outgoingPeering(ev *PeeringEvent) {
	if !ev.Status {
		return
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	s, ok := e.m[ev.Self]
	if !ok {
		s = neighbors{out: make(map[identity.ID]*peer.Peer), in: make(map[identity.ID]*peer.Peer)}
		e.m[ev.Self] = s
	}
	assert.NotContains(e.t, s.out, ev.Peer)
	s.out[ev.Peer.ID()] = ev.Peer
}

func (e *eventMock) incomingPeering(ev *PeeringEvent) {
	if !ev.Status {
		return
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	s, ok := e.m[ev.Self]
	if !ok {
		s = neighbors{out: make(map[identity.ID]*peer.Peer), in: make(map[identity.ID]*peer.Peer)}
		e.m[ev.Self] = s
	}
	assert.NotContains(e.t, s.in, ev.Peer)
	s.in[ev.Peer.ID()] = ev.Peer
}

func (e *eventMock) dropped(ev *DroppedEvent) {
	e.lock.Lock()
	defer e.lock.Unlock()
	if assert.Contains(e.t, e.m, ev.Self) {
		s := e.m[ev.Self]
		delete(s.out, ev.DroppedID)
		delete(s.in, ev.DroppedID)
	}
}

type networkMock struct {
	loc *peer.Local
	mgr map[identity.ID]*manager
}

func (n *networkMock) local() *peer.Local {
	return n.loc
}

func (n *networkMock) PeeringDrop(p *peer.Peer) {
	n.mgr[p.ID()].removeNeighbor(n.local().ID())
}

func (n *networkMock) PeeringRequest(p *peer.Peer, s *salt.Salt) (bool, error) {
	return n.mgr[p.ID()].requestPeering(n.local().Peer, s), nil
}

func (n *networkMock) GetKnownPeers() []*peer.Peer {
	peers := make([]*peer.Peer, 0, len(n.mgr))
	for _, m := range n.mgr {
		peers = append(peers, m.net.local().Peer)
	}
	return peers
}

func newTestManager(name string, mgrMap map[identity.ID]*manager) *manager {
	db, _ := peer.NewDB(mapdb.NewMapDB())
	local := peertest.NewLocal("mock", net.IPv4zero, 0, db)
	networkMock := &networkMock{loc: local, mgr: mgrMap}
	m := newManager(networkMock, networkMock.GetKnownPeers, log.Named(name), &options{})
	mgrMap[m.getID()] = m
	return m
}
