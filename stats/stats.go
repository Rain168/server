package stats

import (
	"bytes"
	"encoding/json"
	"errors"
	capn "github.com/glycerine/go-capnproto"
	"github.com/go-kit/kit/log"
	"goshawkdb.io/common"
	"goshawkdb.io/common/actor"
	cmsgs "goshawkdb.io/common/capnp"
	"goshawkdb.io/server"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/client"
	"goshawkdb.io/server/configuration"
	"goshawkdb.io/server/network"
	eng "goshawkdb.io/server/txnengine"
	"math/rand"
	"time"
)

type StatsPublisher struct {
	*actor.Mailbox
	*actor.BasicServerOuter

	localConnection   *client.LocalConnection
	connectionManager *network.ConnectionManager
	rng               *rand.Rand
	configPublisher

	inner statsPublisherInner
}

type statsPublisherInner struct {
	*StatsPublisher
	*actor.BasicServerInner
}

func NewStatsPublisher(cm *network.ConnectionManager, lc *client.LocalConnection, logger log.Logger) *StatsPublisher {
	sp := &StatsPublisher{
		localConnection:   lc,
		connectionManager: cm,
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	spi := &sp.inner
	spi.StatsPublisher = sp
	spi.BasicServerInner = actor.NewBasicServerInner(log.With(logger, "subsystem", "statsPublisher"))

	_, err := actor.Spawn(spi)
	if err != nil {
		panic(err) // impossible
	}

	return sp
}

func (sp *statsPublisherInner) Init(self *actor.Actor) (bool, error) {
	terminate, err := sp.BasicServerInner.Init(self)
	if terminate || err != nil {
		return terminate, err
	}

	sp.Mailbox = self.Mailbox
	sp.BasicServerOuter = actor.NewBasicServerOuter(self.Mailbox)

	sp.configPublisher.init(sp.StatsPublisher)
	return false, nil
}

type configPublisher struct {
	*StatsPublisher
	vsn        *common.TxnId
	publishing *configPublisherMsg
}

func (cp *configPublisher) init(sp *StatsPublisher) {
	cp.StatsPublisher = sp
	cp.vsn = common.VersionZero
	topology := cp.connectionManager.AddTopologySubscriber(eng.MiscSubscriber, cp)
	go cp.TopologyChanged(topology, func(bool) {})
}

type configPublisherMsgTopologyChanged struct {
	actor.MsgSyncQuery
	*configPublisher
	topology *configuration.Topology
}

func (msg *configPublisherMsgTopologyChanged) Exec() (bool, error) {
	msg.MustClose()

	msg.publishing = nil

	if msg.topology == nil || msg.topology.NextConfiguration != nil {
		// it's not safe to publish during topology changes.
		return false, nil
	}

	var root *configuration.Root
	for idx, rootName := range msg.topology.Roots {
		if rootName == server.ConfigRootName {
			root = &msg.topology.RootVarUUIds[idx]
			break
		}
	}
	if root == nil {
		return false, nil
	}
	json, err := msg.topology.ToJSONString()
	if err != nil {
		return false, err
	}

	msg.publishing = &configPublisherMsg{
		configPublisher: msg.configPublisher,
		root:            root,
		topology:        msg.topology,
		json:            json,
		backoff:         server.NewBinaryBackoffEngine(msg.rng, server.SubmissionMinSubmitDelay, server.SubmissionMaxSubmitDelay),
	}
	return msg.publishing.Exec()
}

func (cp *configPublisher) TopologyChanged(topology *configuration.Topology, done func(bool)) {
	msg := &configPublisherMsgTopologyChanged{configPublisher: cp, topology: topology}
	msg.InitMsg(cp)
	if cp.EnqueueMsg(msg) {
		go done(msg.Wait())
	} else {
		done(false)
	}
}

type configPublisherMsg struct {
	*configPublisher
	root     *configuration.Root
	topology *configuration.Topology
	json     []byte
	backoff  *server.BinaryBackoffEngine
}

func (msg *configPublisherMsg) Exec() (bool, error) {
	if msg.publishing != msg {
		return false, nil
	}

	seg := capn.NewBuffer(nil)
	ctxn := cmsgs.NewClientTxn(seg)
	ctxn.SetRetry(false)

	actions := cmsgs.NewClientActionList(seg, 1)

	action := actions.At(0)
	action.SetVarId(msg.root.VarUUId[:])
	action.SetReadwrite()
	rw := action.Readwrite()
	rw.SetVersion(msg.vsn[:])
	rw.SetValue(msg.json)
	rw.SetReferences(cmsgs.NewClientVarIdPosList(seg, 0))

	ctxn.SetActions(actions)

	varPosMap := make(map[common.VarUUId]*common.Positions)
	varPosMap[*msg.root.VarUUId] = msg.root.Positions

	server.DebugLog(msg.inner.Logger, "debug", "Publishing Config.", "config", string(msg.json))

	time.AfterFunc(2*time.Second, func() { msg.EnqueueMsg(msg) })

	go func() {
		_, result, err := msg.localConnection.RunClientTransaction(&ctxn, false, varPosMap, nil)
		msg.EnqueueFuncAsync(func() (bool, error) { return msg.execPart2(result, err) })
	}()

	return false, nil
}

func (msg *configPublisherMsg) execPart2(result *msgs.Outcome, err error) (bool, error) {
	if msg.publishing != msg {
		return false, nil
	}

	retryAfterDelay := err != nil || (result != nil && result.Abort().Which() == msgs.OUTCOMEABORT_RESUBMIT)
	if err != nil {
		// log, but ignore the error as it's most likely temporary. Then continue.
		msg.inner.Logger.Log("msg", "Error during config publish.", "error", err)
		err = nil
	}
	if result == nil { // shutdown
		msg.publishing = nil
		return false, nil
	} else if result.Which() == msgs.OUTCOME_COMMIT {
		msg.publishing = nil
		server.DebugLog(msg.inner.Logger, "debug", "Publishing Config committed.")
		return false, nil
	}

	if retryAfterDelay {
		server.DebugLog(msg.inner.Logger, "debug", "Publishing Config requires resubmit.")
		msg.backoff.Advance()
		msg.backoff.After(func() { msg.EnqueueMsg(msg) })
		return false, nil
	}

	server.DebugLog(msg.inner.Logger, "debug", "Publishing Config requires rerun.")
	updates := result.Abort().Rerun()
	found := false
	var value []byte
	for idx, l := 0, updates.Len(); idx < l && !found; idx++ {
		update := updates.At(idx)
		updateActions := eng.TxnActionsFromData(update.Actions(), true).Actions()
		for idy, m := 0, updateActions.Len(); idy < m && !found; idy++ {
			updateAction := updateActions.At(idy)
			if found = bytes.Equal(msg.root.VarUUId[:], updateAction.VarId()); found {
				if updateAction.Which() == msgs.ACTION_WRITE {
					msg.vsn = common.MakeTxnId(update.TxnId())
					updateWrite := updateAction.Write()
					value = updateWrite.Value()
				} else {
					// must be MISSING, which I'm really not sure should ever happen!
					msg.vsn = common.VersionZero
				}
			}
		}
	}
	if !found {
		msg.publishing = nil
		return false, errors.New("Internal error: failed to find update for rerun of config publishing")
	}
	if len(value) > 0 {
		inDB := new(configuration.ConfigurationJSON)
		if err := json.Unmarshal(value, inDB); err != nil {
			msg.publishing = nil
			return false, err
		} else if inDB.Version > msg.topology.Version {
			msg.publishing = nil
			server.DebugLog(msg.inner.Logger, "debug", "Existing copy in database is ahead of us. Nothing more to do.")
			return false, nil
		} else if inDB.Version == msg.topology.Version {
			msg.publishing = nil
			server.DebugLog(msg.inner.Logger, "debug", "Existing copy in database is at least as up to date as us. Nothing more to do.")
			return false, nil
		}
	}
	msg.EnqueueMsg(msg)
	return false, nil
}
