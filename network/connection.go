package network

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	cc "github.com/msackman/chancell"
	"goshawkdb.io/common"
	cmsgs "goshawkdb.io/common/capnp"
	"goshawkdb.io/server"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/client"
	"goshawkdb.io/server/configuration"
	"goshawkdb.io/server/paxos"
	eng "goshawkdb.io/server/txnengine"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"
)

type Connection struct {
	remoteHost        string
	remoteRMId        common.RMId
	remoteBootCount   uint32
	remoteClusterUUId uint64
	combinedTieBreak  uint32
	socket            net.Conn
	ConnectionNumber  uint32
	connectionManager *ConnectionManager
	submitter         *client.ClientTxnSubmitter
	cellTail          *cc.ChanCellTail
	enqueueQueryInner func(connectionMsg, *cc.ChanCell, cc.CurCellConsumer) (bool, cc.CurCellConsumer)
	queryChan         <-chan connectionMsg
	rng               *rand.Rand
	currentState      connectionStateMachineComponent
	connectionDelay
	connectionDial
	connectionAwaitHandshake
	connectionAwaitClientHandshake
	connectionAwaitServerHandshake
	connectionRun
}

type connectionMsg interface {
	witness() connectionMsg
}

type connectionMsgBasic struct{}

func (cmb connectionMsgBasic) witness() connectionMsg { return cmb }

type connectionMsgShutdown struct{ connectionMsgBasic }

type connectionMsgSend []byte

func (cms connectionMsgSend) witness() connectionMsg { return cms }

type connectionMsgOutcomeReceived struct {
	connectionMsgBasic
	sender  common.RMId
	txn     *eng.TxnReader
	outcome *msgs.Outcome
}

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

func (conn *Connection) Shutdown(sync paxos.Blocking) {
	if conn.enqueueQuery(connectionMsgShutdown{}) && sync == paxos.Sync {
		conn.cellTail.Wait()
	}
}

func (conn *Connection) Send(msg []byte) {
	conn.enqueueQuery(connectionMsgSend(msg))
}

func (conn *Connection) SubmissionOutcomeReceived(sender common.RMId, txn *eng.TxnReader, outcome *msgs.Outcome) {
	conn.enqueueQuery(connectionMsgOutcomeReceived{
		sender:  sender,
		txn:     txn,
		outcome: outcome,
	})
}

func (conn *Connection) TopologyChanged(topology *configuration.Topology, done func(bool)) {
	msg := &connectionMsgTopologyChanged{
		resultChan: make(chan struct{}),
		topology:   topology,
	}
	if conn.enqueueQuery(msg) {
		go func() {
			select {
			case <-msg.resultChan:
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

type connectionMsgServerConnectionsChanged struct {
	servers map[common.RMId]paxos.Connection
	done    func()
}

func (cmdhc connectionMsgServerConnectionsChanged) witness() connectionMsg { return cmdhc }

func (conn *Connection) ConnectedRMs(servers map[common.RMId]paxos.Connection) {
	conn.enqueueQuery(connectionMsgServerConnectionsChanged{servers: servers})
}
func (conn *Connection) ConnectionLost(rmId common.RMId, servers map[common.RMId]paxos.Connection) {
	conn.enqueueQuery(connectionMsgServerConnectionsChanged{servers: servers})
}
func (conn *Connection) ConnectionEstablished(rmId common.RMId, c paxos.Connection, servers map[common.RMId]paxos.Connection, done func()) {
	conn.enqueueQuery(connectionMsgServerConnectionsChanged{
		servers: servers,
		done:    done,
	})
}

func (conn *Connection) enqueueQuery(msg connectionMsg) bool {
	var f cc.CurCellConsumer
	f = func(cell *cc.ChanCell) (bool, cc.CurCellConsumer) {
		return conn.enqueueQueryInner(msg, cell, f)
	}
	return conn.cellTail.WithCell(f)
}

func NewConnectionToDial(host string, cm *ConnectionManager) *Connection {
	if host == "" {
		panic("empty host")
	}
	conn := &Connection{
		remoteHost:        host,
		connectionManager: cm,
	}
	conn.start()
	return conn
}

func NewConnectionFromTCPConn(socket *net.TCPConn, cm *ConnectionManager, count uint32) *Connection {
	if err := common.ConfigureSocket(socket); err != nil {
		log.Println(err)
		return nil
	}
	conn := &Connection{
		socket:            socket,
		connectionManager: cm,
		ConnectionNumber:  count,
	}
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

	conn.rng = rand.New(rand.NewSource(time.Now().UnixNano()))

	conn.connectionDelay.init(conn)
	conn.connectionDial.init(conn)
	conn.connectionAwaitHandshake.init(conn)
	conn.connectionAwaitServerHandshake.init(conn)
	conn.connectionAwaitClientHandshake.init(conn)
	conn.connectionRun.init(conn)

	if conn.socket == nil {
		conn.currentState = &conn.connectionDial
	} else {
		conn.currentState = &conn.connectionAwaitHandshake
	}

	go conn.actorLoop(head)
}

func (conn *Connection) actorLoop(head *cc.ChanCellHead) {
	conn.topology = conn.connectionManager.AddTopologySubscriber(eng.ConnectionSubscriber, conn)
	defer conn.connectionManager.RemoveTopologySubscriberAsync(eng.ConnectionSubscriber, conn)

	var (
		err       error
		oldState  connectionStateMachineComponent
		queryChan <-chan connectionMsg
		queryCell *cc.ChanCell
	)
	chanFun := func(cell *cc.ChanCell) { queryChan, queryCell = conn.queryChan, cell }
	head.WithCell(chanFun)
	if conn.topology == nil {
		panic("Nil topology on connection start!")
		err = errors.New("No local topology, not ready for any connections")
	}

	terminate := err != nil
	for !terminate {
		if oldState != conn.currentState {
			oldState = conn.currentState
			terminate, err = conn.currentState.start()
		} else if msg, ok := <-queryChan; ok {
			terminate, err = conn.handleMsg(msg)
		} else {
			head.Next(queryCell, chanFun)
		}
		terminate = terminate || err != nil
	}
	conn.cellTail.Terminate()
	conn.handleShutdown(err)
	log.Println("Connection terminated")
}

func (conn *Connection) handleMsg(msg connectionMsg) (terminate bool, err error) {
	switch msgT := msg.(type) {
	case connectionMsgShutdown:
		terminate = true
		conn.currentState = nil
	case *connectionDelay:
		msgT.received()
	case *connectionBeater:
		err = conn.beat()
	case connectionReadError:
		conn.reader = nil
		err = conn.connectionRun.maybeRestartConnection(msgT.error)
	case connectionReadMessage:
		err = conn.handleMsgFromServer((msgs.Message)(msgT))
	case connectionReadClientMessage:
		err = conn.handleMsgFromClient((cmsgs.ClientMessage)(msgT))
	case connectionMsgSend:
		err = conn.sendMessage(msgT)
	case connectionMsgOutcomeReceived:
		err = conn.outcomeReceived(msgT)
	case *connectionMsgTopologyChanged:
		err = conn.topologyChanged(msgT)
	case connectionMsgServerConnectionsChanged:
		err = conn.serverConnectionsChanged(msgT.servers)
		if msgT.done != nil {
			msgT.done()
		}
	case connectionMsgStatus:
		conn.status(msgT.StatusConsumer)
	default:
		err = fmt.Errorf("Fatal to Connection: Received unexpected message: %#v", msgT)
	}
	return
}

func (conn *Connection) handleShutdown(err error) {
	if err != nil {
		log.Println(err)
	}
	conn.maybeStopBeater()
	conn.maybeStopReaderAndCloseSocket()
	if conn.isClient {
		conn.connectionManager.ClientLost(conn.ConnectionNumber, conn)
		if conn.submitter != nil {
			conn.submitter.Shutdown()
		}
	}
	if conn.isServer {
		conn.connectionManager.ServerLost(conn, conn.remoteRMId, false)
	}
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
			conn.currentState = &conn.connectionAwaitHandshake
		case &conn.connectionAwaitClientHandshake:
			conn.currentState = &conn.connectionRun
		case &conn.connectionAwaitServerHandshake:
			conn.currentState = &conn.connectionRun
		default:
			panic(fmt.Sprintf("Unexpected current state for nextState: %v", conn.currentState))
		}
	} else {
		conn.currentState = requestedState
	}
}

func (conn *Connection) status(sc *server.StatusConsumer) {
	sc.Emit(fmt.Sprintf("Connection to %v (%v, %v)", conn.remoteHost, conn.remoteRMId, conn.remoteBootCount))
	sc.Emit(fmt.Sprintf("- Current State: %v", conn.currentState))
	sc.Emit(fmt.Sprintf("- IsServer? %v", conn.isServer))
	sc.Emit(fmt.Sprintf("- IsClient? %v", conn.isClient))
	if conn.submitter != nil {
		conn.submitter.Status(sc.Fork())
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
	cd.maybeStopReaderAndCloseSocket()
	cd.maybeStopBeater()
	cd.isServer = false
	cd.isClient = false
	cd.peerCerts = nil
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
}

func (cc *connectionDial) connectionStateMachineComponentWitness() {}
func (cc *connectionDial) String() string                          { return "ConnectionDial" }

func (cc *connectionDial) init(conn *Connection) {
	cc.Connection = conn
}

func (cc *connectionDial) start() (bool, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", cc.remoteHost)
	if err != nil {
		log.Println(err)
		cc.nextState(&cc.connectionDelay)
		return false, nil
	}
	socket, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		log.Println(err)
		cc.nextState(&cc.connectionDelay)
		return false, nil
	}
	if err := common.ConfigureSocket(socket); err != nil {
		log.Println(err)
		cc.nextState(&cc.connectionDelay)
		return false, nil
	}
	cc.socket = socket
	cc.nextState(nil)
	return false, nil
}

// Await Handshake

type connectionAwaitHandshake struct {
	*Connection
	isServer bool
	isClient bool
	topology *configuration.Topology
}

func (cah *connectionAwaitHandshake) connectionStateMachineComponentWitness() {}
func (cah *connectionAwaitHandshake) String() string                          { return "ConnectionAwaitHandshake" }

func (cah *connectionAwaitHandshake) init(conn *Connection) {
	cah.Connection = conn
}

func (cah *connectionAwaitHandshake) start() (bool, error) {

	helloSeg := cah.makeHello()
	if err := cah.send(server.SegToBytes(helloSeg)); err != nil {
		return cah.maybeRestartConnection(err)
	}

	if seg, err := cah.readOne(); err == nil {
		hello := cmsgs.ReadRootHello(seg)
		if cah.verifyHello(&hello) {
			if hello.IsClient() {
				cah.isClient = true
				cah.nextState(&cah.connectionAwaitClientHandshake)

			} else {
				cah.isServer = true
				cah.nextState(&cah.connectionAwaitServerHandshake)
			}
			return false, nil

		} else {
			product := hello.Product()
			if l := len(common.ProductName); len(product) > l {
				product = product[:l] + "..."
			}
			version := hello.Version()
			if l := len(common.ProductVersion); len(version) > l {
				version = version[:l] + "..."
			}
			return cah.maybeRestartConnection(fmt.Errorf("Received erroneous hello from peer: received product name '%s' (expected '%s'), product version '%s' (expected '%s')",
				product, common.ProductName, version, common.ProductVersion))
		}
	} else {
		return cah.maybeRestartConnection(err)
	}
}

func (cah *connectionAwaitHandshake) makeHello() *capn.Segment {
	seg := capn.NewBuffer(nil)
	hello := cmsgs.NewRootHello(seg)
	hello.SetProduct(common.ProductName)
	hello.SetVersion(common.ProductVersion)
	hello.SetIsClient(false)
	return seg
}

func (cah *connectionAwaitHandshake) send(msg []byte) error {
	l := len(msg)
	for l > 0 {
		switch w, err := cah.socket.Write(msg); {
		case err != nil:
			return err
		case w == l:
			return nil
		default:
			msg = msg[w:]
			l -= w
		}
	}
	return nil
}

func (cah *connectionAwaitHandshake) readOne() (*capn.Segment, error) {
	return capn.ReadFromStream(cah.socket, nil)
}

func (cah *connectionAwaitHandshake) verifyHello(hello *cmsgs.Hello) bool {
	return hello.Product() == common.ProductName &&
		hello.Version() == common.ProductVersion
}

func (cah *connectionAwaitHandshake) maybeRestartConnection(err error) (bool, error) {
	if cah.remoteHost == "" {
		// we came from the listener and don't know who the remote is, so have to shutdown
		return false, err
	} else {
		log.Println(err)
		cah.nextState(&cah.connectionDelay)
		return false, nil
	}
}

func (cah *connectionAwaitHandshake) commonTLSConfig() *tls.Config {
	nodeCertPrivKeyPair := cah.connectionManager.NodeCertificatePrivateKeyPair
	roots := x509.NewCertPool()
	roots.AddCert(nodeCertPrivKeyPair.CertificateRoot)

	return &tls.Config{
		Certificates: []tls.Certificate{
			tls.Certificate{
				Certificate: [][]byte{nodeCertPrivKeyPair.Certificate},
				PrivateKey:  nodeCertPrivKeyPair.PrivateKey,
			},
		},
		CipherSuites:             []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		MinVersion:               tls.VersionTLS12,
		PreferServerCipherSuites: true,
		ClientCAs:                roots,
		RootCAs:                  roots,
	}
}

// Await Server Handshake

type connectionAwaitServerHandshake struct {
	*Connection
}

func (cash *connectionAwaitServerHandshake) connectionStateMachineComponentWitness() {}
func (cash *connectionAwaitServerHandshake) String() string                          { return "ConnectionAwaitServerHandshake" }

func (cash *connectionAwaitServerHandshake) init(conn *Connection) {
	cash.Connection = conn
}

func (cash *connectionAwaitServerHandshake) start() (bool, error) {
	// TLS seems to require us to pick one end as the client and one
	// end as the server even though in a server-server connection we
	// really don't care which is which.
	config := cash.commonTLSConfig()
	if cash.remoteHost == "" {
		// We came from the listener, so we're going to act as the server.
		config.ClientAuth = tls.RequireAndVerifyClientCert
		socket := tls.Server(cash.socket, config)
		if err := socket.SetDeadline(time.Time{}); err != nil {
			return cash.connectionAwaitHandshake.maybeRestartConnection(err)
		}
		cash.socket = socket

	} else {
		config.InsecureSkipVerify = true
		socket := tls.Client(cash.socket, config)
		if err := socket.SetDeadline(time.Time{}); err != nil {
			return cash.connectionAwaitHandshake.maybeRestartConnection(err)
		}
		cash.socket = socket

		// This is nuts: as a server, we can demand the client cert and
		// verify that without any concept of a client name. But as the
		// client, if we don't have a server name, then we have to do
		// the verification ourself. Why is TLS asymmetric?!

		if err := socket.Handshake(); err != nil {
			return cash.connectionAwaitHandshake.maybeRestartConnection(err)
		}

		opts := x509.VerifyOptions{
			Roots:         config.RootCAs,
			DNSName:       "", // disable server name checking
			Intermediates: x509.NewCertPool(),
		}
		certs := socket.ConnectionState().PeerCertificates
		for i, cert := range certs {
			if i == 0 {
				continue
			}
			opts.Intermediates.AddCert(cert)
		}
		if _, err := certs[0].Verify(opts); err != nil {
			return cash.connectionAwaitHandshake.maybeRestartConnection(err)
		}
	}

	helloFromServer := cash.makeHelloServerFromServer()
	if err := cash.send(server.SegToBytes(helloFromServer)); err != nil {
		return cash.connectionAwaitHandshake.maybeRestartConnection(err)
	}

	if seg, err := cash.readOne(); err == nil {
		hello := msgs.ReadRootHelloServerFromServer(seg)
		if cash.verifyTopology(&hello) {
			cash.remoteHost = hello.LocalHost()
			cash.remoteRMId = common.RMId(hello.RmId())

			if _, found := cash.topology.RMsRemoved()[cash.remoteRMId]; found {
				return false, cash.serverError(
					fmt.Errorf("%v has been removed from topology and may not rejoin.", cash.remoteRMId))
			}

			cash.remoteClusterUUId = hello.ClusterUUId()
			cash.remoteBootCount = hello.BootCount()
			cash.combinedTieBreak = cash.combinedTieBreak ^ hello.TieBreak()
			cash.nextState(nil)
			return false, nil
		} else {
			return cash.connectionAwaitHandshake.maybeRestartConnection(fmt.Errorf("Unequal remote topology"))
		}
	} else {
		return cash.connectionAwaitHandshake.maybeRestartConnection(err)
	}
}

func (cash *connectionAwaitServerHandshake) verifyTopology(remote *msgs.HelloServerFromServer) bool {
	if cash.topology.ClusterId == remote.ClusterId() {
		remoteUUId := remote.ClusterUUId()
		localUUId := cash.topology.ClusterUUId()
		if remoteUUId == 0 || localUUId == 0 {
			return true
		} else {
			return remoteUUId == localUUId
		}
	}
	return false
}

func (cash *connectionAwaitServerHandshake) makeHelloServerFromServer() *capn.Segment {
	seg := capn.NewBuffer(nil)
	hello := msgs.NewRootHelloServerFromServer(seg)
	localHost := cash.connectionManager.LocalHost()
	hello.SetLocalHost(localHost)
	hello.SetRmId(uint32(cash.connectionManager.RMId))
	hello.SetBootCount(cash.connectionManager.BootCount())
	tieBreak := cash.rng.Uint32()
	cash.combinedTieBreak = tieBreak
	hello.SetTieBreak(tieBreak)
	hello.SetClusterId(cash.topology.ClusterId)
	hello.SetClusterUUId(cash.topology.ClusterUUId())
	return seg
}

// Await Client Handshake

type connectionAwaitClientHandshake struct {
	*Connection
	peerCerts []*x509.Certificate
	roots     map[string]*common.Capability
	rootsVar  map[common.VarUUId]*common.Capability
}

func (cach *connectionAwaitClientHandshake) connectionStateMachineComponentWitness() {}
func (cach *connectionAwaitClientHandshake) String() string                          { return "ConnectionAwaitClientHandshake" }

func (cach *connectionAwaitClientHandshake) init(conn *Connection) {
	cach.Connection = conn
}

func (cach *connectionAwaitClientHandshake) start() (bool, error) {
	config := cach.commonTLSConfig()
	config.ClientAuth = tls.RequireAnyClientCert
	socket := tls.Server(cach.socket, config)
	cach.socket = socket
	if err := socket.Handshake(); err != nil {
		return false, err
	}

	if cach.topology.ClusterUUId() == 0 {
		return false, errors.New("Cluster not yet formed")
	} else if len(cach.topology.RootNames()) == 0 {
		return false, errors.New("No roots: cluster not yet formed")
	}

	peerCerts := socket.ConnectionState().PeerCertificates
	if authenticated, hashsum, roots := cach.verifyPeerCerts(peerCerts); authenticated {
		cach.peerCerts = peerCerts
		cach.roots = roots
		log.Printf("User '%s' authenticated", hex.EncodeToString(hashsum[:]))
		helloFromServer := cach.makeHelloClientFromServer()
		if err := cach.send(server.SegToBytes(helloFromServer)); err != nil {
			return false, err
		}
		cach.remoteHost = cach.socket.RemoteAddr().String()
		cach.nextState(nil)
		return false, nil
	} else {
		return false, errors.New("Client connection rejected: No client certificate known")
	}
}

func (cach *connectionAwaitClientHandshake) verifyPeerCerts(peerCerts []*x509.Certificate) (authenticated bool, hashsum [sha256.Size]byte, roots map[string]*common.Capability) {
	fingerprints := cach.topology.Fingerprints()
	for _, cert := range peerCerts {
		hashsum = sha256.Sum256(cert.Raw)
		if roots, found := fingerprints[hashsum]; found {
			return true, hashsum, roots
		}
	}
	return false, hashsum, nil
}

func (cach *connectionAwaitClientHandshake) makeHelloClientFromServer() *capn.Segment {
	seg := capn.NewBuffer(nil)
	hello := cmsgs.NewRootHelloClientFromServer(seg)
	namespace := make([]byte, common.KeyLen-8)
	binary.BigEndian.PutUint32(namespace[0:4], cach.ConnectionNumber)
	binary.BigEndian.PutUint32(namespace[4:8], cach.connectionManager.BootCount())
	binary.BigEndian.PutUint32(namespace[8:], uint32(cach.connectionManager.RMId))
	hello.SetNamespace(namespace)
	rootsCap := cmsgs.NewRootList(seg, len(cach.roots))
	idy := 0
	rootsVar := make(map[common.VarUUId]*common.Capability, len(cach.roots))
	for idx, name := range cach.topology.RootNames() {
		if capability, found := cach.roots[name]; found {
			rootCap := rootsCap.At(idy)
			idy++
			vUUId := cach.topology.Roots[idx].VarUUId
			rootCap.SetName(name)
			rootCap.SetVarId(vUUId[:])
			rootCap.SetCapability(capability.Capability)
			rootsVar[*vUUId] = capability
		}
	}
	hello.SetRoots(rootsCap)
	cach.rootsVar = rootsVar
	return seg
}

// Run

type connectionRun struct {
	*Connection
	beater        *connectionBeater
	reader        *connectionReader
	mustSendBeat  bool
	missingBeats  int
	beatBytes     []byte
	restart       bool
	submitterIdle *connectionMsgTopologyChanged
}

func (cr *connectionRun) connectionStateMachineComponentWitness() {}
func (cr *connectionRun) String() string                          { return "ConnectionRun" }

func (cr *connectionRun) init(conn *Connection) {
	cr.Connection = conn
}

func (cr *connectionRun) outcomeReceived(out connectionMsgOutcomeReceived) error {
	if cr.currentState != cr {
		return nil
	}
	err := cr.submitter.SubmissionOutcomeReceived(out.sender, out.txn, out.outcome)
	if cr.submitterIdle != nil && cr.submitter.IsIdle() {
		si := cr.submitterIdle
		cr.submitterIdle = nil
		server.Log("Connection", cr.Connection, "outcomeReceived", si, "(submitterIdle)")
		si.maybeClose()
	}
	return err
}

func (cr *connectionRun) start() (bool, error) {
	log.Printf("Connection established to %v (%v)\n", cr.remoteHost, cr.remoteRMId)

	cr.restart = true

	seg := capn.NewBuffer(nil)
	if cr.isClient {
		message := cmsgs.NewRootClientMessage(seg)
		message.SetHeartbeat()
	} else {
		message := msgs.NewRootMessage(seg)
		message.SetHeartbeat()
	}
	cr.beatBytes = server.SegToBytes(seg)

	if cr.isServer {
		flushSeg := capn.NewBuffer(nil)
		flushMsg := msgs.NewRootMessage(flushSeg)
		flushMsg.SetFlushed()
		flushBytes := server.SegToBytes(flushSeg)
		cr.connectionManager.ServerEstablished(cr.Connection, cr.remoteHost, cr.remoteRMId, cr.remoteBootCount, cr.combinedTieBreak, cr.remoteClusterUUId, func() { cr.Send(flushBytes) })
	}
	if cr.isClient {
		servers := cr.connectionManager.ClientEstablished(cr.ConnectionNumber, cr.Connection)
		if servers == nil {
			return false, errors.New("Not ready for client connections")
		}
		cr.submitter = client.NewClientTxnSubmitter(cr.connectionManager.RMId, cr.connectionManager.BootCount(), cr.rootsVar, cr.connectionManager)
		cr.submitter.TopologyChanged(cr.topology)
		cr.submitter.ServerConnectionsChanged(servers)
	}
	cr.mustSendBeat = true
	cr.missingBeats = 0

	cr.beater = newConnectionBeater(cr.Connection)
	go cr.beater.beat()

	cr.reader = newConnectionReader(cr.Connection)
	if cr.isClient {
		go cr.reader.readClient()
	} else {
		go cr.reader.readServer()
	}

	return false, nil
}

func (cr *connectionRun) topologyChanged(tc *connectionMsgTopologyChanged) error {
	if si := cr.submitterIdle; si != nil {
		cr.submitterIdle = nil
		server.Log("Connection", cr.Connection, "topologyChanged:", tc, "clearing old:", si)
		si.maybeClose()
	}
	topology := tc.topology
	cr.topology = topology
	if cr.currentState != cr {
		server.Log("Connection", cr.Connection, "topologyChanged", tc, "(not in cr)")
		tc.maybeClose()
		return nil
	}
	if cr.isClient {
		if topology != nil {
			if authenticated, _, roots := cr.verifyPeerCerts(cr.peerCerts); !authenticated {
				server.Log("Connection", cr.Connection, "topologyChanged", tc, "(client unauthed)")
				tc.maybeClose()
				return errors.New("Client connection closed: No client certificate known")
			} else if len(roots) == len(cr.roots) {
				for name, capsOld := range cr.roots {
					if capsNew, found := roots[name]; !found || !capsNew.Equal(capsOld) {
						server.Log("Connection", cr.Connection, "topologyChanged", tc, "(roots changed)")
						tc.maybeClose()
						return errors.New("Client connection closed: roots have changed")
					}
				}
			} else {
				server.Log("Connection", cr.Connection, "topologyChanged", tc, "(roots changed)")
				tc.maybeClose()
				return errors.New("Client connection closed: roots have changed")
			}
		}
		if err := cr.submitter.TopologyChanged(topology); err != nil {
			tc.maybeClose()
			return err
		}
		if cr.submitter.IsIdle() {
			server.Log("Connection", cr.Connection, "topologyChanged", tc, "(client, submitter is idle)")
			tc.maybeClose()
		} else {
			server.Log("Connection", cr.Connection, "topologyChanged", tc, "(client, submitter not idle)")
			cr.submitterIdle = tc
		}
	}
	if cr.isServer {
		server.Log("Connection", cr.Connection, "topologyChanged", tc, "(isServer)")
		tc.maybeClose()
		if topology != nil {
			if _, found := topology.RMsRemoved()[cr.remoteRMId]; found {
				cr.restart = false
			}
		}
	}
	return nil
}

func (cr *connectionRun) serverConnectionsChanged(servers map[common.RMId]paxos.Connection) error {
	if cr.submitter != nil {
		return cr.submitter.ServerConnectionsChanged(servers)
	}
	return nil
}

func (cr *connectionRun) handleMsgFromClient(msg cmsgs.ClientMessage) error {
	if cr.currentState != cr {
		// probably just draining the queue from the reader after a restart
		return nil
	}
	cr.missingBeats = 0
	switch which := msg.Which(); which {
	case cmsgs.CLIENTMESSAGE_HEARTBEAT:
		// do nothing
		return nil
	case cmsgs.CLIENTMESSAGE_CLIENTTXNSUBMISSION:
		ctxn := msg.ClientTxnSubmission()
		origTxnId := common.MakeTxnId(ctxn.Id())
		return cr.submitter.SubmitClientTransaction(&ctxn, func(clientOutcome *cmsgs.ClientTxnOutcome, err error) error {
			switch {
			case err != nil:
				return cr.clientTxnError(&ctxn, err, origTxnId)
			case clientOutcome == nil: // shutdown
				return nil
			default:
				seg := capn.NewBuffer(nil)
				msg := cmsgs.NewRootClientMessage(seg)
				msg.SetClientTxnOutcome(*clientOutcome)
				return cr.sendMessage(server.SegToBytes(msg.Segment))
			}
		})
	default:
		return cr.maybeRestartConnection(fmt.Errorf("Unexpected message type received from client: %v", which))
	}
}

func (cr *connectionRun) handleMsgFromServer(msg msgs.Message) error {
	if cr.currentState != cr {
		// probably just draining the queue from the reader after a restart
		return nil
	}
	cr.missingBeats = 0
	switch which := msg.Which(); which {
	case msgs.MESSAGE_HEARTBEAT:
		// do nothing
	case msgs.MESSAGE_CONNECTIONERROR:
		return fmt.Errorf("Error received from %v: \"%s\"", cr.remoteRMId, msg.ConnectionError())
	case msgs.MESSAGE_TOPOLOGYCHANGEREQUEST:
		configCap := msg.TopologyChangeRequest()
		config := configuration.ConfigurationFromCap(&configCap)
		cr.connectionManager.RequestConfigurationChange(config)
	default:
		cr.connectionManager.DispatchMessage(cr.remoteRMId, which, msg)
	}
	return nil
}

func (cr *connectionRun) clientTxnError(ctxn *cmsgs.ClientTxn, err error, origTxnId *common.TxnId) error {
	seg := capn.NewBuffer(nil)
	msg := cmsgs.NewRootClientMessage(seg)
	outcome := cmsgs.NewClientTxnOutcome(seg)
	msg.SetClientTxnOutcome(outcome)
	if origTxnId == nil {
		outcome.SetId(ctxn.Id())
	} else {
		outcome.SetId(origTxnId[:])
	}
	outcome.SetFinalId(ctxn.Id())
	outcome.SetError(err.Error())
	return cr.sendMessage(server.SegToBytes(seg))
}

func (cr *connectionRun) serverError(err error) error {
	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	msg.SetConnectionError(err.Error())
	cr.sendMessage(server.SegToBytes(seg))
	return err
}

func (cr *connectionRun) maybeRestartConnection(err error) error {
	switch {
	case err == nil || cr.currentState != cr:
		return nil

	case cr.isServer:
		log.Printf("Error on server connection to %v: %v", cr.remoteRMId, err)
		cr.connectionManager.ServerLost(cr.Connection, cr.remoteRMId, cr.restart)
		if cr.restart {
			cr.nextState(&cr.connectionDelay)
			return nil
		} else {
			return err
		}

	case cr.isClient:
		log.Printf("Error on client connection to %v: %v", cr.remoteHost, err)
		cr.connectionManager.ClientLost(cr.ConnectionNumber, cr.Connection)
		return err

	default:
		return err
	}
}

func (cr *connectionRun) sendMessage(msg []byte) error {
	if cr.currentState == cr {
		cr.mustSendBeat = false
		return cr.maybeRestartConnection(cr.send(msg))
	}
	return nil
}

func (cr *connectionRun) beat() error {
	if cr.currentState != cr {
		return nil
	}
	if cr.missingBeats == 2 {
		return cr.maybeRestartConnection(
			fmt.Errorf("Missed too many connection heartbeats. Restarting connection."))
	}
	// Useful for testing recovery from network brownouts
	/*
		if cr.rng.Intn(15) == 0 && cr.isServer {
			return cr.maybeRestartConnection(
				fmt.Errorf("Random death. Restarting connection."))
		}
	*/
	cr.missingBeats++
	if cr.mustSendBeat {
		return cr.maybeRestartConnection(cr.send(cr.beatBytes))
	} else {
		cr.mustSendBeat = true
	}
	return nil
}

func (cr *connectionRun) maybeStopBeater() {
	if cr.beater != nil {
		close(cr.beater.terminate)
		cr.beater.terminated.Wait()
		cr.beater = nil
	}
}

func (cr *connectionRun) maybeStopReaderAndCloseSocket() {
	if cr.reader != nil {
		close(cr.reader.terminate)
		cr.reader.terminated.Wait()
		cr.reader = nil
	}
	if cr.socket != nil {
		cr.socket.Close()
		cr.socket = nil
	}
}

// Beater

type connectionBeater struct {
	connectionMsgBasic
	*Connection
	terminate  chan struct{}
	terminated *sync.WaitGroup
	ticker     *time.Ticker
}

func newConnectionBeater(conn *Connection) *connectionBeater {
	wg := new(sync.WaitGroup)
	wg.Add(1)
	return &connectionBeater{
		Connection: conn,
		terminate:  make(chan struct{}),
		terminated: wg,
		ticker:     time.NewTicker(common.HeartbeatInterval),
	}
}

func (cb *connectionBeater) beat() {
	defer func() {
		cb.ticker.Stop()
		cb.ticker = nil
		cb.terminated.Done()
	}()
	for {
		select {
		case <-cb.terminate:
			return
		case <-cb.ticker.C:
			if !cb.enqueueQuery(cb) {
				return
			}
		}
	}
}

// Reader

type connectionReader struct {
	*Connection
	terminate  chan struct{}
	terminated *sync.WaitGroup
}

func newConnectionReader(conn *Connection) *connectionReader {
	wg := new(sync.WaitGroup)
	wg.Add(1)
	return &connectionReader{
		Connection: conn,
		terminate:  make(chan struct{}),
		terminated: wg,
	}
}

func (cr *connectionReader) readServer() {
	cr.read(func(seg *capn.Segment) bool {
		msg := msgs.ReadRootMessage(seg)
		return cr.enqueueQuery(connectionReadMessage(msg))
	})
}

func (cr *connectionReader) readClient() {
	cr.read(func(seg *capn.Segment) bool {
		msg := cmsgs.ReadRootClientMessage(seg)
		return cr.enqueueQuery(connectionReadClientMessage(msg))
	})
}

func (cr *connectionReader) read(fun func(*capn.Segment) bool) {
	defer cr.terminated.Done()
	for {
		select {
		case <-cr.terminate:
			return
		default:
			if seg, err := cr.readOne(); err == nil {
				if !fun(seg) {
					return
				}
			} else {
				cr.enqueueQuery(connectionReadError{error: err})
				return
			}
		}
	}
}

type connectionReadMessage msgs.Message

func (crm connectionReadMessage) witness() connectionMsg { return crm }

type connectionReadClientMessage cmsgs.ClientMessage

func (crcm connectionReadClientMessage) witness() connectionMsg { return crcm }

type connectionReadError struct {
	connectionMsgBasic
	error
}
