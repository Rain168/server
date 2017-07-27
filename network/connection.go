package network

import (
	"errors"
	"fmt"
	"github.com/go-kit/kit/log"
	cc "github.com/msackman/chancell"
	"goshawkdb.io/server"
	"goshawkdb.io/server/configuration"
	eng "goshawkdb.io/server/txnengine"
	"math/rand"
	"net"
	"time"
)

type Connection struct {
	logger            log.Logger
	connectionManager *ConnectionManager
	cellTail          *cc.ChanCellTail
	enqueueQueryInner func(connectionMsg, *cc.ChanCell, cc.CurCellConsumer) (bool, cc.CurCellConsumer)
	queryChan         <-chan connectionMsg
	rng               *rand.Rand
	currentState      connectionStateMachineComponent
	connectionDelay
	connectionDial
	connectionHandshake
	connectionRun
}

type connectionMsg interface {
	witness() connectionMsg
}

type connectionMsgBasic struct{}

func (cmb connectionMsgBasic) witness() connectionMsg { return cmb }

type connectionMsgStartShutdown struct{ connectionMsgBasic }
type connectionMsgShutdownComplete struct{ connectionMsgBasic }

type connectionMsgTopologyChanged struct {
	connectionMsgBasic
	topology   *configuration.Topology
	resultChan chan struct{}
}

func (cmtc *connectionMsgTopologyChanged) maybeClose() {
	select {
	case <-cmtc.resultChan:
	default:
		close(cmtc.resultChan)
	}
}

type connectionMsgStatus struct {
	connectionMsgBasic
	*server.StatusConsumer
}

// for paxos.Actorish
type connectionMsgExec func()

func (cme connectionMsgExec) witness() connectionMsg { return cme }

// is async
func (conn *Connection) Shutdown() {
	conn.enqueueQuery(connectionMsgStartShutdown{})
}
func (conn *Connection) shutdownComplete() {
	conn.enqueueQuery(connectionMsgShutdownComplete{})
}

func (conn *Connection) TopologyChanged(topology *configuration.Topology, done func(bool)) {
	finished := make(chan struct{})
	msg := &connectionMsgTopologyChanged{
		resultChan: finished,
		topology:   topology,
	}
	if conn.enqueueQuery(msg) {
		go func() {
			select {
			case <-finished:
			case <-conn.cellTail.Terminated:
			}
			done(true) // connection drop is not a problem
		}()
	} else {
		done(true)
	}
}

func (conn *Connection) Status(sc *server.StatusConsumer) {
	conn.enqueueQuery(connectionMsgStatus{StatusConsumer: sc})
}

// This is for the paxos.Actorish interface
func (conn *Connection) Enqueue(fun func()) bool {
	return conn.enqueueQuery(connectionMsgExec(fun))
}

// This is for the paxos.Actorish interface
func (conn *Connection) WithTerminatedChan(fun func(chan struct{})) {
	fun(conn.cellTail.Terminated)
}

type connectionQueryCapture struct {
	conn *Connection
	msg  connectionMsg
}

func (cqc *connectionQueryCapture) ccc(cell *cc.ChanCell) (bool, cc.CurCellConsumer) {
	return cqc.conn.enqueueQueryInner(cqc.msg, cell, cqc.ccc)
}

func (conn *Connection) enqueueQuery(msg connectionMsg) bool {
	cqc := &connectionQueryCapture{conn: conn, msg: msg}
	return conn.cellTail.WithCell(cqc.ccc)
}

func NewConnectionTCPTLSCapnpDialer(remoteHost string, cm *ConnectionManager, logger log.Logger) *Connection {
	logger = log.With(logger, "subsystem", "connection", "dir", "outgoing", "protocol", "capnp")
	dialer := NewTCPDialerForTLSCapnp(remoteHost, cm, logger)
	return NewConnectionWithDialer(dialer, cm, logger)
}

func NewConnectionTCPTLSCapnpHandshaker(socket *net.TCPConn, cm *ConnectionManager, count uint32, logger log.Logger) *Connection {
	logger = log.With(logger, "subsystem", "connection", "dir", "incoming", "protocol", "capnp")
	yesman := NewTLSCapnpHandshaker(nil, socket, cm, count, "", logger)
	return NewConnectionWithHandshaker(yesman, cm, logger)
}

func NewConnectionWithHandshaker(yesman Handshaker, cm *ConnectionManager, logger log.Logger) *Connection {
	conn := &Connection{
		logger:            logger,
		connectionManager: cm,
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	conn.Handshaker = yesman
	conn.start()
	return conn
}

func NewConnectionWithDialer(phone Dialer, cm *ConnectionManager, logger log.Logger) *Connection {
	conn := &Connection{
		logger:            logger,
		connectionManager: cm,
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	conn.Dialer = phone
	conn.start()
	return conn
}

func (conn *Connection) start() {
	var head *cc.ChanCellHead
	head, conn.cellTail = cc.NewChanCellTail(
		func(n int, cell *cc.ChanCell) {
			queryChan := make(chan connectionMsg, n)
			cell.Open = func() { conn.queryChan = queryChan }
			cell.Close = func() { close(queryChan) }
			conn.enqueueQueryInner = func(msg connectionMsg, curCell *cc.ChanCell, cont cc.CurCellConsumer) (bool, cc.CurCellConsumer) {
				if curCell == cell {
					select {
					case queryChan <- msg:
						return true, nil
					default:
						return false, nil
					}
				} else {
					return false, cont
				}
			}
		})

	conn.connectionDelay.init(conn)
	conn.connectionDial.init(conn)
	conn.connectionHandshake.init(conn)
	conn.connectionRun.init(conn)

	if conn.Dialer != nil {
		conn.currentState = &conn.connectionDial
	} else if conn.Handshaker != nil {
		conn.currentState = &conn.connectionHandshake
	}

	go conn.actorLoop(head)
}

func (conn *Connection) actorLoop(head *cc.ChanCellHead) {
	conn.topology = conn.connectionManager.AddTopologySubscriber(eng.ConnectionSubscriber, conn)
	defer conn.connectionManager.RemoveTopologySubscriberAsync(eng.ConnectionSubscriber, conn)

	defer func() {
		if r := recover(); r != nil {
			conn.logger.Log("msg", "Connection panicked!", "error", fmt.Sprint(r))
		}
	}()

	var (
		err       error
		oldState  connectionStateMachineComponent
		queryChan <-chan connectionMsg
		queryCell *cc.ChanCell
	)
	chanFun := func(cell *cc.ChanCell) { queryChan, queryCell = conn.queryChan, cell }
	head.WithCell(chanFun)
	if conn.topology == nil {
		// Most likely is that the connection manager has shutdown due
		// to some other error and so the sync enqueue failed.
		err = errors.New("No local topology, not ready for any connections.")
	}

	terminated := err != nil // have we stopped?
	shuttingDown := false    // are we shutting down?
	terminate := false       // what should we do next?
	for !terminated {
		if oldState != conn.currentState {
			oldState = conn.currentState
			terminate, err = conn.currentState.start()
		} else if msg, ok := <-queryChan; ok {
			terminate, terminated, err = conn.handleMsg(msg)
		} else {
			head.Next(queryCell, chanFun)
		}
		terminate = terminate || err != nil
		if terminate {
			conn.startShutdown(shuttingDown, err)
			shuttingDown = true
			err = nil
		}
	}
	conn.cellTail.Terminate()
	conn.handleShutdown(err)
	conn.logger.Log("msg", "Terminated.")
}

func (conn *Connection) handleMsg(msg connectionMsg) (terminate, terminated bool, err error) {
	switch msgT := msg.(type) {
	case connectionMsgStartShutdown:
		terminate = true
	case connectionMsgShutdownComplete:
		terminated = true
	case *connectionDelay:
		msgT.received()
	case connectionReadError:
		err = msgT.error
	case connectionMsgExec:
		msgT()
	case connectionMsgExecError:
		err = msgT()
	case *connectionMsgTopologyChanged:
		err = conn.topologyChanged(msgT)
	case connectionMsgStatus:
		conn.status(msgT.StatusConsumer)
	default:
		err = fmt.Errorf("Fatal to Connection: Received unexpected message: %#v", msgT)
	}
	if err != nil && !terminate {
		err = conn.maybeRestartConnection(err)
	}
	return
}

func (conn *Connection) maybeRestartConnection(err error) error {
	if conn.Handshaker != nil {
		conn.Dialer = conn.Handshaker.RestartDialer()
	} else if conn.Protocol != nil {
		conn.Dialer = conn.Protocol.RestartDialer()
	}

	if conn.Dialer == nil {
		return err // it's fatal; actor loop will shutdown Protocol or Handshaker
	} else {
		conn.logger.Log("msg", "Restarting.", "error", err)
		conn.nextState(&conn.connectionDelay)
		return nil
	}
}

func (conn *Connection) startShutdown(shutdownStarted bool, err error) {
	if err != nil {
		conn.logger.Log("error", err)
	}
	if !shutdownStarted {
		if conn.Protocol != nil {
			conn.Protocol.InternalShutdown()
			conn.Protocol = nil
			conn.Handshaker = nil
		} else {
			conn.shutdownComplete()
		}
	}
}

func (conn *Connection) handleShutdown(err error) {
	if err != nil {
		conn.logger.Log("error", err)
	}
	if conn.Protocol != nil {
		conn.Protocol.InternalShutdown()
	} else if conn.Handshaker != nil {
		conn.Handshaker.InternalShutdown()
	}
	conn.currentState = nil
	conn.Protocol = nil
	conn.Handshaker = nil
	conn.Dialer = nil
}

// state machine

type connectionStateMachineComponent interface {
	init(*Connection)
	start() (bool, error)
	connectionStateMachineComponentWitness()
}

func (conn *Connection) nextState(requestedState connectionStateMachineComponent) {
	if requestedState == nil {
		switch conn.currentState {
		case &conn.connectionDelay:
			conn.currentState = &conn.connectionDial
		case &conn.connectionDial:
			conn.currentState = &conn.connectionHandshake
		case &conn.connectionHandshake:
			conn.currentState = &conn.connectionRun
		default:
			panic(fmt.Sprintf("Unexpected current state for nextState: %v", conn.currentState))
		}
	} else {
		conn.currentState = requestedState
	}
}

func (conn *Connection) status(sc *server.StatusConsumer) {
	if conn.Protocol != nil {
		sc.Emit(fmt.Sprintf("Connection %v", conn.Protocol))
	} else if conn.Handshaker != nil {
		sc.Emit(fmt.Sprintf("Connection %v", conn.Handshaker))
	} else if conn.Dialer != nil {
		sc.Emit(fmt.Sprintf("Connection %v", conn.Dialer))
	}
	sc.Join()
}

// Delay

type connectionDelay struct {
	connectionMsgBasic
	*Connection
	delay *time.Timer
}

func (cd *connectionDelay) connectionStateMachineComponentWitness() {}
func (cd *connectionDelay) String() string                          { return "ConnectionDelay" }

func (cd *connectionDelay) init(conn *Connection) {
	cd.Connection = conn
}

func (cd *connectionDelay) start() (bool, error) {
	cd.Handshaker = nil
	cd.Protocol = nil
	if cd.delay == nil {
		delay := server.ConnectionRestartDelayMin + time.Duration(cd.rng.Intn(server.ConnectionRestartDelayRangeMS))*time.Millisecond
		cd.delay = time.AfterFunc(delay, func() {
			cd.enqueueQuery(cd)
		})
	}
	return false, nil
}

func (cd *connectionDelay) received() {
	if cd.currentState == cd {
		cd.delay = nil
		cd.nextState(nil)
	}
}

// Dial

type connectionDial struct {
	*Connection
	Dialer
}

func (cc *connectionDial) connectionStateMachineComponentWitness() {}
func (cc *connectionDial) String() string                          { return "ConnectionDial" }

func (cc *connectionDial) init(conn *Connection) {
	cc.Connection = conn
}

func (cc *connectionDial) start() (bool, error) {
	yesman, err := cc.Dial()
	if err == nil {
		cc.Handshaker = yesman
		cc.nextState(nil)
	} else {
		cc.logger.Log("msg", "Error when dialing.", "error", err)
		cc.nextState(&cc.connectionDelay)
	}
	return false, nil
}

// Handshake

type connectionHandshake struct {
	*Connection
	topology *configuration.Topology
	Handshaker
}

func (cah *connectionHandshake) connectionStateMachineComponentWitness() {}
func (cah *connectionHandshake) String() string                          { return "ConnectionHandshake" }

func (cah *connectionHandshake) init(conn *Connection) {
	cah.Connection = conn
}

func (cah *connectionHandshake) start() (bool, error) {
	protocol, err := cah.PerformHandshake(cah.topology)
	if err == nil {
		cah.Protocol = protocol
		cah.nextState(nil)
		return false, nil
	} else {
		return false, err
	}
}

// Run

type connectionRun struct {
	*Connection
	Protocol
}

func (cr *connectionRun) connectionStateMachineComponentWitness() {}
func (cr *connectionRun) String() string                          { return "ConnectionRun" }

func (cr *connectionRun) init(conn *Connection) {
	cr.Connection = conn
}

func (cr *connectionRun) start() (bool, error) {
	cr.Handshaker = nil
	return false, cr.Run(cr.Connection)
}

func (cr *connectionRun) topologyChanged(tc *connectionMsgTopologyChanged) error {
	switch {
	case cr.Protocol != nil:
		cr.topology = tc.topology
		return cr.Protocol.TopologyChanged(tc)
	default:
		tc.maybeClose()
		return nil
	}
}

// Reader

type connectionReadError struct {
	connectionMsgBasic
	error
}

type connectionMsgExecError func() error

func (cmee connectionMsgExecError) witness() connectionMsg { return cmee }
