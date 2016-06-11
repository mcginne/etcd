/*
 *
 * Copyright 2016, Google Inc.
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *     * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *     * Redistributions in binary form must reproduce the above
 * copyright notice, this list of conditions and the following disclaimer
 * in the documentation and/or other materials provided with the
 * distribution.
 *     * Neither the name of Google Inc. nor the names of its
 * contributors may be used to endorse or promote products derived from
 * this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 * "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 * LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 * A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 * OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 * DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 * THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 *
 */

package grpc

import (
	"fmt"
	"sync"

	"golang.org/x/net/context"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/naming"
	"google.golang.org/grpc/transport"
)

// Address represents a server the client connects to.
// This is the EXPERIMENTAL API and may be changed or extended in the future.
type Address struct {
	// Addr is the server address on which a connection will be established.
	Addr string
	// Metadata is the information associated with Addr, which may be used
	// to make load balancing decision.
	Metadata interface{}
}

// BalancerGetOptions configures a Get call.
// This is the EXPERIMENTAL API and may be changed or extended in the future.
type BalancerGetOptions struct {
	// BlockingWait specifies whether Get should block when there is no
	// connected address.
	BlockingWait bool
}

// Balancer chooses network addresses for RPCs.
// This is the EXPERIMENTAL API and may be changed or extended in the future.
type Balancer interface {
	// Start does the initialization work to bootstrap a Balancer. For example,
	// this function may start the name resolution and watch the updates. It will
	// be called when dialing.
	Start(target string) error
	// Up informs the Balancer that gRPC has a connection to the server at
	// addr. It returns down which is called once the connection to addr gets
	// lost or closed.
	// TODO: It is not clear how to construct and take advantage the meaningful error
	// parameter for down. Need realistic demands to guide.
	Up(addr Address) (down func(error))
	// Get gets the address of a server for the RPC corresponding to ctx.
	// i) If it returns a connected address, gRPC internals issues the RPC on the
	// connection to this address;
	// ii) If it returns an address on which the connection is under construction
	// (initiated by Notify(...)) but not connected, gRPC internals
	//  * fails RPC if the RPC is fail-fast and connection is in the TransientFailure or
	//  Shutdown state;
	//  or
	//  * issues RPC on the connection otherwise.
	// iii) If it returns an address on which the connection does not exist, gRPC
	// internals treats it as an error and will fail the corresponding RPC.
	//
	// Therefore, the following is the recommended rule when writing a custom Balancer.
	// If opts.BlockingWait is true, it should return a connected address or
	// block if there is no connected address. It should respect the timeout or
	// cancellation of ctx when blocking. If opts.BlockingWait is false (for fail-fast
	// RPCs), it should return an address it has notified via Notify(...) immediately
	// instead of blocking.
	//
	// The function returns put which is called once the rpc has completed or failed.
	// put can collect and report RPC stats to a remote load balancer. gRPC internals
	// will try to call this again if err is non-nil (unless err is ErrClientConnClosing).
	//
	// TODO: Add other non-recoverable errors?
	Get(ctx context.Context, opts BalancerGetOptions) (addr Address, put func(), err error)
	// Notify returns a channel that is used by gRPC internals to watch the addresses
	// gRPC needs to connect. The addresses might be from a name resolver or remote
	// load balancer. gRPC internals will compare it with the existing connected
	// addresses. If the address Balancer notified is not in the existing connected
	// addresses, gRPC starts to connect the address. If an address in the existing
	// connected addresses is not in the notification list, the corresponding connection
	// is shutdown gracefully. Otherwise, there are no operations to take. Note that
	// the Address slice must be the full list of the Addresses which should be connected.
	// It is NOT delta.
	Notify() <-chan []Address
	// Close shuts down the balancer.
	Close() error
}

// downErr implements net.Error. It is constructed by gRPC internals and passed to the down
// call of Balancer.
type downErr struct {
	timeout   bool
	temporary bool
	desc      string
}

func (e downErr) Error() string   { return e.desc }
func (e downErr) Timeout() bool   { return e.timeout }
func (e downErr) Temporary() bool { return e.temporary }

func downErrorf(timeout, temporary bool, format string, a ...interface{}) downErr {
	return downErr{
		timeout:   timeout,
		temporary: temporary,
		desc:      fmt.Sprintf(format, a...),
	}
}

// RoundRobin returns a Balancer that selects addresses round-robin. It uses r to watch
// the name resolution updates and updates the addresses available correspondingly.
func RoundRobin(r naming.Resolver) Balancer {
	return &roundRobin{r: r}
}

type roundRobin struct {
	r         naming.Resolver
	w         naming.Watcher
	open      []Address // all the addresses the client should potentially connect
	mu        sync.Mutex
	addrCh    chan []Address // the channel to notify gRPC internals the list of addresses the client should connect to.
	connected []Address      // all the connected addresses
	next      int            // index of the next address to return for Get()
	waitCh    chan struct{}  // the channel to block when there is no connected address available
	done      bool           // The Balancer is closed.
}

func (rr *roundRobin) watchAddrUpdates() error {
	updates, err := rr.w.Next()
	if err != nil {
		grpclog.Println("grpc: the naming watcher stops working due to %v.", err)
		return err
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	for _, update := range updates {
		addr := Address{
			Addr: update.Addr,
		}
		switch update.Op {
		case naming.Add:
			var exist bool
			for _, v := range rr.open {
				if addr == v {
					exist = true
					grpclog.Println("grpc: The name resolver wanted to add an existing address: ", addr)
					break
				}
			}
			if exist {
				continue
			}
			rr.open = append(rr.open, addr)
		case naming.Delete:
			for i, v := range rr.open {
				if v == addr {
					copy(rr.open[i:], rr.open[i+1:])
					rr.open = rr.open[:len(rr.open)-1]
					break
				}
			}
		default:
			grpclog.Println("Unknown update.Op ", update.Op)
		}
	}
	// Make a copy of rr.open and write it onto rr.addrCh so that gRPC internals gets notified.
	open := make([]Address, len(rr.open), len(rr.open))
	copy(open, rr.open)
	if rr.done {
		return ErrClientConnClosing
	}
	rr.addrCh <- open
	return nil
}

func (rr *roundRobin) Start(target string) error {
	if rr.r == nil {
		// If there is no name resolver installed, it is not needed to
		// do name resolution. In this case, rr.addrCh stays nil.
		return nil
	}
	w, err := rr.r.Resolve(target)
	if err != nil {
		return err
	}
	rr.w = w
	rr.addrCh = make(chan []Address)
	go func() {
		for {
			if err := rr.watchAddrUpdates(); err != nil {
				return
			}
		}
	}()
	return nil
}

// Up appends addr to the end of rr.connected and sends notification if there
// are pending Get() calls.
func (rr *roundRobin) Up(addr Address) func(error) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	for _, a := range rr.connected {
		if a == addr {
			return nil
		}
	}
	rr.connected = append(rr.connected, addr)
	if len(rr.connected) == 1 {
		// addr is only one available. Notify the Get() callers who are blocking.
		if rr.waitCh != nil {
			close(rr.waitCh)
			rr.waitCh = nil
		}
	}
	return func(err error) {
		rr.down(addr, err)
	}
}

// down removes addr from rr.connected and moves the remaining addrs forward.
func (rr *roundRobin) down(addr Address, err error) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	for i, a := range rr.connected {
		if a == addr {
			copy(rr.connected[i:], rr.connected[i+1:])
			rr.connected = rr.connected[:len(rr.connected)-1]
			return
		}
	}
}

// Get returns the next addr in the rotation.
func (rr *roundRobin) Get(ctx context.Context, opts BalancerGetOptions) (addr Address, put func(), err error) {
	var ch chan struct{}
	rr.mu.Lock()
	if rr.done {
		rr.mu.Unlock()
		err = ErrClientConnClosing
		return
	}
	if rr.next >= len(rr.connected) {
		rr.next = 0
	}
	if len(rr.connected) > 0 {
		addr = rr.connected[rr.next]
		rr.next++
		rr.mu.Unlock()
		return
	}
	// There is no address available. Wait on rr.waitCh.
	// TODO(zhaoq): Handle the case when opts.BlockingWait is false.
	if rr.waitCh == nil {
		ch = make(chan struct{})
		rr.waitCh = ch
	} else {
		ch = rr.waitCh
	}
	rr.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			err = transport.ContextErr(ctx.Err())
			return
		case <-ch:
			rr.mu.Lock()
			if rr.done {
				rr.mu.Unlock()
				err = ErrClientConnClosing
				return
			}
			if len(rr.connected) == 0 {
				// The newly added addr got removed by Down() again.
				if rr.waitCh == nil {
					ch = make(chan struct{})
					rr.waitCh = ch
				} else {
					ch = rr.waitCh
				}
				rr.mu.Unlock()
				continue
			}
			if rr.next >= len(rr.connected) {
				rr.next = 0
			}
			addr = rr.connected[rr.next]
			rr.next++
			rr.mu.Unlock()
			return
		}
	}
}

func (rr *roundRobin) Notify() <-chan []Address {
	return rr.addrCh
}

func (rr *roundRobin) Close() error {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.done = true
	if rr.w != nil {
		rr.w.Close()
	}
	if rr.waitCh != nil {
		close(rr.waitCh)
		rr.waitCh = nil
	}
	if rr.addrCh != nil {
		close(rr.addrCh)
	}
	return nil
}