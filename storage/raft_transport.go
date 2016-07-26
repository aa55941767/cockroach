// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package storage

import (
	"net"
	"time"

	"github.com/coreos/etcd/raft/raftpb"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/retry"
	"github.com/cockroachdb/cockroach/util/syncutil"
	"github.com/cockroachdb/cockroach/util/timeutil"
)

const (
	// Outgoing messages are queued per-replica on a channel of this size.
	raftSendBufferSize = 100

	// When no message has been queued for this duration, the corresponding
	// instance of processQueue will shut down.
	//
	// TODO(tamird): make culling of outbound streams more evented, so that we
	// need not rely on this timeout to shut things down.
	raftIdleTimeout = time.Minute
)

var (
	// Initial retry backoff for resolving node ID -> Address.
	InitialResolveBackoff = time.Second
)

type raftMessageHandler func(*RaftMessageRequest) error

// NodeAddressResolver is the function used by RaftTransport to map node IDs to
// network addresses.
type NodeAddressResolver func(roachpb.NodeID) (net.Addr, error)

// GossipAddressResolver is a thin wrapper around gossip's GetNodeIDAddress
// that allows its return value to be used as the net.Addr interface.
func GossipAddressResolver(gossip *gossip.Gossip) NodeAddressResolver {
	return func(nodeID roachpb.NodeID) (net.Addr, error) {
		return gossip.GetNodeIDAddress(nodeID)
	}
}

// RaftSnapshotStatus contains a MsgSnap message and its resulting
// error, for asynchronous notification of completion.
type RaftSnapshotStatus struct {
	Req *RaftMessageRequest
	Err error
}

// RaftTransport handles the rpc messages for raft.
//
// The raft transport is asynchronous with respect to the caller, and
// internally multiplexes outbound messages. Internally, each message is
// queued on a per-destination queue before being asynchronously delivered.
//
// Callers are required to construct a RaftSender before being able to
// dispatch messages, and must provide an error handler which will be invoked
// asynchronously in the event that the recipient of any message closes its
// inbound RPC stream. This callback is asynchronous with respect to the
// outbound message which caused the remote to hang up; all that is known is
// which remote hung up.
type RaftTransport struct {
	resolver           NodeAddressResolver
	rpcContext         *rpc.Context
	SnapshotStatusChan chan RaftSnapshotStatus

	mu struct {
		syncutil.Mutex
		handlers    map[roachpb.StoreID]raftMessageHandler
		queues      map[bool]map[roachpb.ReplicaDescriptor]chan *RaftMessageRequest
		addrLookups map[roachpb.NodeID]chan struct{}
	}
}

// NewDummyRaftTransport returns a dummy raft transport for use in tests which
// need a non-nil raft transport that need not function.
func NewDummyRaftTransport() *RaftTransport {
	return NewRaftTransport(nil, nil, nil)
}

// NewRaftTransport creates a new RaftTransport with specified resolver and grpc server.
// Callers are responsible for monitoring RaftTransport.SnapshotStatusChan.
func NewRaftTransport(resolver NodeAddressResolver, grpcServer *grpc.Server, rpcContext *rpc.Context) *RaftTransport {
	t := &RaftTransport{
		resolver:           resolver,
		rpcContext:         rpcContext,
		SnapshotStatusChan: make(chan RaftSnapshotStatus),
	}
	t.mu.handlers = make(map[roachpb.StoreID]raftMessageHandler)
	t.mu.queues = make(map[bool]map[roachpb.ReplicaDescriptor]chan *RaftMessageRequest)
	t.mu.addrLookups = make(map[roachpb.NodeID]chan struct{})

	if grpcServer != nil {
		RegisterMultiRaftServer(grpcServer, t)
	}

	return t
}

// RaftMessage proxies the incoming request to the listening server interface.
func (t *RaftTransport) RaftMessage(stream MultiRaft_RaftMessageServer) (err error) {
	errCh := make(chan error, 1)

	// Node stopping error is caught below in the select.
	if err := t.rpcContext.Stopper.RunTask(func() {
		t.rpcContext.Stopper.RunWorker(func() {
			errCh <- func() error {
				for {
					req, err := stream.Recv()
					if err != nil {
						return err
					}

					t.mu.Lock()
					handler, ok := t.mu.handlers[req.ToReplica.StoreID]
					t.mu.Unlock()

					if !ok {
						return errors.Errorf(
							"unable to accept Raft message from %+v: no store registered for %+v",
							req.ToReplica, req.FromReplica)
					}

					if err := handler(req); err != nil {
						return err
					}
				}
			}()
		})
	}); err != nil {
		return err
	}

	select {
	case err := <-errCh:
		return err
	case <-t.rpcContext.Stopper.ShouldQuiesce():
		return stream.SendAndClose(new(RaftMessageResponse))
	}
}

// Listen registers a raftMessageHandler to receive proxied messages.
func (t *RaftTransport) Listen(storeID roachpb.StoreID, handler raftMessageHandler) {
	t.mu.Lock()
	t.mu.handlers[storeID] = handler
	t.mu.Unlock()
}

// Stop unregisters a raftMessageHandler.
func (t *RaftTransport) Stop(storeID roachpb.StoreID) {
	t.mu.Lock()
	delete(t.mu.handlers, storeID)
	t.mu.Unlock()
}

// This method handles backoff / retry for nodes which aren't being
// communicated by gossip (yet -- as in the case of reconstituting a
// cluster from copied data or after a long downtime)
func (t *RaftTransport) resolveNodeID(nodeID roachpb.NodeID) (net.Addr, error) {
	t.mu.Lock()
	ch, ok := t.mu.addrLookups[nodeID]
	if !ok {
		ch = make(chan struct{})
		t.mu.addrLookups[nodeID] = ch
	}
	t.mu.Unlock()
	if ok {
		select {
		case <-ch:
			return t.resolver(nodeID)
		case <-t.rpcContext.Stopper.ShouldQuiesce():
			return nil, errors.Errorf("address resolution for %s stopped before completion", nodeID)
		}
	}

	defer func() {
		t.mu.Lock()
		delete(t.mu.addrLookups, nodeID)
		t.mu.Unlock()
	}()

	opts := retry.Options{
		InitialBackoff: InitialResolveBackoff,
		MaxBackoff:     10 * time.Second,
		Multiplier:     2,
		Closer:         t.rpcContext.Stopper.ShouldQuiesce(),
	}
	for r := retry.Start(opts); r.Next(); {
		addr, err := t.resolver(nodeID)
		if err == nil {
			close(ch)
			if r.CurrentAttempt() > 0 {
				log.Infof(context.TODO(), "address resolution for %s succeeded: %s", nodeID, addr)
			}
			return addr, nil
		} else if r.CurrentAttempt() == 0 {
			log.Warningf(context.TODO(), "failing address resolution for %s: %s", nodeID, err)
		}
	}
	return nil, errors.Errorf("address resolution for %s stopped before completion", nodeID)
}

// processQueue creates a client and sends messages from its designated queue
// via that client, exiting when the client fails or when it idles out. All
// messages remaining in the queue at that point are lost and a new instance of
// processQueue should be started by the next message to be sent.
// TODO(tschottdorf) should let raft know if the node is down;
// need a feedback mechanism for that. Potentially easiest is to arrange for
// the next call to Send() to fail appropriately.
func (t *RaftTransport) processQueue(ch chan *RaftMessageRequest, toReplica roachpb.ReplicaDescriptor) error {
	addr, err := t.resolveNodeID(toReplica.NodeID)
	if err != nil {
		return err
	}

	conn, err := t.rpcContext.GRPCDial(addr.String())
	if err != nil {
		return err
	}
	client := NewMultiRaftClient(conn)

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	stream, err := client.RaftMessage(ctx)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)

	// Starting workers in a task prevents data races during shutdown.
	if err := t.rpcContext.Stopper.RunTask(func() {
		t.rpcContext.Stopper.RunWorker(func() {
			errCh <- stream.RecvMsg(new(RaftMessageResponse))
		})
	}); err != nil {
		return err
	}

	var raftIdleTimer timeutil.Timer
	defer raftIdleTimer.Stop()
	for {
		raftIdleTimer.Reset(raftIdleTimeout)
		select {
		case <-t.rpcContext.Stopper.ShouldStop():
			return nil
		case <-raftIdleTimer.C:
			raftIdleTimer.Read = true
			return nil
		case err := <-errCh:
			return err
		case req := <-ch:
			err := stream.Send(req)
			if req.Message.Type == raftpb.MsgSnap {
				select {
				case <-t.rpcContext.Stopper.ShouldStop():
					return nil
				case t.SnapshotStatusChan <- RaftSnapshotStatus{req, err}:
				}

			}
			if err != nil {
				return err
			}
		}
	}
}

type errHandler func(error, roachpb.ReplicaDescriptor)

// RaftSender is a wrapper around RaftTransport that provides an error
// handler.
type RaftSender struct {
	transport *RaftTransport
	onError   errHandler
}

// MakeSender constructs a RaftSender with the provided error handler.
func (t *RaftTransport) MakeSender(onError errHandler) RaftSender {
	return RaftSender{transport: t, onError: onError}
}

// SendAsync sends a message to the recipient specified in the request. It
// returns false if the outgoing queue is full and calls s.onError when the
// recipient closes the stream.
func (s RaftSender) SendAsync(req *RaftMessageRequest) bool {
	isHeartbeat := (req.Message.Type == raftpb.MsgHeartbeat ||
		req.Message.Type == raftpb.MsgHeartbeatResp)
	if req.RangeID == 0 && !isHeartbeat {
		// Coalesced heartbeats are addressed to range 0; everything else
		// needs an explicit range ID.
		panic("only heartbeat messages may be sent to range ID 0")
	}
	isSnap := req.Message.Type == raftpb.MsgSnap
	toReplica := req.ToReplica
	s.transport.mu.Lock()
	// We use two queues; one will be used for snapshots, the other for all other
	// traffic. This is done to prevent snapshots from blocking other traffic.
	queues, ok := s.transport.mu.queues[isSnap]
	if !ok {
		queues = make(map[roachpb.ReplicaDescriptor]chan *RaftMessageRequest)
		s.transport.mu.queues[isSnap] = queues
	}
	ch, ok := queues[toReplica]
	if !ok {
		ch = make(chan *RaftMessageRequest, raftSendBufferSize)
		queues[toReplica] = ch
	}
	s.transport.mu.Unlock()

	if !ok {
		// Starting workers in a task prevents data races during shutdown.
		if err := s.transport.rpcContext.Stopper.RunTask(func() {
			s.transport.rpcContext.Stopper.RunWorker(func() {
				s.onError(s.transport.processQueue(ch, toReplica), toReplica)

				s.transport.mu.Lock()
				delete(queues, toReplica)
				s.transport.mu.Unlock()
			})
		}); err != nil {
			s.onError(err, toReplica)
		}
	}

	select {
	case ch <- req:
		return true
	default:
		return false
	}
}
