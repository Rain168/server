package txnengine

import (
	"fmt"
	mdb "github.com/msackman/gomdb"
	mdbs "github.com/msackman/gomdb/server"
	tw "github.com/msackman/gotimerwheel"
	"goshawkdb.io/common"
	"goshawkdb.io/server"
	"goshawkdb.io/server/configuration"
	"goshawkdb.io/server/db"
	"goshawkdb.io/server/dispatcher"
	"time"
)

type VarManager struct {
	LocalConnection
	Topology         *configuration.Topology
	RMId             common.RMId
	db               *db.Databases
	active           map[common.VarUUId]*Var
	RollAllowed      bool
	onDisk           func(bool)
	tw               *tw.TimerWheel
	beaterTerminator chan struct{}
	exe              *dispatcher.Executor
}

func init() {
	db.DB.Vars = &mdbs.DBISettings{Flags: mdb.CREATE}
}

func NewVarManager(exe *dispatcher.Executor, rmId common.RMId, tp TopologyPublisher, db *db.Databases, lc LocalConnection) *VarManager {
	vm := &VarManager{
		LocalConnection: lc,
		RMId:            rmId,
		db:              db,
		active:          make(map[common.VarUUId]*Var),
		RollAllowed:     false,
		tw:              tw.NewTimerWheel(time.Now(), 25*time.Millisecond),
		exe:             exe,
	}
	exe.Enqueue(func() {
		vm.Topology = tp.AddTopologySubscriber(VarSubscriber, vm)
		vm.RollAllowed = vm.Topology == nil || !vm.Topology.NextBarrierReached1(rmId)
	})
	return vm
}

func (vm *VarManager) TopologyChanged(topology *configuration.Topology, done func(bool)) {
	resultChan := make(chan struct{})
	enqueued := vm.exe.Enqueue(func() {
		if od := vm.onDisk; od != nil {
			vm.onDisk = nil
			od(false)
		}
		vm.Topology = topology
		oldRollAllowed := vm.RollAllowed
		if !vm.RollAllowed {
			vm.RollAllowed = topology == nil || !topology.NextBarrierReached1(vm.RMId)
		}
		server.Log("VarManager", fmt.Sprintf("%p", vm), "rollAllowed:", oldRollAllowed, "->", vm.RollAllowed, fmt.Sprintf("%p", topology))

		goingToDisk := topology != nil && topology.NextBarrierReached1(vm.RMId) && !topology.NextBarrierReached2(vm.RMId)

		doneWrapped := func(result bool) {
			close(resultChan)
			done(result)
		}
		if goingToDisk {
			vm.onDisk = doneWrapped
			vm.checkAllDisk()
		} else {
			server.Log("VarManager", fmt.Sprintf("%p", vm), "calling done", fmt.Sprintf("%p", topology))
			doneWrapped(true)
		}
	})
	if enqueued {
		go vm.exe.WithTerminatedChan(func(terminated chan struct{}) {
			select {
			case <-resultChan:
			case <-terminated:
				select {
				case <-resultChan:
				default:
					done(false)
				}
			}
		})
	} else {
		done(false)
	}
}

func (vm *VarManager) ApplyToVar(fun func(*Var), createIfMissing bool, uuid *common.VarUUId) {
	v, shutdown := vm.find(uuid)
	if shutdown {
		return
	}
	if v == nil && createIfMissing {
		v = NewVar(uuid, vm.exe, vm.db, vm)
		vm.active[*v.UUId] = v
		server.Log(uuid, "New var")
	}
	fun(v)
	if _, found := vm.active[*uuid]; v != nil && !found && !v.isIdle() {
		panic(fmt.Sprintf("Var is not active, yet is not idle! %v %p", uuid, fun))
	} else {
		vm.checkAllDisk()
	}
}

func (vm *VarManager) checkAllDisk() {
	if od := vm.onDisk; od != nil {
		for _, v := range vm.active {
			if v.UUId.Compare(configuration.TopologyVarUUId) != common.EQ && !v.isOnDisk(true) {
				if !vm.RollAllowed {
					server.Log("VarManager", fmt.Sprintf("%p", vm), "WTF?! rolls are banned, but have var", v.UUId, "not on disk!")
				}
				return
			}
		}
		vm.onDisk = nil
		vm.RollAllowed = false
		server.Log("VarManager", fmt.Sprintf("%p", vm), "Rolls banned; calling done", fmt.Sprintf("%p", od))
		od(true)
	}
}

// var.VarLifecycle interface
func (vm *VarManager) SetInactive(v *Var) {
	server.Log(v.UUId, "is now inactive")
	v1, found := vm.active[*v.UUId]
	switch {
	case !found:
		panic(fmt.Sprintf("%v inactive but doesn't exist!\n", v.UUId))
	case v1 != v:
		panic(fmt.Sprintf("%v inactive but different var! %p %p\n", v.UUId, v, v1))
	default:
		//fmt.Printf("%v is now inactive. ", v.UUId)
		delete(vm.active, *v.UUId)
	}
}

func (vm *VarManager) find(uuid *common.VarUUId) (*Var, bool) {
	if v, found := vm.active[*uuid]; found {
		return v, false
	}

	result, err := vm.db.ReadonlyTransaction(func(rtxn *mdbs.RTxn) interface{} {
		// rtxn.Get returns a copy of the data, so we don't need to
		// worry about pointers into the db
		if bites, err := rtxn.Get(vm.db.Vars, uuid[:]); err == nil {
			return bites
		} else {
			return true
		}
	}).ResultError()

	if err != nil {
		panic(fmt.Sprintf("Error when loading %v from disk: %v", uuid, err))
	} else if result == nil { // shutdown
		return nil, true
	} else if bites, ok := result.([]byte); ok {
		v, err := VarFromData(bites, vm.exe, vm.db, vm)
		if err != nil {
			panic(fmt.Sprintf("Error when recreating %v: %v", uuid, err))
		} else if v == nil { // shutdown
			return v, true
		} else {
			vm.active[*v.UUId] = v
			return v, false
		}
	} else { // not found
		return nil, false
	}
}

func (vm *VarManager) Status(sc *server.StatusConsumer) {
	sc.Emit(fmt.Sprintf("- Active Vars: %v", len(vm.active)))
	sc.Emit(fmt.Sprintf("- Callbacks: %v", vm.tw.Length()))
	sc.Emit(fmt.Sprintf("- Beater live? %v", vm.beaterTerminator != nil))
	sc.Emit(fmt.Sprintf("- Roll allowed? %v", vm.RollAllowed))
	for _, v := range vm.active {
		v.Status(sc.Fork())
	}
	sc.Join()
}

func (vm *VarManager) ScheduleCallback(interval time.Duration, fun tw.Event) {
	if err := vm.tw.ScheduleEventIn(interval, fun); err != nil {
		panic(err)
	}
	if vm.beaterTerminator == nil {
		vm.beaterTerminator = make(chan struct{})
		go vm.beater(vm.beaterTerminator)
	}
}

func (vm *VarManager) beat() {
	vm.tw.AdvanceTo(time.Now(), 32)
	// fmt.Println("done:", )
	if vm.tw.IsEmpty() && vm.beaterTerminator != nil {
		close(vm.beaterTerminator)
		vm.beaterTerminator = nil
	}
}

func (vm *VarManager) beater(terminate chan struct{}) {
	sleep := 100 * time.Millisecond
	for {
		time.Sleep(sleep)
		select {
		case <-terminate:
			return
		default:
			vm.exe.Enqueue(vm.beat)
		}
	}
}
