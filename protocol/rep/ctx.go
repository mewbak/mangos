// Copyright 2018 The Mangos Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package rep implements the REP protocol, which is the response side of
// the request/response pattern.  (REQ is the request.)
package rep

import (
	"sync"
	"time"

	"nanomsg.org/go/mangos/v2"
	"nanomsg.org/go/mangos/v2/impl"
)

type pipe struct {
	s      *socket
	p      mangos.Endpoint
	closed bool
	sendQ  chan *mangos.Message
	closeQ chan struct{}
}

type socket struct {
	sock     mangos.ProtocolSocket
	closed   bool
	pipes    map[uint32]*pipe
	ttl      int
	sendQLen int
	recvCond *sync.Cond
	recvCtxs map[*context]struct{}
	ctxs     map[*context]struct{}
	defCtx   *context
	sync.Mutex
}

type context struct {
	s          *socket
	closed     bool
	recvWait   bool
	recvExpire time.Duration
	recvPipe   *pipe
	recvQ      chan *mangos.Message
	closeQ     chan struct{}

	sendExpire time.Duration
	sendWait   bool
	sendMsg    *mangos.Message

	bestEffort bool
	backtrace  []byte
	repMsg     *mangos.Message
	pipeID     uint32 // using ID keeps GC from holding the pipe

	cond *sync.Cond
}

// closedQ represents a nonblocking time channel.
var closedQ <-chan time.Time

// nilQ represents a nil time channel (blocks forever)
var nilQ <-chan time.Time

func init() {
	tq := make(chan time.Time)
	closedQ = tq
	close(tq)
}

func (c *context) RecvMsg() (*mangos.Message, error) {
	s := c.s
	s.Lock()

	if c.closed {
		s.Unlock()
		return nil, mangos.ErrClosed
	}
	if c.recvWait {
		s.Unlock()
		return nil, mangos.ErrProtoState
	}
	c.recvWait = true

	cq := c.closeQ
	wq := nilQ
	exptime := c.recvExpire

	s.recvCtxs[c] = struct{}{}
	s.recvCond.Signal()
	s.Unlock()

	if exptime > 0 {
		wq = time.After(exptime * 10)
	}

	var err error
	var m *mangos.Message

	select {
	case m = <-c.recvQ:
		err = nil
	case <-wq:
		err = mangos.ErrRecvTimeout
	case <-cq:
		err = mangos.ErrClosed
	}

	s.Lock()
	delete(s.recvCtxs, c)

	select {
	case m = <-c.recvQ:
		err = nil
	default:
	}
	if m != nil {
		c.backtrace = m.Header
		m.Header = nil
	}
	c.recvWait = false
	s.Unlock()
	return m, err
}

func (c *context) SendMsg(m *mangos.Message) error {
	r := c.s
	r.Lock()

	if r.closed || c.closed {
		r.Unlock()
		return mangos.ErrClosed
	}
	if c.backtrace == nil {
		r.Unlock()
		return mangos.ErrProtoState
	}
	p := c.recvPipe
	c.recvPipe = nil

	bestEffort := c.bestEffort
	wq := nilQ
	if bestEffort {
		wq = closedQ
	} else if c.sendExpire > 0 {
		wq = time.After(c.sendExpire)
	}

	m.Header = c.backtrace
	c.backtrace = nil
	cq := c.closeQ
	r.Unlock()

	select {
	case <-cq:
		m.Header = nil
		return mangos.ErrClosed
	case <-p.closeQ:
		// Pipe closed, so no way to get it to the recipient.
		// Just discard the message.
		m.Free()
		return nil
	case <-wq:
		if bestEffort {
			// No way to report to caller, so just discard
			// the message.
			m.Free()
			return nil
		}
		m.Header = nil
		return mangos.ErrSendTimeout

	case p.sendQ <- m:
		return nil
	}
}

func (c *context) Close() error {
	s := c.s
	s.Lock()
	if c.closed {
		s.Unlock()
		return mangos.ErrClosed
	}
	delete(s.recvCtxs, c)
	delete(s.ctxs, c)
	c.closed = true
	close(c.closeQ)
	s.Unlock()
	return nil
}

func (c *context) GetOption(name string) (interface{}, error) {
	switch name {
	case mangos.OptionBestEffort:
		c.s.Lock()
		v := c.bestEffort
		c.s.Unlock()
		return v, nil

	case mangos.OptionRecvDeadline:
		c.s.Lock()
		v := c.recvExpire
		c.s.Unlock()
		return v, nil

	case mangos.OptionSendDeadline:
		c.s.Lock()
		v := c.sendExpire
		c.s.Unlock()
		return v, nil

	default:
		return nil, mangos.ErrBadOption
	}
}

func (c *context) SetOption(name string, v interface{}) error {
	switch name {
	case mangos.OptionSendDeadline:
		if val, ok := v.(time.Duration); ok && val.Nanoseconds() > 0 {
			c.s.Lock()
			c.sendExpire = val
			c.s.Unlock()
			return nil
		}
		return mangos.ErrBadValue
	case mangos.OptionRecvDeadline:
		if val, ok := v.(time.Duration); ok && val.Nanoseconds() > 0 {
			c.s.Lock()
			c.recvExpire = val
			c.s.Unlock()
			return nil
		}
		return mangos.ErrBadValue

	default:
		return mangos.ErrBadOption
	}
}

func (p *pipe) receiver() {
	s := p.s
getmsg:
	for {
		m := p.p.RecvMsg()
		if m == nil {
			break
		}

		// Move backtrace from body to header.
		hops := 0
		for {
			if hops >= s.ttl {
				m.Free() // ErrTooManyHops
				continue getmsg
			}
			hops++
			if len(m.Body) < 4 {
				m.Free() // ErrGarbled
				continue getmsg
			}
			m.Header = append(m.Header, m.Body[:4]...)
			m.Body = m.Body[4:]
			// Check for high order bit set (0x80000000, big endian)
			if m.Header[len(m.Header)-4]&0x80 != 0 {
				break
			}
		}

		s.Lock()
		for len(s.recvCtxs) == 0 && !s.closed && !p.closed {
			s.recvCond.Wait()
		}
		if s.closed || p.closed {
			s.Unlock()
			m.Free()
			break
		}

		for c := range s.recvCtxs {
			delete(s.recvCtxs, c)
			c.recvPipe = p
			select {
			case c.recvQ <- m:
			default:
				m.Free()
			}
			// We *only* want to do this loop once, as we just
			// want to use a random element of recvCtxs.
			break
		}
		s.Unlock()
	}
	go p.close()
}

func (p *pipe) sender() {
	for {
		select {
		case m := <-p.sendQ:
			if p.p.SendMsg(m) != nil {
				p.close()
				return
			}
		case <-p.closeQ:
			return
		}
	}
}

func (p *pipe) close() {
	// Avoid double close
	p.s.Lock()
	if !p.closed {
		p.closed = true
		p.p.Close()
		close(p.closeQ)
	}
	p.s.Unlock()
}

func (s *socket) Close() error {

	s.Lock()
	defer s.Unlock()

	if s.closed {
		return mangos.ErrClosed
	}
	s.closed = true
	for c := range s.ctxs {
		go c.Close()
	}
	// close and remove each and every pipe
	for _, p := range s.pipes {
		go p.close()
	}
	return nil
}

func (*socket) Info() mangos.ProtocolInfo {
	return mangos.ProtocolInfo{
		Self:     mangos.ProtoRep,
		Peer:     mangos.ProtoReq,
		SelfName: "rep",
		PeerName: "req",
	}
}

func (s *socket) AddPipe(ep mangos.Endpoint) error {

	s.Lock()
	p := &pipe{
		p:      ep,
		s:      s,
		sendQ:  make(chan *mangos.Message, s.sendQLen),
		closeQ: make(chan struct{}),
	}
	if s.closed {
		s.Unlock()
		return mangos.ErrClosed
	}
	s.pipes[ep.GetID()] = p
	go p.sender()
	go p.receiver()
	s.Unlock()
	return nil
}

func (s *socket) RemovePipe(ep mangos.Endpoint) {

	s.Lock()
	if p, ok := s.pipes[ep.GetID()]; ok {
		delete(s.pipes, ep.GetID())
		go p.close()
	}
	s.Unlock()
}

func (s *socket) SetOption(name string, v interface{}) error {
	switch name {
	case mangos.OptionWriteQLen:
		if qlen, ok := v.(int); ok && qlen > 0 {
			s.Lock()
			s.sendQLen = qlen
			s.Unlock()
			return nil
		}
		return mangos.ErrBadValue
	case mangos.OptionTTL:
		if ttl, ok := v.(int); ok && ttl > 0 && ttl < 256 {
			s.Lock()
			s.ttl = ttl
			s.Unlock()
			return nil
		}
		return mangos.ErrBadValue
	}
	return s.defCtx.SetOption(name, v)
}

func (s *socket) GetOption(name string) (interface{}, error) {
	switch name {
	case mangos.OptionRaw:
		return false, nil
	case mangos.OptionTTL:
		s.Lock()
		v := s.ttl
		s.Unlock()
		return v, nil
	case mangos.OptionWriteQLen:
		s.Lock()
		v := s.sendQLen
		s.Unlock()
		return v, nil
	}

	return s.defCtx.GetOption(name)
}

func (s *socket) OpenContext() (mangos.ProtocolContext, error) {
	s.Lock()
	defer s.Unlock()
	if s.closed {
		return nil, mangos.ErrClosed
	}
	c := &context{
		s:      s,
		closeQ: make(chan struct{}),
		recvQ:  make(chan *mangos.Message, 1),
	}
	return c, nil
}

func (s *socket) RecvMsg() (*mangos.Message, error) {
	return s.defCtx.RecvMsg()
}

func (s *socket) SendMsg(m *mangos.Message) error {
	return s.defCtx.SendMsg(m)
}

// NewSocket allocates a new Socket using the REP protocol.
func NewSocket() (mangos.Socket, error) {
	s := &socket{
		ttl:      8,
		pipes:    make(map[uint32]*pipe),
		ctxs:     make(map[*context]struct{}),
		recvCtxs: make(map[*context]struct{}),
		defCtx: &context{
			closeQ: make(chan struct{}),
			recvQ:  make(chan *mangos.Message, 1),
		},
	}
	s.defCtx.s = s
	s.recvCond = sync.NewCond(s)
	s.ctxs[s.defCtx] = struct{}{}
	return impl.MakeSocket(s), nil
}
