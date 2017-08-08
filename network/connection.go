package network

import (
	"errors"
	"fmt"
	"github.com/go-kit/kit/log"
	"goshawkdb.io/common"
	"goshawkdb.io/common/actor"
	"goshawkdb.io/server"
	"goshawkdb.io/server/configuration"
	eng "goshawkdb.io/server/txnengine"
	"math/rand"
	"net"
	"time"
)

type Connection struct {
	*actor.Mailbox
	*actor.BasicServerOuter

	connectionManager *ConnectionManager
	shuttingDown      bool
	handshaker        Handshaker
	rng               *rand.Rand
	previousState     connectionStateMachineComponent
	currentState      connectionStateMachineComponent
	connectionDelay
	connectionDial
	connectionHandshake
	connectionRun

	inner connectionInner
}

type connectionInner struct {
	*Connection
	*actor.BasicServerInner // super-type, essentially
}

// we are dialing out to someone else
func NewConnectionTCPTLSCapnpDialer(remoteHost string, cm *ConnectionManager, logger log.Logger) *Connection {
	logger = log.With(logger, "subsystem", "connection", "dir", "outgoing", "protocol", "capnp")
	phone := common.NewTCPDialer(nil, remoteHost, logger)
	yesman := NewTLSCapnpHandshaker(phone, logger, 0, cm)
	return NewConnection(yesman, cm, logger)
}

// the socket is already established - we got it from the TCP listener
func NewConnectionTCPTLSCapnpHandshaker(socket *net.TCPConn, cm *ConnectionManager, count uint32, logger log.Logger) {
	logger = log.With(logger, "subsystem", "connection", "dir", "incoming", "protocol", "capnp")
	phone := common.NewTCPDialer(socket, "", logger)
	yesman := NewTLSCapnpHandshaker(phone, logger, count, cm)
	NewConnection(yesman, cm, logger)
}

func NewConnection(yesman Handshaker, cm *ConnectionManager, logger log.Logger) *Connection {
	c := &Connection{
		connectionManager: cm,
		handshaker:        yesman,
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	ci := &c.inner
	ci.Connection = c
	ci.BasicServerInner = actor.NewBasicServerInner(logger)

	_, err := actor.Spawn(ci)
	if err != nil {
		panic(err) // "impossible"
	}

	c.EnqueueMsg(connectionMsgInit{Connection: c})

	return c
}

type connectionMsgInit struct {
	*Connection
}

func (msg connectionMsgInit) Exec() (bool, error) {
	msg.topology = msg.connectionManager.AddTopologySubscriber(eng.ConnectionSubscriber, msg.Connection)
	if msg.topology == nil {
		// Most likely is that the connection manager has shutdown due
		// to some other error and so the sync enqueue failed.
		return false, errors.New("No local topology, not ready for any connections.")
	}

	msg.nextState(nil)
	return false, nil
}

func (c *Connection) shutdownCompleted() {
	c.EnqueueMsg(actor.MsgShutdown{})
}

type connectionMsgTopologyChanged struct {
	actor.MsgSyncQuery
	c        *Connection
	topology *configuration.Topology
}

func (msg *connectionMsgTopologyChanged) Exec() (bool, error) {
	msg.c.topology = msg.topology
	switch {
	case msg.c.protocol != nil:
		return false, msg.c.protocol.TopologyChanged(msg) // This calls msg.MustClose
	default:
		msg.MustClose()
		return false, nil
	}
}

func (c *Connection) TopologyChanged(topology *configuration.Topology, done func(bool)) {
	msg := &connectionMsgTopologyChanged{topology: topology, c: c}
	msg.InitMsg(c)
	if c.EnqueueMsg(msg) {
		go func() {
			msg.Wait()
			done(true) // connection drop is not a problem
		}()
	} else {
		done(true) // connection drop is not a problem
	}
}

type connectionMsgStatus struct {
	*Connection
	sc *server.StatusConsumer
}

func (msg connectionMsgStatus) Exec() (bool, error) {
	if msg.protocol != nil {
		msg.sc.Emit(fmt.Sprintf("Connection %v", msg.protocol))
	} else if msg.handshaker != nil {
		msg.sc.Emit(fmt.Sprintf("Connection %v", msg.handshaker))
	}
	msg.sc.Join()
	return false, nil
}

func (c *Connection) Status(sc *server.StatusConsumer) {
	c.EnqueueMsg(connectionMsgStatus{Connection: c, sc: sc})
}

func (c *connectionInner) Init(self *actor.Actor) (bool, error) {
	terminate, err := c.BasicServerInner.Init(self)
	if terminate || err != nil {
		return terminate, err
	}

	c.Mailbox = self.Mailbox
	c.BasicServerOuter = actor.NewBasicServerOuter(self.Mailbox)

	c.connectionDelay.init(c.Connection)
	c.connectionDial.init(c.Connection)
	c.connectionHandshake.init(c.Connection)
	c.connectionRun.init(c.Connection)

	return false, nil
}

func (c *connectionInner) HandleShutdown(err error) bool {
	if c.shuttingDown {
		c.connectionManager.RemoveTopologySubscriberAsync(eng.ConnectionSubscriber, c)
		return c.BasicServerInner.HandleShutdown(err)
	}

	if err != nil {
		if err = c.maybeRestartConnection(err); err == nil {
			return false
		}
	}

	if err != nil {
		c.Logger.Log("error", err)
	}
	c.shuttingDown = true
	if c.protocol != nil {
		c.protocol.InternalShutdown() // this will call shutdownCompleted
	} else {
		if c.handshaker != nil {
			c.handshaker.InternalShutdown()
		}
		c.shutdownCompleted()
	}
	c.currentState = nil
	c.protocol = nil
	c.handshaker = nil
	return false
}

func (c *connectionInner) HandleBeat() (terminate bool, err error) {
	for c.currentState != nil && c.previousState != c.currentState && !terminate && err == nil {
		c.previousState = c.currentState
		terminate, err = c.currentState.start()
	}
	return
}

func (c *Connection) maybeRestartConnection(err error) error {
	restartable := false
	if c.protocol != nil {
		restartable = c.protocol.Restart()
	} else if c.handshaker != nil {
		restartable = c.handshaker.Restart()
	}

	if restartable {
		c.inner.Logger.Log("msg", "Restarting.", "error", err)
		c.nextState(&c.connectionDelay)
		return nil
	} else {
		return err // it's fatal; HandleShutdown will shutdown Protocol and Handshaker
	}
}

// state machine

type connectionStateMachineComponent interface {
	init(*Connection)
	start() (bool, error)
}

func (c *Connection) nextState(requestedState connectionStateMachineComponent) {
	if requestedState == nil {
		switch c.currentState {
		case nil, &c.connectionDelay:
			c.currentState = &c.connectionDial
		case &c.connectionDial:
			c.currentState = &c.connectionHandshake
		case &c.connectionHandshake:
			c.currentState = &c.connectionRun
		default:
			panic(fmt.Sprintf("Unexpected current state for nextState: %v", c.currentState))
		}
	} else {
		c.currentState = requestedState
	}
}

// Delay

type connectionDelay struct {
	*Connection
	delay *time.Timer
}

func (cd *connectionDelay) String() string { return "ConnectionDelay" }

func (cd *connectionDelay) init(conn *Connection) {
	cd.Connection = conn
}

func (cd *connectionDelay) start() (bool, error) {
	cd.protocol = nil
	if cd.delay == nil {
		delay := server.ConnectionRestartDelayMin + time.Duration(cd.rng.Intn(server.ConnectionRestartDelayRangeMS))*time.Millisecond
		cd.delay = time.AfterFunc(delay, func() { cd.EnqueueMsg(cd) })
	}
	return false, nil
}

func (cd *connectionDelay) Exec() (bool, error) {
	if cd.currentState == cd {
		cd.delay = nil
		cd.nextState(nil)
	}
	return false, nil
}

// Dial

type connectionDial struct {
	*Connection
}

func (cc *connectionDial) String() string { return "ConnectionDial" }

func (cc *connectionDial) init(conn *Connection) {
	cc.Connection = conn
}

func (cc *connectionDial) start() (bool, error) {
	err := cc.handshaker.Dial()
	if err == nil {
		cc.nextState(nil)
	} else {
		cc.inner.Logger.Log("msg", "Error when dialing.", "error", err)
		cc.nextState(&cc.connectionDelay)
	}
	return false, nil
}

// Handshake

type connectionHandshake struct {
	*Connection
	topology *configuration.Topology
}

func (cah *connectionHandshake) String() string { return "ConnectionHandshake" }

func (cah *connectionHandshake) init(conn *Connection) {
	cah.Connection = conn
}

func (cah *connectionHandshake) start() (bool, error) {
	protocol, err := cah.handshaker.PerformHandshake(cah.topology)
	if err == nil {
		cah.protocol = protocol
		cah.nextState(nil)
		return false, nil
	} else {
		return false, err
	}
}

// Run

type connectionRun struct {
	*Connection
	protocol Protocol
}

func (cr *connectionRun) String() string { return "ConnectionRun" }

func (cr *connectionRun) init(conn *Connection) {
	cr.Connection = conn
}

func (cr *connectionRun) start() (bool, error) {
	return false, cr.protocol.Run(cr.Connection)
}
