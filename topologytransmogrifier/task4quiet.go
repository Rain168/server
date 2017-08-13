package topologyTransmogrifier

import (
	"fmt"
	"goshawkdb.io/server/configuration"
	eng "goshawkdb.io/server/txnengine"
)

// quiet

type quiet struct {
	*targetConfigBase
	installing *configuration.Configuration
	stage      uint8
}

func (task *quiet) init(base *targetConfigBase) {
	task.targetConfigBase = base
}

func (task *quiet) IsValidTask() bool {
	active := task.activeTopology
	return active != nil && len(active.ClusterId) > 0 &&
		active.NextConfiguration != nil && active.NextConfiguration.Version == task.targetConfig.Version &&
		active.NextConfiguration.InstalledOnNew &&
		!active.NextConfiguration.QuietRMIds[task.connectionManager.RMId]
}

func (task *quiet) Tick() (bool, error) {
	// The purpose of getting the vars to go quiet isn't just for
	// emigration; it's also to require that txn outcomes are decided
	// (consensus reached) before any acceptors get booted out. So we
	// go through all this even if len(pending) is 0.
	if !task.IsValidTask() {
		return task.completed()
	}

	next := task.activeTopology.NextConfiguration
	localHost, err := task.firstLocalHost(task.activeTopology.Configuration)
	if err != nil {
		return task.fatal(err)
	}

	remoteHosts := task.allHostsBarLocalHost(localHost, next)

	activeNextConfig := next.Configuration
	if activeNextConfig != task.installing {
		task.installing = activeNextConfig
		task.stage = 0
		task.inner.Logger.Log("msg", "Quiet: new target topology detected; restarting.")
	}

	switch task.stage {
	case 0, 2:
		task.inner.Logger.Log("msg", fmt.Sprintf("Quiet: installing on to Proposers (%d of 3).", task.stage+1))
		// 0: Install to the proposerManagers. Once we know this is on
		// all our proposerManagers, we know that they will stop
		// accepting client txns.
		// 2: Install to the proposers again. This is to ensure that
		// TLCs have been written to disk.
		task.installTopology(task.activeTopology, map[eng.TopologyChangeSubscriberType]func() (bool, error){
			eng.ProposerSubscriber: func() (bool, error) {
				if activeNextConfig == task.installing {
					if task.stage == 0 || task.stage == 2 {
						task.stage++
					}
				}
				return task.maybeTick()
			},
		}, localHost, remoteHosts)

	case 1:
		task.inner.Logger.Log("msg", "Quiet: installing on to Vars (2 of 3).")
		// 1: Install to the varManagers. They only confirm back to us
		// once they've banned rolls, and ensured all active txns are
		// completed (though the TLC may not have gone to disk yet).
		task.installTopology(task.activeTopology, map[eng.TopologyChangeSubscriberType]func() (bool, error){
			eng.VarSubscriber: func() (bool, error) {
				if activeNextConfig == task.installing && task.stage == 1 {
					task.stage = 2
				}
				return task.maybeTick()
			},
		}, localHost, remoteHosts)

	case 3:
		// Now run a txn to record this.
		active, passive := task.formActivePassive(next.RMs, next.LostRMIds)
		if active == nil {
			return false, nil
		}

		twoFInc := uint16(next.RMs.NonEmptyLen())

		task.inner.Logger.Log("msg", "Quiet achieved, recording progress.", "pending", next.Pending,
			"active", fmt.Sprint(active), "passive", fmt.Sprint(passive))

		topology := task.activeTopology.Clone()
		topology.NextConfiguration.QuietRMIds[task.connectionManager.RMId] = true

		txn := task.createTopologyTransaction(task.activeTopology, topology, twoFInc, active, passive)
		go task.runTopologyTransaction(task, txn, active, passive)

	default:
		panic(fmt.Sprintf("Unexpected stage: %d", task.stage))
	}

	task.ensureShareGoalWithAll()
	return false, nil
}
