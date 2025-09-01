/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package subscribe implements the gnmi.proto Subscribe service API.
package subscribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"github.com/openconfig/gnmi/cache"
	"github.com/openconfig/gnmi/coalesce"
	"github.com/openconfig/gnmi/ctree"
	"github.com/openconfig/gnmi/match"
	"github.com/openconfig/gnmi/path"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/openconfig/gnmi/proto/gnmi"
)

type aclStub struct{}

func (a *aclStub) Check(string) bool {
	return true
}

// RPCACL is the per RPC ACL interface
type RPCACL interface {
	Check(string) bool
}

// ACL is server ACL interface
type ACL interface {
	NewRPCACL(context.Context) (RPCACL, error)
	Check(string, string) bool
}

// options contains options for creating a Server.
type options struct {
	noDupReport bool
	timeout     time.Duration
	stats       *stats
	acl         ACL
	// Test override functions
	flowControlTest          func()
	clientStatsTest          func(int64, int64)
	updateSubsCountEnterTest func()
	updateSubsCountExitTest  func()
}

// Option defines the function prototype to set options for creating a Server.
type Option func(*options)

// WithTimeout returns an Option to specify how long a send can be pending
// before the RPC is closed.
func WithTimeout(t time.Duration) Option {
	return func(o *options) {
		o.timeout = t
	}
}

// WithFlowControlTest returns an Option to override a test function to
// simulate flow control.
func WithFlowControlTest(f func()) Option {
	return func(o *options) {
		o.flowControlTest = f
	}
}

// WithClientStatsTest test override function.
func WithClientStatsTest(f func(int64, int64)) Option {
	return func(o *options) {
		o.clientStatsTest = f
	}
}

// WithUpdateSubsCountEnterTest test override function.
func WithUpdateSubsCountEnterTest(f func()) Option {
	return func(o *options) {
		o.updateSubsCountEnterTest = f
	}
}

// WithUpdateSubsCountExitTest test override function.
func WithUpdateSubsCountExitTest(f func()) Option {
	return func(o *options) {
		o.updateSubsCountExitTest = f
	}
}

// WithStats returns an Option to enable statistics collection of client
// queries to the server.
func WithStats() Option {
	return func(o *options) {
		o.stats = newStats()
	}
}

// WithACL sets server ACL.
func WithACL(a ACL) Option {
	return func(o *options) {
		o.acl = a
	}
}

// WithoutDupReport returns an Option to disable reporting of duplicates in
// the responses to the clients. When duplicate reporting is disabled, there
// is no need to clone the Notification proto message for setting a non-zero
// field "duplicates" in a response sent to clients, which can potentially
// save CPU cycles.
func WithoutDupReport() Option {
	return func(o *options) {
		o.noDupReport = true
	}
}

// Server is the implementation of the gNMI Subcribe API.
type Server struct {
	pb.UnimplementedGNMIServer // Stub out all RPCs except Subscribe.

	c                 *cache.Cache // The cache queries are performed against.
	m                 *match.Match // Structure to match updates against active subscriptions.
	o                 options
	peerSessionStates map[string]string // Peer session states for each neighbor.
}

// NewServer instantiates server to handle client queries.  The cache should be
// already instantiated.
func NewServer(c *cache.Cache, opts ...Option) (*Server, error) {
	o := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.timeout == 0 {
		o.timeout = time.Minute
	}
	return &Server{c: c, m: match.New(), o: o, peerSessionStates: make(map[string]string)}, nil
}

// UpdateNotification uses paths in a pb.Notification n to match registered
// clients in m and pass value v to those clients.
// prefix is the prefix of n that should be used to match clients in m.
// Depending on the caller, the target may or may not be in prefix.
// v should be n itself or the container of n (e.g. a ctree.Leaf) depending
// on the caller.
func UpdateNotification(m *match.Match, v interface{}, n *pb.Notification, prefix []string) {
	var updated map[match.Client]struct{}
	if len(n.Update)+len(n.Delete) > 1 {
		updated = make(map[match.Client]struct{})
	}
	for _, u := range n.Update {
		m.UpdateOnce(v, append(prefix, path.ToStrings(u.Path, false)...), updated)
	}
	for _, d := range n.Delete {
		m.UpdateOnce(v, append(prefix, path.ToStrings(d, false)...), updated)
	}
}

// Update passes a streaming update to registered clients.
func (s *Server) Update(n *ctree.Leaf) {
	switch v := n.Value().(type) {
	case *pb.Notification:
		UpdateNotification(s.m, n, v, path.ToStrings(v.Prefix, true))
	default:
		log.Errorf("update is not a known type; type is %T", v)
	}
}

func (s *Server) updateTargetCounts(target string) func() {
	if s.o.stats == nil {
		return func() {}
	}
	st := s.o.stats.targetStats(target)
	atomic.AddInt64(&st.ActiveSubscriptionCount, 1)
	atomic.AddInt64(&st.SubscriptionCount, 1)
	return func() {
		atomic.AddInt64(&st.ActiveSubscriptionCount, -1)
		if s.o.updateSubsCountExitTest != nil {
			s.o.updateSubsCountExitTest()
		}
	}
}

func (s *Server) updateTypeCounts(typ string) func() {
	if s.o.stats == nil {
		return func() {}
	}
	st := s.o.stats.typeStats(typ)
	atomic.AddInt64(&st.ActiveSubscriptionCount, 1)
	atomic.AddInt64(&st.SubscriptionCount, 1)
	if s.o.updateSubsCountEnterTest != nil {
		s.o.updateSubsCountEnterTest()
	}
	return func() {
		atomic.AddInt64(&st.ActiveSubscriptionCount, -1)
	}
}

// addSubscription registers all subscriptions for this client for update matching.
func addSubscription(m *match.Match, s *pb.SubscriptionList, c *matchClient) (remove func()) {
	var removes []func()
	prefix := path.ToStrings(s.Prefix, true)
	for _, sub := range s.Subscription {
		p := sub.GetPath()
		if p == nil {
			continue
		}
		query := prefix
		if origin := p.GetOrigin(); s.Prefix.GetOrigin() == "" && origin != "" {
			query = append(prefix, origin)
		}
		query = append(query, path.ToStrings(p, false)...)
		removes = append(removes, m.AddQuery(query, c))
	}
	return func() {
		for _, remove := range removes {
			remove()
		}
	}
}

// Subscribe is the entry point for the external RPC request of the same name
// defined in gnmi.proto.
func (s *Server) Subscribe(stream pb.GNMI_SubscribeServer) error {
	log.Infof("~~~~~~Subscribe: %v~~~~~~", stream)
	c := streamClient{stream: stream, acl: &aclStub{}}
	var err error
	if s.o.acl != nil {
		a, err := s.o.acl.NewRPCACL(stream.Context())
		if err != nil {
			log.Errorf("NewRPCACL fails due to %v", err)
			return status.Error(codes.Unauthenticated, "no authentication/authorization for requested operation")
		}
		c.acl = a
	}
	c.sr, err = stream.Recv()

	switch {
	case err == io.EOF:
		return nil
	case err != nil:
		return err
	case c.sr.GetSubscribe() == nil:
		return status.Errorf(codes.InvalidArgument, "request must contain a subscription %#v", c.sr)
	case c.sr.GetSubscribe().GetPrefix() == nil:
		return status.Errorf(codes.InvalidArgument, "request must contain a prefix %#v", c.sr)
	case c.sr.GetSubscribe().GetPrefix().GetTarget() == "":
		return status.Error(codes.InvalidArgument, "missing target")
	}

	c.target = c.sr.GetSubscribe().GetPrefix().GetTarget()

	runGetPeerSessionStates := false
	for _, sub := range c.sr.GetSubscribe().Subscription {
		p := sub.GetPath()
		if p == nil {
			continue
		}
		pathElems := path.ToStrings(p, false)
		if len(pathElems) > 0 && pathElems[len(pathElems)-1] == "loc-rib" {
			runGetPeerSessionStates = true
			break
		}
	}
	if runGetPeerSessionStates {
		log.Infof("~~~~~~runGetPeerSessionStates: %v~~~~~~", c.sr)
		peerState, err := s.GetPeerSessionStates(&c, c.target)
		if err != nil {
			log.Errorf("Failed to get peer session states for target %v: %v", c.target, err)
		} else {
			s.peerSessionStates = peerState
			if len(peerState) > 0 {
				log.Infof("~~~~~~peerState: %v~~~~~~", peerState)
				var updates []*pb.Update
				for neighbor, state := range peerState {
					u := &pb.Update{
						Path: &pb.Path{
							Elem: []*pb.PathElem{
								{Name: "peer-session-state"},
								{Name: neighbor},
							},
						},
						Val: &pb.TypedValue{
							Value: &pb.TypedValue_StringVal{StringVal: state},
						},
					}
					updates = append(updates, u)
				}
				notification := &pb.Notification{
					Prefix: c.sr.GetSubscribe().GetPrefix(),
					Update: updates,
				}
				if err := stream.Send(&pb.SubscribeResponse{Response: &pb.SubscribeResponse_Update{Update: notification}}); err != nil {
					log.Errorf("Failed to send peer session state notification to client: %v", err)
					return err
				}
			}
		}
	}

	if !s.c.HasTarget(c.target) {
		return status.Errorf(codes.NotFound, "no such target: %q", c.target)
	}
	defer s.updateTargetCounts(c.target)()
	peer, _ := peer.FromContext(stream.Context())
	mode := c.sr.GetSubscribe().Mode
	if m := mode.String(); m != "" {
		defer s.updateTypeCounts(strings.ToLower(m))()
	}
	log.Infof("peer: %v target: %q subscription: %s", peer.Addr, c.target, c.sr)
	defer log.Infof("peer: %v target %q subscription: end: %q", peer.Addr, c.target, c.sr)

	c.queue = coalesce.NewQueue()
	defer c.queue.Close()

	// reject single device subscription if not allowed by ACL
	if c.target != "*" && !c.acl.Check(c.target) {
		return status.Errorf(codes.PermissionDenied, "not authorized for target %q", c.target)
	}
	// This error channel is buffered to accept errors from all goroutines spawned
	// for this RPC.  Only the first is ever read and returned causing the RPC to
	// terminate.
	errC := make(chan error, 3)
	c.errC = errC

	switch mode {
	case pb.SubscriptionList_ONCE:
		go func() {
			s.processSubscription(&c)
			c.queue.Close()
		}()
	case pb.SubscriptionList_POLL:
		go s.processPollingSubscription(&c)
	case pb.SubscriptionList_STREAM:
		if c.sr.GetSubscribe().GetUpdatesOnly() {
			c.queue.Insert(syncMarker{})
		}
		remove := addSubscription(s.m, c.sr.GetSubscribe(),
			&matchClient{acl: c.acl, q: c.queue})
		defer remove()
		if !c.sr.GetSubscribe().GetUpdatesOnly() {
			go s.processSubscription(&c)
		}
	default:
		return status.Errorf(codes.InvalidArgument, "Subscription mode %v not recognized", mode)
	}

	go s.sendStreamingResults(&c)

	return <-errC
}

type resp struct {
	stream pb.GNMI_SubscribeServer
	n      *ctree.Leaf
	dup    uint32
	t      *time.Timer // Timer used to timout the subscription.
}

// sendSubscribeResponse populates and sends a single response returned on
// the Subscription RPC output stream. Streaming queries send responses for the
// initial walk of the results as well as streamed updates and use a queue to
// ensure order.
func (s *Server) sendSubscribeResponse(r *resp, c *streamClient) error {
	notification, err := s.MakeSubscribeResponse(r.n.Value(), r.dup)
	if err != nil {
		return status.Errorf(codes.Unknown, err.Error())
	}

	if pre := notification.GetUpdate().GetPrefix(); pre != nil {
		if !c.acl.Check(pre.GetTarget()) {
			// reaching here means notification is denied for sending.
			// return with no error. function caller can continue for next one.
			return nil
		}
	}

	// Start the timeout before attempting to send.
	r.t.Reset(s.o.timeout)
	// Clear the timeout upon sending.
	defer r.t.Stop()
	// An empty function in production, replaced in test to simulate flow control
	// by blocking before send.
	if s.o.flowControlTest != nil {
		s.o.flowControlTest()
	}
	return r.stream.Send(notification)
}

// subscribeSync is a response indicating that a Subscribe RPC has successfully
// returned all matching nodes once for ONCE and POLLING queries and at least
// once for STREAMING queries.
var subscribeSync = &pb.SubscribeResponse{Response: &pb.SubscribeResponse_SyncResponse{true}}

type syncMarker struct{}

// cacheClient implements match.Client interface.
type matchClient struct {
	acl RPCACL
	q   *coalesce.Queue
	err error
}

// Update implements the match.Client Update interface for coalesce.Queue.
func (c matchClient) Update(n interface{}) {
	// Stop processing updates on error.
	if c.err != nil {
		return
	}
	_, c.err = c.q.Insert(n)
}

type streamClient struct {
	acl    RPCACL
	target string
	sr     *pb.SubscribeRequest
	queue  *coalesce.Queue
	stream pb.GNMI_SubscribeServer
	errC   chan<- error
}

// processSubscription walks the cache tree and inserts all of the matching
// nodes into the coalesce queue followed by a subscriptionSync response.
func (s *Server) processSubscription(c *streamClient) {
	var err error
	log.V(2).Infof("start processSubscription for %p", c)
	// Close the cache client queue on error.
	defer func() {
		if err != nil {
			log.Error(err)
			c.errC <- err
		}
		log.V(2).Infof("end processSubscription for %p", c)
	}()
	if len(s.peerSessionStates) > 0 {
		log.Infof("~~~~~~Peer session states in processSubscription: %v~~~~~~", s.peerSessionStates)

		for neighborAddr := range s.peerSessionStates {
			fullPath := &pb.Path{
				Elem: []*pb.PathElem{
					{Name: "openconfig"},
					{Name: "network-instances"},
					{Name: "network-instance", Key: map[string]string{"name": "DEFAULT"}},
					{Name: "protocols"},
					{Name: "protocol", Key: map[string]string{"identifier": "BGP", "name": "BGP"}},
					{Name: "bgp"},
					{Name: "rib"},
					{Name: "afi-safis"},
					{Name: "afi-safi", Key: map[string]string{"afi-safi-name": "IPV4_UNICAST"}},
					{Name: "ipv4-unicast"},
					{Name: "neighbors"},
					{Name: "neighbor", Key: map[string]string{"neighbor-address": neighborAddr}},
					{Name: "adj-rib-in-pre"},
					{Name: "routes"},
					{Name: "route", Key: map[string]string{"prefix": "*", "path-id": "*"}},
					{Name: "state"},
				},
			}
			fullPathStr := path.ToStrings(fullPath, false)

			log.Infof("Querying adj-rib-in-pre for neighbor %s on target %s", neighborAddr, c.target)
			err = s.c.Query(c.target, fullPathStr, func(p []string, l *ctree.Leaf, _ any) error {
				if err != nil {
					return err
				}
				log.Infof("~~~~~~adj-rib-in-pre response for target %v, neighbor %s: path: %q, leaf: %v~~~~~~", c.target, neighborAddr, p, l)
				log.Infof("~~~~~~adj-rib-in-pre leaf value: %v~~~~~~", l.Value())
				_, err = c.queue.Insert(l)
				return err
			})
			if err != nil {
				log.Errorf("Error querying adj-rib-in-pre for neighbor %s on target %s: %v", neighborAddr, c.target, err)
				// Continue to the next neighbor if one query fails
			}
			// Send End-Of-RIB marker here.
			eorPath := &pb.Path{
				Elem: []*pb.PathElem{
					{Name: "openconfig"},
					{Name: "network-instances"},
					{Name: "network-instance", Key: map[string]string{"name": "DEFAULT"}},
					{Name: "protocols"},
					{Name: "protocol", Key: map[string]string{"identifier": "BGP", "name": "BGP"}},
					{Name: "bgp"},
					{Name: "rib"},
					{Name: "afi-safis"},
					{Name: "afi-safi", Key: map[string]string{"afi-safi-name": "IPV4_UNICAST"}},
					{Name: "ipv4-unicast"},
					{Name: "neighbors"},
					{Name: "neighbor", Key: map[string]string{"neighbor-address": neighborAddr}},
					{Name: "adj-rib-in-pre"},
					{Name: "routes"},
					{Name: "route", Key: map[string]string{"prefix": "0.0.0.0/0", "path-id": "4294967295"}},
					{Name: "state"},
				},
			}
			eorUpdate := &pb.Update{
				Path: eorPath,
				Val: &pb.TypedValue{
					Value: &pb.TypedValue_JsonVal{JsonVal: []byte(`{"valid-route": false}`)}, // Indicate EOR
				},
			}
			eorNotification := &pb.Notification{
				Prefix: c.sr.GetSubscribe().GetPrefix(),
				Update: []*pb.Update{eorUpdate},
			}
			_, err = c.queue.Insert(ctree.DetachedLeaf(eorNotification))
			if err != nil {
				log.Errorf("Error inserting End-of-RIB marker for neighbor %s on target %s: %v", neighborAddr, c.target, err)
			} else {
				log.Infof("~~~~~~Sent End-of-RIB marker for neighbor %s on target %s", neighborAddr, c.target)
			}
		}
	}
	if !c.sr.GetSubscribe().GetUpdatesOnly() {
		for _, subscription := range c.sr.GetSubscribe().Subscription {
			var fullPath []string
			fullPath, err = path.CompletePath(c.sr.GetSubscribe().GetPrefix(), subscription.GetPath())
			if err != nil {
				return
			}
			log.Infof("~~~~~~fullPath: %q~~~~~~", fullPath)
			// Note that fullPath doesn't contain target name as the first element.
			s.c.Query(c.target, fullPath, func(p []string, l *ctree.Leaf, _ interface{}) error {
				// Stop processing query results on error.
				log.Infof("~~~~~~full responses for target %v: path: %q, leaf: %v~~~~~~", c.target, p, l)
				if err != nil {
					return err
				}
				_, err = c.queue.Insert(l)
				return nil
			})
			if err != nil {
				return
			}
		}
	}

	_, err = c.queue.Insert(syncMarker{})
}

// processPollingSubscription handles the POLL mode Subscription RPC.
func (s *Server) processPollingSubscription(c *streamClient) {
	s.processSubscription(c)
	log.Infof("polling subscription: first complete response: %q", c.sr)
	for {
		if c.queue.IsClosed() {
			log.Info("Terminating polling subscription due to closed client queue.")
			c.errC <- nil
			return
		}
		// Subsequent receives are only triggers to poll again. The contents of the
		// request are completely ignored.
		_, err := c.stream.Recv()
		if err == io.EOF {
			log.Info("Terminating polling subscription due to EOF.")
			c.errC <- nil
			return
		}
		if err != nil {
			log.Error(err)
			c.errC <- err
			return
		}
		log.Infof("polling subscription: repoll: %q", c.sr)
		s.processSubscription(c)
		log.Infof("polling subscription: repoll complete: %q", c.sr)
	}
}

func (s *Server) updateClientStats(client, target string, dup, queueSize int64) {
	if s.o.stats == nil {
		return
	}
	st := s.o.stats.clientStats(client, target)
	atomic.AddInt64(&st.CoalesceCount, dup)
	atomic.StoreInt64(&st.QueueSize, queueSize)
	if s.o.clientStatsTest != nil {
		s.o.clientStatsTest(dup, queueSize)
	}
}

// sendStreamingResults forwards all streaming updates to a given streaming
// Subscription RPC client.
func (s *Server) sendStreamingResults(c *streamClient) {
	ctx := c.stream.Context()
	peer, _ := peer.FromContext(ctx)
	// The pointer is used only to disambiguate among multiple subscriptions from the
	// same Peer and has no meaning otherwise.
	szKey := fmt.Sprintf("%s:%p", peer.Addr, c.sr)
	defer func() {
		if s.o.stats != nil {
			s.o.stats.removeClientStats(szKey)
		}
	}()

	t := time.NewTimer(s.o.timeout)
	// Make sure the timer doesn't expire waiting for a value to send, only
	// waiting to send.
	t.Stop()
	done := make(chan struct{})
	defer close(done)
	// If a send doesn't occur within the timeout, close the stream.
	go func() {
		select {
		case <-t.C:
			err := errors.New("subscription timed out while sending")
			c.errC <- err
			log.Errorf("%v : %v", peer, err)
		case <-done:
		}
	}()
	for {
		item, dup, err := c.queue.Next(ctx)
		if coalesce.IsClosedQueue(err) {
			c.errC <- nil
			return
		}
		if err != nil {
			c.errC <- err
			return
		}
		s.updateClientStats(szKey, c.target, int64(dup), int64(c.queue.Len()))

		// s.processSubscription will send a sync marker, handle it separately.
		if _, ok := item.(syncMarker); ok {
			if err = c.stream.Send(subscribeSync); err != nil {
				c.errC <- err
				return
			}
			continue
		}

		n, ok := item.(*ctree.Leaf)
		if !ok || n == nil {
			c.errC <- status.Errorf(codes.Internal, "invalid cache node: %#v", item)
			return
		}

		if err = s.sendSubscribeResponse(&resp{
			stream: c.stream,
			n:      n,
			dup:    dup,
			t:      t,
		}, c); err != nil {
			c.errC <- err
			return
		}
		// If the only target being subscribed was deleted, stop streaming.
		if isTargetDelete(n) && c.target != "*" {
			log.Infof("Target %q was deleted. Closing stream.", c.target)
			c.errC <- nil
			return
		}
	}
}

// MakeSubscribeResponse produces a gnmi_proto.SubscribeResponse from a
// gnmi_proto.Notification.
//
// This function modifies the message to set the duplicate count if it is
// greater than 0. The function clones the gnmi notification if the duplicate count needs to be set.
// You have to be working on a cloned message if you need to modify the message in any way.
func (s *Server) MakeSubscribeResponse(n interface{}, dup uint32) (*pb.SubscribeResponse, error) {
	var notification *pb.Notification
	var ok bool
	notification, ok = n.(*pb.Notification)
	if !ok {
		return nil, status.Errorf(codes.Internal, "invalid notification type: %#v", n)
	}

	// There may be multiple updates in a notification. Since duplicate count is just
	// an indicator that coalescion is happening, not a critical data, just the first
	// update is set with duplicate count to be on the side of efficiency.
	// Only attempt to set the duplicate count if it is greater than 0. The default
	// value in the message is already 0.
	if !s.o.noDupReport && dup > 0 && len(notification.Update) > 0 {
		// We need a copy of the cached notification before writing a client specific
		// duplicate count as the notification is shared across all clients.
		notification = proto.Clone(notification).(*pb.Notification)
		notification.Update[0].Duplicates = dup
	}
	response := &pb.SubscribeResponse{
		Response: &pb.SubscribeResponse_Update{
			Update: notification,
		},
	}

	return response, nil
}

func isTargetDelete(l *ctree.Leaf) bool {
	switch v := l.Value().(type) {
	case *pb.Notification:
		if len(v.Delete) == 1 {
			var orig string
			if v.Prefix != nil {
				orig = v.Prefix.Origin
			}
			// Prefix path is indexed without target and origin
			p := path.ToStrings(v.Prefix, false)
			p = append(p, path.ToStrings(v.Delete[0], false)...)
			// When origin isn't set, intention must be to delete entire target.
			return orig == "" && len(p) == 1 && p[0] == "*"
		}
	}
	return false
}

// TypeStats returns statistics for all types of subscribe queries, e.g.
// stream, once, or poll. Statistics is available only if the Server is
// created with NewServerWithStats.
func (s *Server) TypeStats() map[string]TypeStats {
	if s.o.stats == nil {
		return nil
	}
	return s.o.stats.allTypeStats()
}

// TargetStats returns statistics of subscribe queries for all targets.
// Statistics is available only if the Server is created with
// NewServerWithStats.
func (s *Server) TargetStats() map[string]TargetStats {
	if s.o.stats == nil {
		return nil
	}
	return s.o.stats.allTargetStats()
}

// ClientStats returns states of all subscribe clients such as queue size,
// coalesce count. Statistics is available only if the Server is created
// with NewServerWithStats.
func (s *Server) ClientStats() map[string]ClientStats {
	if s.o.stats == nil {
		return nil
	}
	return s.o.stats.allClientStats()
}

// GetPeerSessionStates: Dummy comment here
func (s *Server) GetPeerSessionStates(c *streamClient, target string) (map[string]string, error) {
	peerStates := make(map[string]string)
	if !s.c.HasTarget(target) {
		return nil, fmt.Errorf("target %s not found in cache", target)
	}

	// Construct the path using pb.Path
	fullPath := &pb.Path{
		Elem: []*pb.PathElem{
			{Name: "openconfig"},
			{Name: "network-instances"},
			{Name: "network-instance", Key: map[string]string{"name": "DEFAULT"}},
			{Name: "protocols"},
			{Name: "protocol", Key: map[string]string{"identifier": "BGP", "name": "BGP"}},
			{Name: "bgp"},
			{Name: "neighbors"},
			{Name: "neighbor", Key: map[string]string{"neighbor-address": "*"}},
			{Name: "state"},
			{Name: "session-state"},
		},
	}
	fullPathStr := path.ToStrings(fullPath, false)

	err := s.c.Query(target, fullPathStr, func(p []string, l *ctree.Leaf, _ any) error {
		if l == nil {
			log.Warningf("Received nil leaf at path %v for target %s", p, target)
			return nil
		}
		notification, ok := l.Value().(*pb.Notification)
		if !ok {
			log.Errorf("Unexpected type %T at path %v for target %s", l.Value(), p, target)
			return fmt.Errorf("unexpected type %T at path %v", l.Value(), p)
		}
		for _, update := range notification.GetUpdate() {
			sessionState := update.GetVal().GetStringVal()
			if sessionState == "" {
				continue
			}

			var neighborAddr string
			for _, elem := range update.GetPath().GetElem() {
				if elem.GetName() == "neighbor" {
					if addr, ok := elem.GetKey()["neighbor-address"]; ok {
						neighborAddr = addr
						break
					}
				}
			}
			if neighborAddr == "" {
				log.Warningf("Could not find neighbor-address in update path for target %s: %v", target, update.GetPath())
				continue
			}
			peerStates[neighborAddr] = sessionState
			log.Infof("Found session state for target %s, neighbor %s: %s", target, neighborAddr, sessionState)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query peer session states for target %s: %v", target, err)
	}

	if len(peerStates) == 0 {
		log.Warningf("No peer session states found for target %s with path %v", target, fullPathStr)
	}
	return peerStates, nil
}
