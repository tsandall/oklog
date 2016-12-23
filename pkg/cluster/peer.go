// Package cluster provides an elastic peer discovery and gossip layer.
//
// Ingest and store instances join the same cluster and know about each other.
// Store instances consume segments from each ingest instance, and broadcast
// queries to each store instance. In the future, ingest instances will share
// load information to potentially refuse connections and balance writes.
package cluster

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	level "github.com/go-kit/kit/log/experimental_level"
	"github.com/hashicorp/memberlist"
	"github.com/pborman/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// Peer represents this node in the cluster.
type Peer struct {
	ml *memberlist.Memberlist
	d  *delegate
}

// PeerType enumerates the two types of nodes in the cluster.
// A peer may be one or the other.
type PeerType string

const (
	// PeerTypeIngest serves the ingest API.
	PeerTypeIngest PeerType = "ingest"

	// PeerTypeStore serves the store API.
	PeerTypeStore = "store"

	// PeerTypeIngestStore serves both ingest and store APIs.
	PeerTypeIngestStore = "ingeststore"
)

// NewPeer creates or joins a cluster with the existing peers.
// We will listen for cluster communications on the given addr:port.
// We advertise a PeerType HTTP API, reachable on apiPort.
func NewPeer(addr string, port int, existing []string, t PeerType, apiPort int, delegateInvocations *prometheus.CounterVec, logger log.Logger) (*Peer, error) {
	d := newDelegate(delegateInvocations, logger)
	conf := memberlistConfig(addr, port, d, d)
	ml, err := memberlist.Create(conf)
	if err != nil {
		return nil, err
	}
	d.init(conf.Name, t, ml.LocalNode().Addr.String(), apiPort, ml.NumMembers)
	n, err := ml.Join(existing)
	if err != nil {
		return nil, err
	}
	level.Debug(logger).Log("Join", n)
	return &Peer{
		ml: ml,
		d:  d,
	}, nil
}

// Leave the cluster, waiting up to timeout.
func (p *Peer) Leave(timeout time.Duration) error {
	return p.ml.Leave(timeout)
}

// Current API host:ports for the given type of node.
func (p *Peer) Current(t PeerType) []string {
	return p.d.current(t)
}

// Name returns the unique ID of this peer in the cluster.
func (p *Peer) Name() string {
	return p.ml.LocalNode().Name
}

// ClusterSize returns the total size of the cluster from this node's perspective.
func (p *Peer) ClusterSize() int {
	return p.ml.NumMembers()
}

// State returns a JSON-serializable dump of cluster state.
// Useful for debug.
func (p *Peer) State() map[string]interface{} {
	return map[string]interface{}{
		"self":     p.ml.LocalNode(),
		"members":  p.ml.Members(),
		"n":        p.ml.NumMembers(),
		"delegate": p.d.state(),
	}
}

func memberlistConfig(addr string, port int, delegate memberlist.Delegate, events memberlist.EventDelegate) *memberlist.Config {
	config := memberlist.DefaultLANConfig()
	config.Name = uuid.New()
	config.BindAddr = addr
	config.BindPort = port
	config.LogOutput = ioutil.Discard
	config.Delegate = delegate
	config.Events = events
	return config
}

// delegate manages gossiped data: the set of peers, their type, and API port.
// Clients must invoke init before the delegate can be used.
// Inspired by https://github.com/asim/memberlist/blob/master/memberlist.go
type delegate struct {
	mtx         sync.RWMutex
	bcast       *memberlist.TransmitLimitedQueue
	data        map[string]peerInfo
	invocations *prometheus.CounterVec // method
	logger      log.Logger
}

type peerInfo struct {
	Type    PeerType `json:"type"`
	APIAddr string   `json:"api_addr"`
	APIPort int      `json:"api_port"`
}

func newDelegate(invocations *prometheus.CounterVec, logger log.Logger) *delegate {
	return &delegate{
		bcast:       nil,
		data:        map[string]peerInfo{},
		invocations: invocations,
		logger:      logger,
	}
}

func (d *delegate) init(myName string, myType PeerType, apiAddr string, apiPort int, numNodes func() int) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	// As far as I can tell, it is only luck which ensures the d.bcast isn't
	// used (via GetBroadcasts) before we have a chance to create it here. But I
	// don't see a way to wire up the components (in NewPeer) that doesn't
	// involve this roundabout sort of initialization. Shrug!
	d.bcast = &memberlist.TransmitLimitedQueue{
		NumNodes:       numNodes,
		RetransmitMult: 3,
	}
	d.data[myName] = peerInfo{myType, apiAddr, apiPort}
}

func (d *delegate) current(t PeerType) (res []string) {
	for _, info := range d.state() {
		var (
			matchIngest      = t == PeerTypeIngest && (info.Type == PeerTypeIngest || info.Type == PeerTypeIngestStore)
			matchStore       = t == PeerTypeStore && (info.Type == PeerTypeStore || info.Type == PeerTypeIngestStore)
			matchIngestStore = t == PeerTypeIngestStore && info.Type == PeerTypeIngestStore
		)
		if matchIngest || matchStore || matchIngestStore {
			res = append(res, net.JoinHostPort(info.APIAddr, strconv.Itoa(info.APIPort)))
		}
	}
	return res
}

func (d *delegate) state() map[string]peerInfo {
	d.mtx.RLock()
	defer d.mtx.RUnlock()
	return d.data
}

// NodeMeta is used to retrieve meta-data about the current node
// when broadcasting an alive message. It's length is limited to
// the given byte size. This metadata is available in the Node structure.
// Implements memberlist.Delegate.
func (d *delegate) NodeMeta(limit int) []byte {
	d.invocations.WithLabelValues("NodeMeta").Inc()
	d.mtx.RLock()
	defer d.mtx.RUnlock()
	return []byte{} // no metadata
}

// NotifyMsg is called when a user-data message is received.
// Care should be taken that this method does not block, since doing
// so would block the entire UDP packet receive loop. Additionally, the byte
// slice may be modified after the call returns, so it should be copied if needed.
// Implements memberlist.Delegate.
func (d *delegate) NotifyMsg(b []byte) {
	d.invocations.WithLabelValues("NotifyMsg").Inc()
	if len(b) == 0 {
		return
	}
	var data map[string]peerInfo
	if err := json.Unmarshal(b, &data); err != nil {
		level.Error(d.logger).Log("method", "NotifyMsg", "b", strings.TrimSpace(string(b)), "err", err)
		return
	}
	d.mtx.Lock()
	defer d.mtx.Unlock()
	for k, v := range data {
		// Removing data is handled by NotifyLeave
		d.data[k] = v
	}
}

// GetBroadcasts is called when user data messages can be broadcast.
// It can return a list of buffers to send. Each buffer should assume an
// overhead as provided with a limit on the total byte size allowed.
// The total byte size of the resulting data to send must not exceed
// the limit. Care should be taken that this method does not block,
// since doing so would block the entire UDP packet receive loop.
// Implements memberlist.Delegate.
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	d.invocations.WithLabelValues("GetBroadcasts").Inc()
	d.mtx.RLock()
	defer d.mtx.RUnlock()
	if d.bcast == nil {
		panic("GetBroadcast before init")
	}
	return d.bcast.GetBroadcasts(overhead, limit)
}

// LocalState is used for a TCP Push/Pull. This is sent to
// the remote side in addition to the membership information. Any
// data can be sent here. See MergeRemoteState as well. The `join`
// boolean indicates this is for a join instead of a push/pull.
// Implements memberlist.Delegate.
func (d *delegate) LocalState(join bool) []byte {
	d.invocations.WithLabelValues("LocalState").Inc()
	d.mtx.RLock()
	defer d.mtx.RUnlock()
	buf, err := json.Marshal(d.data)
	if err != nil {
		panic(err)
	}
	return buf
}

// MergeRemoteState is invoked after a TCP Push/Pull. This is the
// state received from the remote side and is the result of the
// remote side's LocalState call. The 'join'
// boolean indicates this is for a join instead of a push/pull.
// Implements memberlist.Delegate.
func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	d.invocations.WithLabelValues("MergeRemoteState").Inc()
	if len(buf) == 0 {
		level.Debug(d.logger).Log("method", "MergeRemoteState", "join", join, "buf_sz", 0)
		return
	}
	var data map[string]peerInfo
	if err := json.Unmarshal(buf, &data); err != nil {
		level.Error(d.logger).Log("method", "MergeRemoteState", "err", err)
		return
	}
	d.mtx.Lock()
	defer d.mtx.Unlock()
	for k, v := range data {
		d.data[k] = v
	}
}

// NotifyJoin is invoked when a node is detected to have joined.
// The Node argument must not be modified.
// Implements memberlist.EventDelegate.
func (d *delegate) NotifyJoin(n *memberlist.Node) {
	d.invocations.WithLabelValues("NotifyJoin").Inc()
}

// NotifyUpdate is invoked when a node is detected to have updated, usually
// involving the meta data. The Node argument must not be modified.
// Implements memberlist.EventDelegate.
func (d *delegate) NotifyUpdate(n *memberlist.Node) {
	d.invocations.WithLabelValues("NotifyUpdate").Inc()
}

// NotifyLeave is invoked when a node is detected to have left.
// The Node argument must not be modified.
// Implements memberlist.EventDelegate.
func (d *delegate) NotifyLeave(n *memberlist.Node) {
	d.invocations.WithLabelValues("NotifyLeave").Inc()
	d.mtx.Lock()
	defer d.mtx.Unlock()
	delete(d.data, n.Name)
}
