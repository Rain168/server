package paxos

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"github.com/go-kit/kit/log"
	mdbs "github.com/msackman/gomdb/server"
	"goshawkdb.io/common"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/configuration"
	"goshawkdb.io/server/types"
	sconn "goshawkdb.io/server/types/connections/server"
	"goshawkdb.io/server/utils"
	"goshawkdb.io/server/utils/senders"
	"goshawkdb.io/server/utils/status"
	"goshawkdb.io/server/utils/txnreader"
	"time"
)

type Acceptor struct {
	logger          log.Logger
	txnId           *common.TxnId
	acceptorManager *AcceptorManager
	birthday        time.Time
	createdFromDisk bool
	currentState    acceptorStateMachineComponent
	acceptorReceiveBallots
	acceptorWriteToDisk
	acceptorAwaitLocallyComplete
	acceptorDeleteFromDisk
}

func NewAcceptor(txn *txnreader.TxnReader, am *AcceptorManager) *Acceptor {
	a := &Acceptor{
		txnId:           txn.Id,
		acceptorManager: am,
		birthday:        time.Now(),
	}
	a.init(txn)
	return a
}

func AcceptorFromData(txnId *common.TxnId, outcome *msgs.Outcome, sendToAll bool, instances *msgs.InstancesForVar_List, am *AcceptorManager) *Acceptor {
	outcomeEqualId := (*outcomeEqualId)(outcome)
	txn := txnreader.TxnReaderFromData(outcome.Txn())
	a := NewAcceptor(txn, am)
	a.ballotAccumulator = BallotAccumulatorFromData(txn, outcomeEqualId, instances, a)
	a.outcome = outcomeEqualId
	a.sendToAll = sendToAll
	a.sendToAllOnDisk = sendToAll
	a.outcomeOnDisk = outcomeEqualId
	a.createdFromDisk = true
	return a
}

func (a *Acceptor) Log(keyvals ...interface{}) error {
	if a.logger == nil {
		a.logger = log.With(a.acceptorManager.logger, "TxnId", a.txnId)
	}
	return a.logger.Log(keyvals...)
}

func (a *Acceptor) init(txn *txnreader.TxnReader) {
	a.acceptorReceiveBallots.init(a, txn)
	a.acceptorWriteToDisk.init(a, txn)
	a.acceptorAwaitLocallyComplete.init(a, txn)
	a.acceptorDeleteFromDisk.init(a, txn)
}

func (a *Acceptor) Start() {
	if a.currentState != nil {
		return
	}
	if a.outcomeOnDisk == nil {
		a.currentState = &a.acceptorReceiveBallots
	} else {
		a.currentState = &a.acceptorAwaitLocallyComplete
	}
	a.currentState.start()
}

func (a *Acceptor) Status(sc *status.StatusConsumer) {
	sc.Emit(fmt.Sprintf("Acceptor for %v", a.txnId))
	sc.Emit(fmt.Sprintf("- Born: %v", a.birthday))
	sc.Emit(fmt.Sprintf("- Created from disk: %v", a.createdFromDisk))
	sc.Emit(fmt.Sprintf("- Current State: %v", a.currentState))
	sc.Emit(fmt.Sprintf("- Outcome determined? %v", a.outcome != nil))
	sc.Emit(fmt.Sprintf("- Pending TLC: %v", a.pendingTLC))
	sc.Emit(fmt.Sprintf("- Received TLC: %v", a.tlcsReceived))
	sc.Emit(fmt.Sprintf("- Received TSC: %v", a.tscReceived))
	a.ballotAccumulator.Status(sc.Fork())
	sc.Join()
}

func (a *Acceptor) nextState(requestedState acceptorStateMachineComponent) {
	if requestedState == nil {
		switch a.currentState {
		case &a.acceptorReceiveBallots:
			a.currentState = &a.acceptorWriteToDisk
		case &a.acceptorWriteToDisk:
			a.currentState = &a.acceptorAwaitLocallyComplete
		case &a.acceptorAwaitLocallyComplete:
			a.currentState = &a.acceptorDeleteFromDisk
		case &a.acceptorDeleteFromDisk:
			a.currentState = nil
			return
		}

	} else {
		a.currentState = requestedState
	}

	a.currentState.start()
}

type acceptorStateMachineComponent interface {
	init(*Acceptor, *txnreader.TxnReader)
	start()
	acceptorStateMachineComponentWitness()
}

// receive ballots

type acceptorReceiveBallots struct {
	*Acceptor
	ballotAccumulator     *BallotAccumulator
	outcome               *outcomeEqualId
	txn                   *txnreader.TxnReader
	txnSubmitter          common.RMId
	txnSubmitterBootCount uint32
	txnSender             *senders.RepeatingSender
}

func (arb *acceptorReceiveBallots) init(a *Acceptor, txn *txnreader.TxnReader) {
	arb.Acceptor = a
	arb.ballotAccumulator = NewBallotAccumulator(txn, arb.Acceptor)
	arb.txn = txn
	arb.txnSubmitter = txn.Id.RMId(a.acceptorManager.RMId)
	arb.txnSubmitterBootCount = txn.Id.BootCount()
}

func (arb *acceptorReceiveBallots) start() {
	// We need to watch to see if the submitter dies. If it does, there
	// is a chance that we might be the only remaining record of this
	// txn and so we need to ensure progress somehow. To see how this
	// happens, consider the following scenario:
	//
	// 1. Provided the submitter stays up, its repeating sender will
	// make sure that the txn gets to all proposers, and progress
	// continues to be made.
	//
	// 2. But consider what happens if the submitter and a proposer are
	// on the same node which fails: That proposer has local votes and
	// has sent those votes to us, so we now contain state. But that
	// node now goes down. The txn never made it to any other node (we
	// must be an acceptor, and a learner), so when the node comes back
	// up, there is no record of it anywhere, other than in any such
	// acceptor.
	//
	// Once we've gone to disk, we will then have a repeating 2B sender
	// which will ensure progress, so we have no risk once we've
	// started going to disk.
	//
	// However, if we are a learner, then we cannot start an abort
	// proposer as we're not allowed to vote. So our response in this
	// scenario is actually to start a repeating sender of the txn
	// itself to the other active RMs, thus taking the role of the
	// submitter.
	arb.acceptorManager.AddServerConnectionSubscriber(arb)
}

func (arb *acceptorReceiveBallots) acceptorStateMachineComponentWitness() {}
func (arb *acceptorReceiveBallots) String() string {
	return "acceptorReceiveBallots"
}

func (arb *acceptorReceiveBallots) BallotAccepted(instanceRMId common.RMId, inst *instance, vUUId *common.VarUUId, txn *txnreader.TxnReader) {
	// We can accept a ballot from instanceRMId at any point up until
	// we've received a TLC from instanceRMId (see notes in ALC re
	// retry). Note an acceptor can change it's mind!
	if arb.currentState == &arb.acceptorDeleteFromDisk {
		arb.Log("error", "Received ballot after all TLCs have been received.", "instanceRMId", instanceRMId)
	}
	outcome := arb.ballotAccumulator.BallotReceived(instanceRMId, inst, vUUId, txn)
	if outcome != nil && !outcome.Equal(arb.outcome) {
		arb.outcome = outcome
		arb.nextState(&arb.acceptorWriteToDisk)
	}
}

func (arb *acceptorReceiveBallots) ConnectedRMs(conns map[common.RMId]*sconn.ServerConnection) {
	if conn, found := conns[arb.txnSubmitter]; !found || (conn.BootCount != arb.txnSubmitterBootCount && arb.txnSubmitterBootCount > 0) {
		arb.enqueueCreateTxnSender()
	}
}
func (arb *acceptorReceiveBallots) ConnectionLost(rmId common.RMId, conns map[common.RMId]*sconn.ServerConnection) {
	if rmId == arb.txnSubmitter {
		arb.enqueueCreateTxnSender()
	}
}
func (arb *acceptorReceiveBallots) ConnectionEstablished(conn *sconn.ServerConnection, conns map[common.RMId]*sconn.ServerConnection, done func()) {
	if conn.RMId == arb.txnSubmitter && conn.BootCount != arb.txnSubmitterBootCount && arb.txnSubmitterBootCount > 0 {
		arb.enqueueCreateTxnSender()
	}
	done()
}

func (arb *acceptorReceiveBallots) enqueueCreateTxnSender() {
	arb.acceptorManager.Exe.EnqueueFuncAsync(arb.createTxnSender)
}

func (arb *acceptorReceiveBallots) createTxnSender() (bool, error) {
	if arb.currentState == arb && arb.txnSender == nil {
		arb.acceptorManager.RemoveServerConnectionSubscriber(arb)
		seg := capn.NewBuffer(nil)
		msg := msgs.NewRootMessage(seg)
		msg.SetTxnSubmission(arb.txn.Data)
		activeRMs := make([]common.RMId, 0, arb.txn.Txn.TwoFInc())
		allocs := arb.txn.Txn.Allocations()
		for idx := 0; idx < allocs.Len(); idx++ {
			alloc := allocs.At(idx)
			if alloc.Active() == 0 {
				break
			} else {
				activeRMs = append(activeRMs, common.RMId(alloc.RmId()))
			}
		}
		utils.DebugLog(arb, "debug", "Starting extra txn sender.", "actives", activeRMs)
		arb.txnSender = senders.NewRepeatingSender(common.SegToBytes(seg), activeRMs...)
		arb.acceptorManager.AddServerConnectionSubscriber(arb.txnSender)
	}
	return false, nil
}

// write to disk

type acceptorWriteToDisk struct {
	*Acceptor
	outcomeOnDisk   *outcomeEqualId
	sendToAll       bool
	sendToAllOnDisk bool
}

func (awtd *acceptorWriteToDisk) init(a *Acceptor, txn *txnreader.TxnReader) {
	awtd.Acceptor = a
}

func (awtd *acceptorWriteToDisk) start() {
	awtd.acceptorManager.RemoveServerConnectionSubscriber(&awtd.acceptorReceiveBallots)
	if awtd.txnSender != nil {
		awtd.acceptorManager.RemoveServerConnectionSubscriber(awtd.txnSender)
	}
	outcome := awtd.outcome
	outcomeCap := (*msgs.Outcome)(outcome)
	awtd.sendToAll = awtd.sendToAll || outcomeCap.Which() == msgs.OUTCOME_COMMIT
	sendToAll := awtd.sendToAll
	stateSeg := capn.NewBuffer(nil)
	state := msgs.NewRootAcceptorState(stateSeg)
	state.SetOutcome(*outcomeCap)
	state.SetSendToAll(awtd.sendToAll)
	state.SetInstances(awtd.ballotAccumulator.AddInstancesToSeg(stateSeg))

	data := common.SegToBytes(stateSeg)

	// to ensure correct order of writes, schedule the write from
	// the current go-routine...
	utils.DebugLog(awtd, "debug", "Writing 2B to disk...")
	future := awtd.acceptorManager.DB.ReadWriteTransaction(func(rwtxn *mdbs.RWTxn) interface{} {
		rwtxn.Put(awtd.acceptorManager.DB.BallotOutcomes, awtd.txnId[:], data, 0)
		return true
	})
	go func() {
		// ... but process the result in a new go-routine to avoid blocking the executor.
		if ran, err := future.ResultError(); err != nil {
			panic(fmt.Sprintf("Error: %v Acceptor Write error: %v", awtd.txnId, err))
		} else if ran != nil {
			utils.DebugLog(awtd, "debug", "Writing 2B to disk...done.")
			awtd.acceptorManager.Exe.EnqueueFuncAsync(func() (bool, error) {
				awtd.writeDone(outcome, sendToAll)
				return false, nil
			})
		}
	}()
}

func (awtd *acceptorWriteToDisk) acceptorStateMachineComponentWitness() {}
func (awtd *acceptorWriteToDisk) String() string {
	return "acceptorWriteToDisk"
}

func (awtd *acceptorWriteToDisk) writeDone(outcome *outcomeEqualId, sendToAll bool) {
	// There could have been a number a outcomes determined in quick
	// succession. We only "won" if we got here and our outcome is
	// still the right one.
	if awtd.outcome == outcome && awtd.currentState == awtd {
		awtd.outcomeOnDisk = outcome
		awtd.sendToAllOnDisk = sendToAll
		awtd.nextState(nil)
	}
}

// await locally complete

type acceptorAwaitLocallyComplete struct {
	*Acceptor
	pendingTLC    map[common.RMId]types.EmptyStruct
	tlcsReceived  map[common.RMId]types.EmptyStruct
	tgcRecipients common.RMIds
	tscReceived   bool
	twoBSender    *twoBTxnVotesSender
	txnSubmitter  common.RMId
}

func (aalc *acceptorAwaitLocallyComplete) init(a *Acceptor, txn *txnreader.TxnReader) {
	aalc.Acceptor = a
	aalc.tlcsReceived = make(map[common.RMId]types.EmptyStruct, aalc.ballotAccumulator.txn.Txn.Allocations().Len())
	aalc.txnSubmitter = txn.Id.RMId(a.acceptorManager.RMId)
}

func (aalc *acceptorAwaitLocallyComplete) start() {
	if aalc.twoBSender != nil {
		aalc.acceptorManager.RemoveServerConnectionSubscriber(aalc.twoBSender)
		aalc.twoBSender = nil
	}

	// If our outcome changes, it may look here like we're throwing
	// away TLCs received from proposers/learners. However,
	// proposers/learners wait until all acceptors have given the same
	// answer before issuing any TLCs, so if we are here, we cannot
	// have received any TLCs from anyone... unless we're a retry!  If
	// the txn is a retry then proposers start as soon as they have any
	// ballot, and the ballot accumulator will return a result
	// immediately. However, other ballots can continue to arrive even
	// after a proposer has received F+1 equal outcomes from
	// acceptors. In that case, the acceptor can be here, waiting for
	// TLCs, and can even have received some TLCs when it now receives
	// another ballot. It cannot ignore that ballot because to do so
	// opens the possibility that the acceptors do not arrive at the
	// same outcome and the txn will block.

	allocs := aalc.ballotAccumulator.txn.Txn.Allocations()
	aalc.pendingTLC = make(map[common.RMId]types.EmptyStruct, allocs.Len())
	aalc.tgcRecipients = make([]common.RMId, 0, allocs.Len())

	var rmsRemoved map[common.RMId]types.EmptyStruct
	if aalc.acceptorManager.Topology != nil {
		rmsRemoved = aalc.acceptorManager.Topology.RMsRemoved
	}

	for idx, l := 0, allocs.Len(); idx < l; idx++ {
		alloc := allocs.At(idx)
		active := alloc.Active() != 0
		rmId := common.RMId(alloc.RmId())
		if _, found := rmsRemoved[rmId]; found {
			continue
		}
		if aalc.sendToAllOnDisk || active {
			if _, found := aalc.tlcsReceived[rmId]; !found {
				aalc.pendingTLC[rmId] = types.EmptyStructVal
			}
			aalc.tgcRecipients = append(aalc.tgcRecipients, rmId)
		}
	}

	if _, found := rmsRemoved[aalc.txnSubmitter]; found {
		aalc.tscReceived = true
	}

	if len(aalc.pendingTLC) == 0 && aalc.tscReceived {
		aalc.maybeDelete()

	} else {
		utils.DebugLog(aalc, "debug", "Adding sender for 2B.")
		aalc.twoBSender = newTwoBTxnVotesSender(aalc, (*msgs.Outcome)(aalc.outcomeOnDisk), aalc.txnId, aalc.txnSubmitter, aalc.tgcRecipients...)
		aalc.acceptorManager.AddServerConnectionSubscriber(aalc.twoBSender)
	}
}

func (aalc *acceptorAwaitLocallyComplete) acceptorStateMachineComponentWitness() {}
func (aalc *acceptorAwaitLocallyComplete) String() string {
	return "acceptorAwaitLocallyComplete"
}

func (aalc *acceptorAwaitLocallyComplete) TxnLocallyCompleteReceived(sender common.RMId) {
	aalc.tlcsReceived[sender] = types.EmptyStructVal
	if aalc.currentState == aalc {
		delete(aalc.pendingTLC, sender)
		aalc.maybeDelete()
	}
}

func (aalc *acceptorAwaitLocallyComplete) TxnSubmissionCompleteReceived(sender common.RMId) {
	// Submitter will issues TSCs after FInc outcomes so we can receive this early, which is fine.
	if !aalc.tscReceived {
		aalc.tscReceived = true
		aalc.maybeDelete()
	}
}

func (aalc *acceptorAwaitLocallyComplete) TopologyChanged(topology *configuration.Topology) {
	if topology == nil {
		return
	}
	rmsRemoved := topology.RMsRemoved
	if _, found := rmsRemoved[aalc.acceptorManager.RMId]; found {
		return
	}
	for idx := 0; idx < len(aalc.tgcRecipients); idx++ {
		if _, found := rmsRemoved[aalc.tgcRecipients[idx]]; found {
			aalc.tgcRecipients = append(aalc.tgcRecipients[:idx], aalc.tgcRecipients[idx+1:]...)
			idx--
		}
	}
	for rmId := range rmsRemoved {
		aalc.TxnLocallyCompleteReceived(rmId)
	}
	if _, found := rmsRemoved[aalc.txnSubmitter]; found {
		aalc.TxnSubmissionCompleteReceived(aalc.txnSubmitter)
	}
}

func (aalc *acceptorAwaitLocallyComplete) maybeDelete() {
	if aalc.currentState == aalc && aalc.tscReceived && len(aalc.pendingTLC) == 0 {
		aalc.nextState(nil)
	}
}

// delete from disk

type acceptorDeleteFromDisk struct {
	*Acceptor
}

func (adfd *acceptorDeleteFromDisk) init(a *Acceptor, txn *txnreader.TxnReader) {
	adfd.Acceptor = a
}

func (adfd *acceptorDeleteFromDisk) start() {
	if adfd.twoBSender != nil {
		adfd.acceptorManager.RemoveServerConnectionSubscriber(adfd.twoBSender)
		adfd.twoBSender = nil
	}
	utils.DebugLog(adfd, "debug", "Deleting 2B from disk...")
	future := adfd.acceptorManager.DB.ReadWriteTransaction(func(rwtxn *mdbs.RWTxn) interface{} {
		rwtxn.Del(adfd.acceptorManager.DB.BallotOutcomes, adfd.txnId[:], nil)
		return true
	})
	go func() {
		if ran, err := future.ResultError(); err != nil {
			panic(fmt.Sprintf("Error: %v Acceptor Deletion error: %v", adfd.txnId, err))
		} else if ran != nil {
			utils.DebugLog(adfd, "debug", "Deleting 2B from disk...done.")
			adfd.acceptorManager.Exe.EnqueueFuncAsync(adfd.deletionDone)
		}
	}()
}

func (adfd *acceptorDeleteFromDisk) acceptorStateMachineComponentWitness() {}
func (adfd *acceptorDeleteFromDisk) String() string {
	return "acceptorDeleteFromDisk"
}

func (adfd *acceptorDeleteFromDisk) deletionDone() (bool, error) {
	if adfd.currentState == adfd {
		adfd.nextState(nil)
		adfd.acceptorManager.AcceptorFinished(adfd.txnId)

		seg := capn.NewBuffer(nil)
		msg := msgs.NewRootMessage(seg)
		tgc := msgs.NewTxnGloballyComplete(seg)
		msg.SetTxnGloballyComplete(tgc)
		tgc.SetTxnId(adfd.txnId[:])
		utils.DebugLog(adfd, "debug", "Sending TGC.", "destination", adfd.tgcRecipients)
		// If this gets lost it doesn't matter - the TLC will eventually
		// get resent and we'll then send out another TGC.
		senders.NewOneShotSender(adfd.logger, common.SegToBytes(seg), adfd.acceptorManager, adfd.tgcRecipients...)
	}
	return false, nil
}

// 2B Sender

type twoBTxnVotesSender struct {
	msg          []byte
	recipients   []common.RMId
	submitterMsg []byte
	submitter    common.RMId
}

func newTwoBTxnVotesSender(logger log.Logger, outcome *msgs.Outcome, txnId *common.TxnId, submitter common.RMId, recipients ...common.RMId) *twoBTxnVotesSender {
	submitterSeg := capn.NewBuffer(nil)
	submitterMsg := msgs.NewRootMessage(submitterSeg)
	submitterMsg.SetSubmissionOutcome(*outcome)

	if outcome.Which() == msgs.OUTCOME_ABORT {
		abort := outcome.Abort()
		abort.SetResubmit() // nuke out the updates as proposers don't need them.
	}

	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	twoB := msgs.NewTwoBTxnVotes(seg)
	msg.SetTwoBTxnVotes(twoB)
	twoB.SetOutcome(*outcome)

	utils.DebugLog(logger, "debug", "Sending 2B.", "recipients", recipients, "submitter", submitter)

	return &twoBTxnVotesSender{
		msg:          common.SegToBytes(seg),
		recipients:   recipients,
		submitterMsg: common.SegToBytes(submitterSeg),
		submitter:    submitter,
	}
}

func (s *twoBTxnVotesSender) ConnectedRMs(conns map[common.RMId]*sconn.ServerConnection) {
	for _, rmId := range s.recipients {
		if conn, found := conns[rmId]; found {
			conn.Send(s.msg)
		}
	}
	if conn, found := conns[s.submitter]; found {
		conn.Send(s.submitterMsg)
	}
}

func (s *twoBTxnVotesSender) ConnectionLost(common.RMId, map[common.RMId]*sconn.ServerConnection) {}

func (s *twoBTxnVotesSender) ConnectionEstablished(conn *sconn.ServerConnection, conns map[common.RMId]*sconn.ServerConnection, done func()) {
	defer done()
	for _, recipient := range s.recipients {
		if recipient == conn.RMId {
			conn.Send(s.msg)
			break
		}
	}
	if s.submitter == conn.RMId {
		conn.Send(s.submitterMsg)
	}
}
