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
	"goshawkdb.io/common"
	cmsgs "goshawkdb.io/common/capnp"
	"goshawkdb.io/server"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/configuration"
	"log"
	"math/rand"
	"net"
	"time"
)

type Handshaker interface {
	PerformHandshake() (Protocol, error)
}

type Protocol interface {
	Run() error
}

type TLSCapnpHandshaker struct {
	remoteHost        string
	socket            net.Conn
	connectionManager *ConnectionManager
	rng               *rand.Rand
	topology          *configuration.Topology
}

type TLSCapnpServer struct {
	*TLSCapnpHandshaker
	remoteRMId        common.RMId
	remoteClusterUUId uint64
	remoteBootCount   uint32
	combinedTieBreak  uint32
}

type TLSCapnpClient struct {
	*TLSCapnpHandshaker
	peerCerts        []*x509.Certificate
	roots            map[string]*common.Capability
	rootsVar         map[common.VarUUId]*common.Capability
	ConnectionNumber uint32
}

type WebsocketClient struct {
}

func (tch *TLSCapnpHandshaker) makeHello() *capn.Segment {
	seg := capn.NewBuffer(nil)
	hello := cmsgs.NewRootHello(seg)
	hello.SetProduct(common.ProductName)
	hello.SetVersion(common.ProductVersion)
	hello.SetIsClient(false)
	return seg
}

func (tch *TLSCapnpHandshaker) verifyHello(hello *cmsgs.Hello) bool {
	return hello.Product() == common.ProductName &&
		hello.Version() == common.ProductVersion
}

func (tch *TLSCapnpHandshaker) send(msg []byte) error {
	l := len(msg)
	for l > 0 {
		switch w, err := tch.socket.Write(msg); {
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

func (tch *TLSCapnpHandshaker) readOne() (*capn.Segment, error) {
	return capn.ReadFromStream(tch.socket, nil)
}

func (tch *TLSCapnpHandshaker) PerformHandshake() (Protocol, error) {
	helloSeg := tch.makeHello()
	if err := tch.send(server.SegToBytes(helloSeg)); err != nil {
		return nil, err
	}

	if seg, err := tch.readOne(); err == nil {
		hello := cmsgs.ReadRootHello(seg)
		if tch.verifyHello(&hello) {
			if hello.IsClient() {
				tcc := tch.newTLSCapnpClient()
				if err = tcc.finishHandshake(); err == nil {
					return tcc, nil
				} else {
					return nil, err
				}

			} else {
				tcs := tch.newTLSCapnpServer()
				if err = tcs.finishHandshake(); err == nil {
					return tcs, nil
				} else {
					return nil, err
				}
			}

		} else {
			product := hello.Product()
			if l := len(common.ProductName); len(product) > l {
				product = product[:l] + "..."
			}
			version := hello.Version()
			if l := len(common.ProductVersion); len(version) > l {
				version = version[:l] + "..."
			}
			return nil, fmt.Errorf("Received erroneous hello from peer: received product name '%s' (expected '%s'), product version '%s' (expected '%s')",
				product, common.ProductName, version, common.ProductVersion)
		}
	} else {
		return nil, err
	}
}

func (tch *TLSCapnpHandshaker) newTLSCapnpClient() *TLSCapnpClient {
	return &TLSCapnpClient{
		TLSCapnpHandshaker: tch,
	}
}

func (tch *TLSCapnpHandshaker) newTLSCapnpServer() *TLSCapnpServer {
	return &TLSCapnpServer{
		TLSCapnpHandshaker: tch,
	}
}

func (tch *TLSCapnpHandshaker) serverError(err error) error {
	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	msg.SetConnectionError(err.Error())
	tch.send(server.SegToBytes(seg))
	return err
}

func (tch *TLSCapnpHandshaker) baseTLSConfig() *tls.Config {
	nodeCertPrivKeyPair := tch.connectionManager.NodeCertificatePrivateKeyPair
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

func (tcs *TLSCapnpServer) finishHandshake() error {
	// TLS seems to require us to pick one end as the client and one
	// end as the server even though in a server-server connection we
	// really don't care which is which.
	config := tcs.baseTLSConfig()
	if len(tcs.remoteHost) == 0 {
		// We came from the listener, so we're going to act as the server.
		config.ClientAuth = tls.RequireAndVerifyClientCert
		socket := tls.Server(tcs.socket, config)
		if err := socket.SetDeadline(time.Time{}); err != nil {
			return err
		}
		tcs.socket = socket

	} else {
		config.InsecureSkipVerify = true
		socket := tls.Client(tcs.socket, config)
		if err := socket.SetDeadline(time.Time{}); err != nil {
			return err
		}
		tcs.socket = socket

		// This is nuts: as a server, we can demand the client cert and
		// verify that without any concept of a client name. But as the
		// client, if we don't have a server name, then we have to do
		// the verification ourself. Why is TLS asymmetric?!

		if err := socket.Handshake(); err != nil {
			return err
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
			return err
		}
	}

	hello := tcs.makeHelloServer()
	if err := tcs.send(server.SegToBytes(hello)); err != nil {
		return err
	}

	if seg, err := tcs.readOne(); err == nil {
		hello := msgs.ReadRootHelloServerFromServer(seg)
		tcs.remoteHost = hello.LocalHost()
		tcs.remoteRMId = common.RMId(hello.RmId())
		if tcs.verifyTopology(&hello) {
			if _, found := tcs.topology.RMsRemoved[tcs.remoteRMId]; found {
				return tcs.serverError(
					fmt.Errorf("%v has been removed from topology and may not rejoin.", tcs.remoteRMId))
			}

			tcs.remoteClusterUUId = hello.ClusterUUId()
			tcs.remoteBootCount = hello.BootCount()
			tcs.combinedTieBreak = tcs.combinedTieBreak ^ hello.TieBreak()
			return nil
		} else {
			return fmt.Errorf("Unequal remote topology (%v, %v)", tcs.remoteHost, tcs.remoteRMId)
		}
	} else {
		return err
	}
}

func (tcs *TLSCapnpServer) makeHelloServer() *capn.Segment {
	seg := capn.NewBuffer(nil)
	hello := msgs.NewRootHelloServerFromServer(seg)
	localHost := tcs.connectionManager.LocalHost()
	hello.SetLocalHost(localHost)
	hello.SetRmId(uint32(tcs.connectionManager.RMId))
	hello.SetBootCount(tcs.connectionManager.BootCount())
	tieBreak := tcs.rng.Uint32()
	tcs.combinedTieBreak = tieBreak
	hello.SetTieBreak(tieBreak)
	hello.SetClusterId(tcs.topology.ClusterId)
	hello.SetClusterUUId(tcs.topology.ClusterUUId)
	return seg
}

func (tcs *TLSCapnpServer) verifyTopology(remote *msgs.HelloServerFromServer) bool {
	if tcs.topology.ClusterId == remote.ClusterId() {
		remoteUUId := remote.ClusterUUId()
		localUUId := tcs.topology.ClusterUUId
		return remoteUUId == 0 || localUUId == 0 || remoteUUId == localUUId
	}
	return false
}

func (tcs *TLSCapnpServer) Run() error {
	return nil
}

func (tcc *TLSCapnpClient) finishHandshake() error {
	config := tcc.baseTLSConfig()
	config.ClientAuth = tls.RequireAnyClientCert
	socket := tls.Server(tcc.socket, config)
	tcc.socket = socket
	if err := socket.Handshake(); err != nil {
		return err
	}

	if tcc.topology.ClusterUUId == 0 {
		return errors.New("Cluster not yet formed")
	} else if len(tcc.topology.Roots) == 0 {
		return errors.New("No roots: cluster not yet formed")
	}

	peerCerts := socket.ConnectionState().PeerCertificates
	if authenticated, hashsum, roots := tcc.verifyPeerCerts(peerCerts); authenticated {
		tcc.peerCerts = peerCerts
		tcc.roots = roots
		log.Printf("User '%s' authenticated", hex.EncodeToString(hashsum[:]))
		helloFromServer := tcc.makeHelloClient()
		if err := tcc.send(server.SegToBytes(helloFromServer)); err != nil {
			return err
		}
		tcc.remoteHost = tcc.socket.RemoteAddr().String()
		return nil
	} else {
		return errors.New("Client connection rejected: No client certificate known")
	}
}

func (tcc *TLSCapnpClient) verifyPeerCerts(peerCerts []*x509.Certificate) (authenticated bool, hashsum [sha256.Size]byte, roots map[string]*common.Capability) {
	fingerprints := tcc.topology.Fingerprints
	for _, cert := range peerCerts {
		hashsum = sha256.Sum256(cert.Raw)
		if roots, found := fingerprints[hashsum]; found {
			return true, hashsum, roots
		}
	}
	return false, hashsum, nil
}

func (tcc *TLSCapnpClient) makeHelloClient() *capn.Segment {
	seg := capn.NewBuffer(nil)
	hello := cmsgs.NewRootHelloClientFromServer(seg)
	namespace := make([]byte, common.KeyLen-8)
	binary.BigEndian.PutUint32(namespace[0:4], tcc.ConnectionNumber)
	binary.BigEndian.PutUint32(namespace[4:8], tcc.connectionManager.BootCount())
	binary.BigEndian.PutUint32(namespace[8:], uint32(tcc.connectionManager.RMId))
	hello.SetNamespace(namespace)
	rootsCap := cmsgs.NewRootList(seg, len(tcc.roots))
	idy := 0
	rootsVar := make(map[common.VarUUId]*common.Capability, len(tcc.roots))
	for idx, name := range tcc.topology.Roots {
		if capability, found := tcc.roots[name]; found {
			rootCap := rootsCap.At(idy)
			idy++
			vUUId := tcc.topology.RootVarUUIds[idx].VarUUId
			rootCap.SetName(name)
			rootCap.SetVarId(vUUId[:])
			rootCap.SetCapability(capability.Capability)
			rootsVar[*vUUId] = capability
		}
	}
	hello.SetRoots(rootsCap)
	tcc.rootsVar = rootsVar
	return seg
}

func (tcc *TLSCapnpClient) Run() error {
	return nil
}

func (wc *WebsocketClient) PerformHandshake() (*WebsocketClient, error) {
	return wc, nil
}

func (wc *WebsocketClient) Run() error {
	return nil
}
