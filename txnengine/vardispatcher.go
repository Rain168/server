package txnengine

import (
	"fmt"
	"github.com/go-kit/kit/log"
	"goshawkdb.io/common"
	"goshawkdb.io/server"
	"goshawkdb.io/server/db"
	"goshawkdb.io/server/dispatcher"
	"goshawkdb.io/server/types/connectionmanager"
	"goshawkdb.io/server/types/localconnection"
	"goshawkdb.io/server/utils/status"
)

type VarDispatcher struct {
	dispatcher.Dispatcher
	varmanagers []*VarManager
}

func NewVarDispatcher(count uint8, rmId common.RMId, cm connectionmanager.ConnectionManager, db *db.Databases, lc localconnection.LocalConnection, logger log.Logger) *VarDispatcher {
	vd := &VarDispatcher{
		varmanagers: make([]*VarManager, count),
	}
	logger = log.With(logger, "subsystem", "varManager")
	vd.Dispatcher.Init(count, logger)
	for idx, exe := range vd.Executors {
		vd.varmanagers[idx] = NewVarManager(exe, rmId, cm, db, lc,
			log.With(logger, "instance", idx))
	}
	return vd
}

func (vd *VarDispatcher) ApplyToVar(fun func(*Var), createIfMissing bool, vUUId *common.VarUUId) {
	vd.withVarManager(vUUId, func(vm *VarManager) { vm.ApplyToVar(fun, createIfMissing, vUUId) })
}

func (vd *VarDispatcher) Status(sc *status.StatusConsumer) {
	sc.Emit("Vars")
	for idx, exe := range vd.Executors {
		s := sc.Fork()
		s.Emit(fmt.Sprintf("Var Manager %v", idx))
		manager := vd.varmanagers[idx]
		exe.EnqueueFuncAsync(func() (bool, error) {
			manager.Status(s)
			return false, nil
		})
	}
	sc.Join()
}

func (vd *VarDispatcher) withVarManager(vUUId *common.VarUUId, fun func(*VarManager)) bool {
	idx := uint8(vUUId[server.MostRandomByteIndex]) % vd.ExecutorCount
	exe := vd.Executors[idx]
	manager := vd.varmanagers[idx]
	return exe.EnqueueFuncAsync(func() (bool, error) {
		fun(manager)
		return false, nil
	})
}
