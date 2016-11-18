package paxos

import (
	"fmt"
	mdb "github.com/msackman/gomdb"
	mdbs "github.com/msackman/gomdb/server"
	"goshawkdb.io/common"
	"goshawkdb.io/server"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/db"
	"goshawkdb.io/server/dispatcher"
	eng "goshawkdb.io/server/txnengine"
	"log"
)

type AcceptorDispatcher struct {
	dispatcher.Dispatcher
	connectionManager ConnectionManager
	acceptormanagers  []*AcceptorManager
}

func NewAcceptorDispatcher(count uint8, rmId common.RMId, cm ConnectionManager, db *db.Databases) *AcceptorDispatcher {
	ad := &AcceptorDispatcher{
		acceptormanagers: make([]*AcceptorManager, count),
	}
	ad.Dispatcher.Init(count)
	for idx, exe := range ad.Executors {
		ad.acceptormanagers[idx] = NewAcceptorManager(rmId, exe, cm, db)
	}
	ad.loadFromDisk(db)
	return ad
}

func (ad *AcceptorDispatcher) OneATxnVotesReceived(sender common.RMId, oneATxnVotes *msgs.OneATxnVotes) {
	txnId := common.MakeTxnId(oneATxnVotes.TxnId())
	ad.withAcceptorManager(txnId, func(am *AcceptorManager) { am.OneATxnVotesReceived(sender, txnId, oneATxnVotes) })
}

func (ad *AcceptorDispatcher) TwoATxnVotesReceived(sender common.RMId, twoATxnVotes *msgs.TwoATxnVotes) {
	txn := eng.TxnReaderFromData(twoATxnVotes.Txn())
	txnId := txn.Id
	ad.withAcceptorManager(txnId, func(am *AcceptorManager) { am.TwoATxnVotesReceived(sender, txn, twoATxnVotes) })
}

func (ad *AcceptorDispatcher) TxnLocallyCompleteReceived(sender common.RMId, tlc *msgs.TxnLocallyComplete) {
	txnId := common.MakeTxnId(tlc.TxnId())
	ad.withAcceptorManager(txnId, func(am *AcceptorManager) { am.TxnLocallyCompleteReceived(sender, txnId, tlc) })
}

func (ad *AcceptorDispatcher) TxnSubmissionCompleteReceived(sender common.RMId, tsc *msgs.TxnSubmissionComplete) {
	txnId := common.MakeTxnId(tsc.TxnId())
	ad.withAcceptorManager(txnId, func(am *AcceptorManager) { am.TxnSubmissionCompleteReceived(sender, txnId, tsc) })
}

func (ad *AcceptorDispatcher) Status(sc *server.StatusConsumer) {
	sc.Emit("Acceptors")
	for idx, executor := range ad.Executors {
		s := sc.Fork()
		s.Emit(fmt.Sprintf("Acceptor Manager %v", idx))
		manager := ad.acceptormanagers[idx]
		executor.Enqueue(func() { manager.Status(s) })
	}
	sc.Join()
}

func (ad *AcceptorDispatcher) loadFromDisk(db *db.Databases) {
	res, err := db.ReadonlyTransaction(func(rtxn *mdbs.RTxn) interface{} {
		res, _ := rtxn.WithCursor(db.BallotOutcomes, func(cursor *mdbs.Cursor) interface{} {
			// cursor.Get returns a copy of the data. So it's fine for us
			// to store and process this later - it's not about to be
			// overwritten on disk.
			acceptorStates := make(map[*common.TxnId][]byte)
			txnIdData, acceptorState, err := cursor.Get(nil, nil, mdb.FIRST)
			for ; err == nil; txnIdData, acceptorState, err = cursor.Get(nil, nil, mdb.NEXT) {
				txnId := common.MakeTxnId(txnIdData)
				acceptorStates[txnId] = acceptorState
			}
			if err == mdb.NotFound {
				// fine, we just fell off the end as expected.
				return acceptorStates
			} else {
				cursor.Error(err)
				return nil
			}
		})
		return res
	}).ResultError()
	if err != nil {
		panic(fmt.Sprintf("AcceptorDispatcher error loading from disk: %v", err))
	} else if res != nil {
		acceptorStates := res.(map[*common.TxnId][]byte)
		for txnId, acceptorState := range acceptorStates {
			acceptorStateCopy := acceptorState
			txnIdCopy := txnId
			ad.withAcceptorManager(txnIdCopy, func(am *AcceptorManager) {
				if err := am.loadFromData(txnIdCopy, acceptorStateCopy); err != nil {
					log.Printf("AcceptorDispatcher error loading %v from disk: %v\n", txnIdCopy, err)
				}
			})
		}
		log.Printf("Loaded %v acceptors from disk\n", len(acceptorStates))
	}
}

func (ad *AcceptorDispatcher) withAcceptorManager(txnId *common.TxnId, fun func(*AcceptorManager)) bool {
	idx := uint8(txnId[server.MostRandomByteIndex]) % ad.ExecutorCount
	executor := ad.Executors[idx]
	manager := ad.acceptormanagers[idx]
	return executor.Enqueue(func() { fun(manager) })
}
