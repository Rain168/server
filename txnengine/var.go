package txnengine

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	mdbs "github.com/msackman/gomdb/server"
	"goshawkdb.io/common"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/db"
	"goshawkdb.io/server/dispatcher"
	"goshawkdb.io/server/types"
	"goshawkdb.io/server/utils"
	"goshawkdb.io/server/utils/poisson"
	"goshawkdb.io/server/utils/status"
	vc "goshawkdb.io/server/utils/vectorclock"
	"math/rand"
	"time"
)

type Var struct {
	UUId            *common.VarUUId
	positions       *common.Positions
	poisson         *poisson.Poisson
	curFrame        *frame
	curFrameOnDisk  *frame
	writeInProgress func()
	subscriptions   *Subscriptions
	exe             *dispatcher.Executor
	db              *db.Databases
	vm              *VarManager
	rng             *rand.Rand
}

func VarFromData(data []byte, exe *dispatcher.Executor, db *db.Databases, vm *VarManager) (*Var, error) {
	seg, _, err := capn.ReadFromMemoryZeroCopy(data)
	if err != nil {
		return nil, err
	}
	varCap := msgs.ReadRootVar(seg)

	v := newVar(common.MakeVarUUId(varCap.Id()), exe, db, vm)
	positions := varCap.Positions()
	if positions.Len() != 0 {
		v.positions = (*common.Positions)(&positions)
	}

	writeTxnId := common.MakeTxnId(varCap.WriteTxnId())
	writeTxnClock := vc.VectorClockFromData(varCap.WriteTxnClock(), true).AsMutable()
	writesClock := vc.VectorClockFromData(varCap.WritesClock(), true).AsMutable()
	utils.DebugLog(vm.logger, "debug", "Restored.", "VarUUId", v.UUId, "TxnId", writeTxnId)

	v.curFrame = NewFrame(nil, v, writeTxnId, nil, writeTxnClock, writesClock)
	v.curFrameOnDisk = v.curFrame
	return v, nil
}

func NewVar(uuid *common.VarUUId, exe *dispatcher.Executor, db *db.Databases, vm *VarManager) *Var {
	v := newVar(uuid, exe, db, vm)

	clock := vc.NewVectorClock().AsMutable().Bump(v.UUId, 1)
	written := vc.NewVectorClock().AsMutable().Bump(v.UUId, 1)
	v.curFrame = NewFrame(nil, v, nil, nil, clock, written)

	return v
}

func newVar(uuid *common.VarUUId, exe *dispatcher.Executor, db *db.Databases, vm *VarManager) *Var {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &Var{
		UUId:            uuid,
		positions:       nil,
		poisson:         poisson.NewPoisson(),
		curFrame:        nil,
		curFrameOnDisk:  nil,
		writeInProgress: nil,
		subscriptions:   NewSubscriptions(vm),
		exe:             exe,
		db:              db,
		vm:              vm,
		rng:             rng,
	}
}

func (v *Var) ReceiveTxn(action *localAction, enqueuedAt time.Time) {
	utils.DebugLog(v.vm.logger, "debug", "ReceiveTxn.", "VarUUId", v.UUId, "action", action)
	v.poisson.AddThen(enqueuedAt)

	isRead, isWrite := action.IsRead(), action.IsWrite()

	switch {
	case isRead && isWrite:
		v.curFrame.AddReadWrite(action)
	case isRead:
		v.curFrame.AddRead(action)
	case isWrite: // includes roll
		v.curFrame.AddWrite(action)
	default:
		panic(fmt.Sprintf("Received txn action I don't understand: %v", action))
	}
}

func (v *Var) ReceiveTxnOutcome(action *localAction, enqueuedAt time.Time) {
	utils.DebugLog(v.vm.logger, "debug", "ReceiveTxnOutcome.", "VarUUId", v.UUId, "action", action)
	v.poisson.AddThen(enqueuedAt)

	isRead, isWrite := action.IsRead(), action.IsWrite()

	switch {
	case action.frame == nil:
		if (isWrite && !v.curFrame.WriteLearnt(action)) ||
			(!isWrite && isRead && !v.curFrame.ReadLearnt(action)) {
			action.LocallyComplete()
			v.maybeMakeInactive()
		}

	case action.frame.v != v:
		panic(fmt.Sprintf("%v frame var has changed %p -> %p (%v)", v.UUId, action.frame.v, v, action))

	case action.aborted:
		switch {
		case isRead && isWrite:
			action.frame.ReadWriteAborted(action, true)
		case isRead:
			action.frame.ReadAborted(action)
		case isWrite:
			action.frame.WriteAborted(action, true)
		default:
			panic(fmt.Sprintf("Received txn abort outcome I don't understand: %v", action))
		}

	default:
		switch {
		case isRead && isWrite:
			action.frame.ReadWriteCommitted(action)
		case isRead:
			action.frame.ReadCommitted(action)
		case isWrite:
			action.frame.WriteCommitted(action)
		default:
			panic(fmt.Sprintf("Received txn commit outcome I don't understand: %v", action))
		}
	}
}

func (v *Var) SetCurFrame(f *frame, action *localAction, positions *common.Positions) {
	utils.DebugLog(v.vm.logger, "debug", "SetCurFrame.", "VarUUId", v.UUId, "action", action)

	v.curFrame = f
	v.positions = positions

	// diffLen := action.outcomeClock.Len() - action.TxnReader.Actions(true).Actions().Len()
	// fmt.Printf("d%v ", diffLen)

	v.maybeWriteFrame(f, action)
}

func (v *Var) maybeWriteFrame(f *frame, action *localAction) {
	if v.writeInProgress != nil {
		v.writeInProgress = func() {
			v.writeInProgress = nil
			v.maybeWriteFrame(f, action)
		}
		return
	}
	v.writeInProgress = func() {
		v.writeInProgress = nil
		v.maybeMakeInactive()
	}

	varSeg := capn.NewBuffer(nil)
	varCap := msgs.NewRootVar(varSeg)

	varCap.SetId(v.UUId[:])
	varCap.SetPositions(capn.UInt8List(*v.positions))
	varCap.SetWriteTxnId(f.frameTxnId[:])
	varCap.SetWriteTxnClock(f.frameTxnClock.AsData())
	varCap.SetWritesClock(f.frameWritesClock.AsData())
	varData := common.SegToBytes(varSeg)

	curFrameOnDisk := v.curFrameOnDisk
	// to ensure correct order of writes, schedule the write from
	// the current go-routine...
	future := v.db.ReadWriteTransaction(func(rwtxn *mdbs.RWTxn) interface{} {
		if err := v.db.WriteTxnToDisk(rwtxn, f.frameTxnId, action.TxnReader.Data); err != nil {
			return types.EmptyStructVal
		} else if err := rwtxn.Put(v.db.Vars, v.UUId[:], varData, 0); err != nil {
			return types.EmptyStructVal
		} else if curFrameOnDisk != nil {
			v.db.DeleteTxnFromDisk(rwtxn, curFrameOnDisk.frameTxnId)
		}
		return types.EmptyStructVal
	})
	go func() {
		// ... but process the result in a new go-routine to avoid blocking the executor.
		if ran, err := future.ResultError(); err != nil {
			panic(fmt.Sprintf("Var error when writing to disk: %v\n", err))
		} else if ran != nil {
			// Switch back to the right go-routine
			v.applyToSelf(func() {
				utils.DebugLog(v.vm.logger, "debug", "Written to disk.", "VarUUId", v.UUId, "TxnId", f.frameTxnId)
				v.curFrameOnDisk = f
				for ancestor := f.parent; ancestor != nil && ancestor.DescendentOnDisk(); ancestor = ancestor.parent {
				}
				v.writeInProgress()
			})
		}
	}()
}

func (v *Var) TxnGloballyComplete(action *localAction, enqueuedAt time.Time) {
	utils.DebugLog(v.vm.logger, "debug", "Txn globally complete.", "VarUUId", v.UUId, "action", action)
	if action.frame.v != v {
		panic(fmt.Sprintf("%v frame var has changed %p -> %p (%v)", v.UUId, action.frame.v, v, action))
	}
	v.poisson.AddThen(enqueuedAt)
	if action.IsWrite() {
		action.frame.WriteGloballyComplete(action)
	} else {
		action.frame.ReadGloballyComplete(action)
	}
}

func (v *Var) maybeMakeInactive() {
	if v.isIdle() {
		v.vm.SetInactive(v)
	}
}

func (v *Var) isIdle() bool {
	return v.writeInProgress == nil && v.curFrame.isIdle()
}

func (v *Var) isOnDisk() bool {
	return v.writeInProgress == nil && v.curFrame == v.curFrameOnDisk && v.curFrame.isEmpty()
}

func (v *Var) applyToSelf(fun func()) {
	v.exe.EnqueueFuncAsync(func() (bool, error) {
		v.vm.ApplyToVar(func(v1 *Var) {
			switch {
			case v1 == nil:
				panic(fmt.Sprintf("%v not found!", v.UUId))
			case v1 != v:
				utils.DebugLog(v.vm.logger, "debug", "Ignoring callback as var object has changed.", "VarUUId", v.UUId)
				v1.maybeMakeInactive()
			default:
				fun()
			}
		}, false, v.UUId)
		return false, nil
	})
}

func (v *Var) Status(sc *status.StatusConsumer) {
	sc.Emit(v.UUId.String())
	if v.positions == nil {
		sc.Emit("- Positions: unknown")
	} else {
		sc.Emit(fmt.Sprintf("- Positions: %v", v.positions))
	}
	v.subscriptions.Status(sc.Fork())
	sc.Emit("- CurFrame:")
	v.curFrame.Status(sc.Fork())
	sc.Emit(fmt.Sprintf("- Idle? %v", v.isIdle()))
	sc.Emit(fmt.Sprintf("- IsOnDisk? %v", v.isOnDisk()))
	sc.Join()
}
