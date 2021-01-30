// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package network

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stretchr/testify/assert"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/networking/router"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/hashing"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
)

const (
	defaultSendQueueSize       = 1 << 10
	defaultAliasReleaseFreq    = 500 * time.Millisecond
	defaultAliasReleaseTimeout = 10 * time.Millisecond
)

var (
	errClosed  = errors.New("closed")
	errRefused = errors.New("connection refused")
)

type testListener struct {
	addr    net.Addr
	inbound chan net.Conn
	once    sync.Once
	closed  chan struct{}
}

func (l *testListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.inbound:
		return c, nil
	case <-l.closed:
		return nil, errClosed
	}
}

func (l *testListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *testListener) Addr() net.Addr { return l.addr }

type testDialer struct {
	addr      net.Addr
	outbounds map[string]*testListener
	closer    func(net.Addr, net.Addr)
}

func (d *testDialer) Dial(ip utils.IPDesc) (net.Conn, error) {
	outbound, ok := d.outbounds[ip.String()]
	if !ok {
		return nil, errRefused
	}
	server := &testConn{
		pendingReads:  make(chan []byte, 1<<10),
		pendingWrites: make(chan []byte, 1<<10),
		closed:        make(chan struct{}),
		local:         outbound.addr,
		remote:        d.addr,
		closer:        d.closer,
	}
	client := &testConn{
		pendingReads:  server.pendingWrites,
		pendingWrites: server.pendingReads,
		closed:        make(chan struct{}),
		local:         d.addr,
		remote:        outbound.addr,
		closer:        d.closer,
	}

	select {
	case outbound.inbound <- server:
		return client, nil
	default:
		return nil, errRefused
	}
}

type testConn struct {
	partialRead   []byte
	pendingReads  chan []byte
	pendingWrites chan []byte
	closed        chan struct{}
	once          sync.Once
	closer        func(net.Addr, net.Addr)

	local, remote net.Addr
}

func (c *testConn) Read(b []byte) (int, error) {
	for len(c.partialRead) == 0 {
		select {
		case read, ok := <-c.pendingReads:
			if !ok {
				return 0, errClosed
			}
			c.partialRead = read
		case <-c.closed:
			return 0, errClosed
		}
	}

	copy(b, c.partialRead)
	if length := len(c.partialRead); len(b) > length {
		c.partialRead = nil
		return length, nil
	}
	c.partialRead = c.partialRead[len(b):]
	return len(b), nil
}

func (c *testConn) Write(b []byte) (int, error) {
	newB := make([]byte, len(b))
	copy(newB, b)

	select {
	case c.pendingWrites <- newB:
	case <-c.closed:
		return 0, errClosed
	}

	return len(b), nil
}

func (c *testConn) Close() error {
	c.once.Do(func() {
		if c.closer != nil {
			c.closer(c.local, c.remote)
		}
		close(c.closed)
	})
	return nil
}

func (c *testConn) LocalAddr() net.Addr              { return c.local }
func (c *testConn) RemoteAddr() net.Addr             { return c.remote }
func (c *testConn) SetDeadline(time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(time.Time) error { return nil }

type testHandler struct {
	router.Router
	connected    func(ids.ShortID)
	disconnected func(ids.ShortID)
}

func (h *testHandler) Connected(id ids.ShortID) {
	if h.connected != nil {
		h.connected(id)
	}
}
func (h *testHandler) Disconnected(id ids.ShortID) {
	if h.disconnected != nil {
		h.disconnected(id)
	}
}

type testUpgrader struct {
	m map[string]ids.ShortID
}

func (t *testUpgrader) Upgrade(conn net.Conn) (ids.ShortID, net.Conn, error) {
	addr := conn.RemoteAddr()
	str := addr.String()
	return t.m[str], conn, nil
}

func TestNewDefaultNetwork(t *testing.T) {
	log := logging.NoLog{}
	ip := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id := ids.ShortID(hashing.ComputeHash160Array([]byte(ip.IP().String())))
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	listener := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	serverUpgrader := NewIPUpgrader()
	clientUpgrader := NewIPUpgrader()

	vdrs := validators.NewSet()
	handler := &testHandler{}

	net := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id,
		ip,
		networkID,
		appVersion,
		versionParser,
		listener,
		caller,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net)

	go func() {
		err := net.Close()
		assert.NoError(t, err)
	}()

	err := net.Dispatch()
	assert.Error(t, err)
}

func TestEstablishConnection(t *testing.T) {
	log := logging.NoLog{}
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	ip0 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id0 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip0.IP().String())))
	ip1 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		1,
	)
	id1 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip1.IP().String())))

	listener0 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller0 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	listener1 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller1 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		outbounds: make(map[string]*testListener),
	}

	caller0.outbounds[ip1.IP().String()] = listener1
	caller1.outbounds[ip0.IP().String()] = listener0

	serverUpgrader := NewIPUpgrader()
	clientUpgrader := NewIPUpgrader()

	vdrs := validators.NewSet()

	var (
		wg0 sync.WaitGroup
		wg1 sync.WaitGroup
	)
	wg0.Add(1)
	wg1.Add(1)

	handler0 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id0 {
				wg0.Done()
			}
		},
	}

	handler1 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id1 {
				wg1.Done()
			}
		},
	}

	net0 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id0,
		ip0,
		networkID,
		appVersion,
		versionParser,
		listener0,
		caller0,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler0,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net0)

	net1 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip1,
		networkID,
		appVersion,
		versionParser,
		listener1,
		caller1,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler1,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net1)

	go func() {
		err := net0.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net1.Dispatch()
		assert.Error(t, err)
	}()

	net0.Track(ip1.IP())

	wg0.Wait()
	wg1.Wait()

	err := net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)
}

func TestDoubleTrack(t *testing.T) {
	log := logging.NoLog{}
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	ip0 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id0 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip0.IP().String())))
	ip1 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		1,
	)
	id1 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip1.IP().String())))

	listener0 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller0 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	listener1 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller1 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		outbounds: make(map[string]*testListener),
	}

	caller0.outbounds[ip1.IP().String()] = listener1
	caller1.outbounds[ip0.IP().String()] = listener0

	serverUpgrader := NewIPUpgrader()
	clientUpgrader := NewIPUpgrader()

	vdrs := validators.NewSet()

	var (
		wg0 sync.WaitGroup
		wg1 sync.WaitGroup
	)
	wg0.Add(1)
	wg1.Add(1)

	handler0 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id0 {
				wg0.Done()
			}
		},
	}

	handler1 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id1 {
				wg1.Done()
			}
		},
	}

	net0 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id0,
		ip0,
		networkID,
		appVersion,
		versionParser,
		listener0,
		caller0,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler0,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net0)

	net1 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip1,
		networkID,
		appVersion,
		versionParser,
		listener1,
		caller1,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler1,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net1)

	net0.Track(ip1.IP())
	net0.Track(ip1.IP())

	go func() {
		err := net0.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net1.Dispatch()
		assert.Error(t, err)
	}()

	wg0.Wait()
	wg1.Wait()

	err := net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)
}

func TestDoubleClose(t *testing.T) {
	log := logging.NoLog{}
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	ip0 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id0 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip0.IP().String())))
	ip1 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		1,
	)
	id1 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip1.IP().String())))

	listener0 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller0 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	listener1 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller1 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		outbounds: make(map[string]*testListener),
	}

	caller0.outbounds[ip1.IP().String()] = listener1
	caller1.outbounds[ip0.IP().String()] = listener0

	serverUpgrader := NewIPUpgrader()
	clientUpgrader := NewIPUpgrader()

	vdrs := validators.NewSet()

	var (
		wg0 sync.WaitGroup
		wg1 sync.WaitGroup
	)
	wg0.Add(1)
	wg1.Add(1)

	handler0 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id0 {
				wg0.Done()
			}
		},
	}

	handler1 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id1 {
				wg1.Done()
			}
		},
	}

	net0 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id0,
		ip0,
		networkID,
		appVersion,
		versionParser,
		listener0,
		caller0,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler0,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net0)

	net1 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip1,
		networkID,
		appVersion,
		versionParser,
		listener1,
		caller1,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler1,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net1)

	net0.Track(ip1.IP())

	go func() {
		err := net0.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net1.Dispatch()
		assert.Error(t, err)
	}()

	wg0.Wait()
	wg1.Wait()

	err := net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)

	err = net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)
}

func TestTrackConnected(t *testing.T) {
	log := logging.NoLog{}
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	ip0 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id0 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip0.IP().String())))
	ip1 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		1,
	)
	id1 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip1.IP().String())))

	listener0 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller0 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	listener1 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller1 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		outbounds: make(map[string]*testListener),
	}

	caller0.outbounds[ip1.IP().String()] = listener1
	caller1.outbounds[ip0.IP().String()] = listener0

	serverUpgrader := NewIPUpgrader()
	clientUpgrader := NewIPUpgrader()

	vdrs := validators.NewSet()

	var (
		wg0 sync.WaitGroup
		wg1 sync.WaitGroup
	)
	wg0.Add(1)
	wg1.Add(1)

	handler0 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id0 {
				wg0.Done()
			}
		},
	}

	handler1 := &testHandler{
		connected: func(id ids.ShortID) {
			if id != id1 {
				wg1.Done()
			}
		},
	}

	net0 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id0,
		ip0,
		networkID,
		appVersion,
		versionParser,
		listener0,
		caller0,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler0,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net0)

	net1 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip1,
		networkID,
		appVersion,
		versionParser,
		listener1,
		caller1,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler1,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net1)

	net0.Track(ip1.IP())

	go func() {
		err := net0.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net1.Dispatch()
		assert.Error(t, err)
	}()

	wg0.Wait()
	wg1.Wait()

	net0.Track(ip1.IP())

	err := net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)
}

func TestTrackConnectedRace(t *testing.T) {
	log := logging.NoLog{}
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	ip0 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id0 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip0.IP().String())))
	ip1 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		1,
	)
	id1 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip1.IP().String())))

	listener0 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller0 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	listener1 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller1 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		outbounds: make(map[string]*testListener),
	}

	caller0.outbounds[ip1.IP().String()] = listener1
	caller1.outbounds[ip0.IP().String()] = listener0

	serverUpgrader := NewIPUpgrader()
	clientUpgrader := NewIPUpgrader()

	vdrs := validators.NewSet()
	handler := &testHandler{}

	net0 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id0,
		ip0,
		networkID,
		appVersion,
		versionParser,
		listener0,
		caller0,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net0)

	net1 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip1,
		networkID,
		appVersion,
		versionParser,
		listener1,
		caller1,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net1)

	net0.Track(ip1.IP())

	go func() {
		err := net0.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net1.Dispatch()
		assert.Error(t, err)
	}()

	err := net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)
}

func TestPeerAliases(t *testing.T) {
	log := logging.NoLog{}
	networkID := uint32(0)
	appVersion := version.NewDefaultVersion("app", 0, 1, 0)
	versionParser := version.NewDefaultParser()

	ip0 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		0,
	)
	id0 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip0.IP().String())))
	ip1 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		1,
	)
	id1 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip1.IP().String())))
	ip2 := utils.NewDynamicIPDesc(
		net.IPv6loopback,
		2,
	)
	id2 := ids.ShortID(hashing.ComputeHash160Array([]byte(ip2.IP().String())))

	listener0 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller0 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 0,
		},
		outbounds: make(map[string]*testListener),
	}
	listener1 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller1 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 1,
		},
		outbounds: make(map[string]*testListener),
	}
	listener2 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 2,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller2 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 2,
		},
		outbounds: make(map[string]*testListener),
	}
	listener3 := &testListener{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 2,
		},
		inbound: make(chan net.Conn, 1<<10),
		closed:  make(chan struct{}),
	}
	caller3 := &testDialer{
		addr: &net.TCPAddr{
			IP:   net.IPv6loopback,
			Port: 2,
		},
		outbounds: make(map[string]*testListener),
	}

	caller0.outbounds[ip1.IP().String()] = listener1
	caller0.outbounds[ip2.IP().String()] = listener2
	caller1.outbounds[ip0.IP().String()] = listener0
	caller2.outbounds[ip0.IP().String()] = listener0
	caller3.outbounds[ip0.IP().String()] = listener0

	upgrader := &testUpgrader{
		m: map[string]ids.ShortID{
			ip0.IP().String(): id0,
			ip1.IP().String(): id1,
			ip2.IP().String(): id1,
		},
	}
	serverUpgrader := upgrader
	clientUpgrader := upgrader

	vdrs := validators.NewSet()

	var (
		wg0 sync.WaitGroup
		wg1 sync.WaitGroup
		wg2 sync.WaitGroup
	)
	wg0.Add(2)
	wg1.Add(1)
	wg2.Add(2)

	handler0 := &testHandler{
		connected: func(id ids.ShortID) {
			if id == id1 {
				wg0.Done()
			}
			if id == id2 {
				wg2.Done()
			}
		},
	}

	handler1 := &testHandler{
		connected: func(id ids.ShortID) {
			if id == id0 {
				wg0.Done()
			}
		},
	}

	net2conn := false
	handler2 := &testHandler{
		connected: func(id ids.ShortID) {
			net2conn = true
		},
	}

	handler3 := &testHandler{
		connected: func(id ids.ShortID) {
			if id == id0 {
				wg2.Done()
			}
		},
	}

	wg1Passed := false
	caller0.closer = func(local net.Addr, remote net.Addr) {
		if remote.String() == ip2.String() && !wg1Passed {
			wg1.Done()
		}
	}

	net0 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id0,
		ip0,
		networkID,
		appVersion,
		versionParser,
		listener0,
		caller0,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler0,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net0)

	net1 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip1,
		networkID,
		appVersion,
		versionParser,
		listener1,
		caller1,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler1,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net1)

	net2 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id1,
		ip2,
		networkID,
		appVersion,
		versionParser,
		listener2,
		caller2,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler2,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net2)

	net3 := NewDefaultNetwork(
		prometheus.NewRegistry(),
		log,
		id2,
		ip2,
		networkID,
		appVersion,
		versionParser,
		listener3,
		caller3,
		serverUpgrader,
		clientUpgrader,
		vdrs,
		vdrs,
		handler3,
		time.Duration(0),
		0,
		nil,
		false,
		0,
		0,
		time.Now(),
		defaultSendQueueSize,
		defaultAliasReleaseFreq,
		defaultAliasReleaseTimeout,
	)
	assert.NotNil(t, net3)

	go func() {
		err := net0.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net1.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net2.Dispatch()
		assert.Error(t, err)
	}()
	go func() {
		err := net3.Dispatch()
		assert.Error(t, err)
	}()

	// Connect to peer with id1
	net0.Track(ip1.IP())

	wg0.Wait()

	// Confirm net0 peers correct
	assert.Len(t, net0.Peers(), 1)
	assert.Equal(t, net0.Peers()[0].ID, id1.PrefixedString(constants.NodeIDPrefix))
	assert.Equal(t, net0.Peers()[0].IP, ip1.String())
	assert.Len(t, net2.Peers(), 0)

	// Attempt to connect to ip2 (same id as ip1)
	net0.Track(ip2.IP())

	wg1.Wait()
	wg1Passed = true

	// Ensure net2 never connected
	assert.False(t, net2conn)

	// Confirm that ip2 was not added to net0 peers
	assert.Len(t, net0.Peers(), 1)
	assert.Equal(t, net0.Peers()[0].ID, id1.PrefixedString(constants.NodeIDPrefix))
	assert.Equal(t, net0.Peers()[0].IP, ip1.String())
	assert.Len(t, net2.Peers(), 0)

	// Subsequent track call returns immediately with no connection attempts
	net0.Track(ip2.IP())
	time.Sleep(100 * time.Millisecond)

	// Confirm that ip2 not added to net0 peers
	assert.Len(t, net0.Peers(), 1)
	assert.Equal(t, net0.Peers()[0].ID, id1.PrefixedString(constants.NodeIDPrefix))
	assert.Equal(t, net0.Peers()[0].IP, ip1.String())
	assert.Len(t, net2.Peers(), 0)

	// Wait for aliases to be removed
	time.Sleep(1000 * time.Millisecond)

	// Track ip2 with id2
	upgrader.m[ip2.String()] = id2
	caller0.outbounds[ip2.String()] = listener3
	net0.Track(ip2.IP())

	wg2.Wait()

	// Confirm that networks have correct peers
	assert.Len(t, net0.Peers(), 2)
	// TODO: need to handle map non-deterministic response
	assert.Equal(t, net0.Peers()[0].ID, id1.PrefixedString(constants.NodeIDPrefix))
	assert.Equal(t, net0.Peers()[0].IP, ip1.String())
	assert.Equal(t, net0.Peers()[1].ID, id2.PrefixedString(constants.NodeIDPrefix))
	assert.Equal(t, net0.Peers()[1].IP, ip2.String())
	assert.Len(t, net2.Peers(), 0)
	assert.Len(t, net3.Peers(), 1)
	assert.Equal(t, net3.Peers()[0].ID, id0.PrefixedString(constants.NodeIDPrefix))
	assert.Equal(t, net3.Peers()[0].IP, ip0.String())

	// Cleanup
	err := net0.Close()
	assert.NoError(t, err)

	err = net1.Close()
	assert.NoError(t, err)

	err = net2.Close()
	assert.NoError(t, err)

	err = net3.Close()
	assert.NoError(t, err)
}
