package client

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"goshawkdb.io/common"
	cmsgs "goshawkdb.io/common/capnp"
	msgs "goshawkdb.io/server/capnp"
	eng "goshawkdb.io/server/txnengine"
)

type versionCache map[common.VarUUId]*cached

type cached struct {
	txnId      *common.TxnId
	clockElem  uint64
	caps       *cmsgs.Capabilities
	value      []byte
	references []msgs.VarIdPos
}

type update struct {
	*cached
	varUUId *common.VarUUId
}

type unreached struct {
	update  *update
	updates *[]*update
}

var maxCapsCap *cmsgs.Capabilities

func init() {
	seg := capn.NewBuffer(nil)
	cap := cmsgs.NewCapabilities(seg)
	cap.SetValue(cmsgs.VALUECAPABILITY_READWRITE)
	ref := cap.References()
	ref.Read().SetAll()
	ref.Write().SetAll()
	maxCapsCap = &cap
}

func NewVersionCache(roots map[common.VarUUId]*cmsgs.Capabilities) versionCache {
	cache := make(map[common.VarUUId]*cached)
	for vUUId, caps := range roots {
		cache[vUUId] = &cached{caps: caps}
	}
	return cache
}

func (vc versionCache) ValidateTransaction(cTxn *cmsgs.ClientTxn) error {
	actions := cTxn.Actions()
	if cTxn.Retry() {
		for idx, l := 0, actions.Len(); idx < l; idx++ {
			action := actions.At(idx)
			vUUId := common.MakeVarUUId(action.VarId())
			if which := action.Which(); which != cmsgs.CLIENTACTION_READ {
				return fmt.Errorf("Retry transaction should only include reads. Found %v", which)
			} else if _, found := vc[*vUUId]; !found {
				return fmt.Errorf("Retry transaction has attempted to read from unknown object: %v", vUUId)
			}
		}

	} else {
		for idx, l := 0, actions.Len(); idx < l; idx++ {
			action := actions.At(idx)
			vUUId := common.MakeVarUUId(action.VarId())
			_, found := vc[*vUUId]
			switch action.Which() {
			case cmsgs.CLIENTACTION_READ, cmsgs.CLIENTACTION_WRITE, cmsgs.CLIENTACTION_READWRITE:
				if !found {
					return fmt.Errorf("Transaction manipulates unknown object: %v", vUUId)
				}

			case cmsgs.CLIENTACTION_CREATE:
				if found {
					return fmt.Errorf("Transaction tries to create existing object %v", vUUId)
				}

			default:
				return fmt.Errorf("Only read, write, readwrite or create actions allowed in client transaction, found %v", action.Which())
			}
		}
	}
	return nil
}

func (vc versionCache) ValueForWrite(vUUId *common.VarUUId, value []byte) []byte {
	if vc == nil {
		return value
	}
	if c, found := vc[*vUUId]; !found {
		panic(fmt.Errorf("ValueForWrite called for unknown %v", vUUId))
	} else {
		switch c.caps.Value() {
		case cmsgs.VALUECAPABILITY_WRITE, cmsgs.VALUECAPABILITY_READWRITE:
			return value
		default:
			return c.value
		}
	}
}

func (vc versionCache) ReferencesWriteMask(vUUId *common.VarUUId) (bool, []uint32, []msgs.VarIdPos) {
	if vc == nil || vUUId == nil {
		return true, nil, nil
	}
	if c, found := vc[*vUUId]; !found {
		panic(fmt.Errorf("ReferencesWriteMask called for unknown %v", vUUId))
	} else {
		write := c.caps.References().Write()
		switch write.Which() {
		case cmsgs.CAPABILITIESREFERENCESWRITE_ALL:
			return true, nil, c.references
		default:
			return false, write.Only().ToArray(), c.references
		}
	}
}

func (vc versionCache) EnsureSubset(vUUId *common.VarUUId, cap cmsgs.Capabilities) bool {
	if vc == nil {
		return true
	}
	if c, found := vc[*vUUId]; found {
		if c.caps == maxCapsCap {
			return true
		}
		valueNew, valueOld := cap.Value(), c.caps.Value()
		if valueNew > valueOld {
			return false
		}

		readNew, readOld := cap.References().Read(), c.caps.References().Read()
		if readOld.Which() == cmsgs.CAPABILITIESREFERENCESREAD_ONLY {
			if readNew.Which() != cmsgs.CAPABILITIESREFERENCESREAD_ONLY {
				return false
			}
			readNewOnly, readOldOnly := readNew.Only().ToArray(), readOld.Only().ToArray()
			if len(readNewOnly) > len(readOldOnly) {
				return false
			}
			common.SortUInt32(readNewOnly).Sort()
			common.SortUInt32(readOldOnly).Sort()
			for idx, indexNew := range readNewOnly {
				indexOld := readOldOnly[0]
				readOldOnly = readOldOnly[1:]
				if indexNew < indexOld {
					return false
				} else {
					for ; indexNew > indexOld && len(readOldOnly) > 0; readOldOnly = readOldOnly[1:] {
						indexOld = readOldOnly[0]
					}
					if len(readNewOnly)-idx > len(readOldOnly) {
						return false
					}
				}
			}
		}

		writeNew, writeOld := cap.References().Write(), c.caps.References().Write()
		if writeOld.Which() == cmsgs.CAPABILITIESREFERENCESWRITE_ONLY {
			if writeNew.Which() != cmsgs.CAPABILITIESREFERENCESWRITE_ONLY {
				return false
			}
			writeNewOnly, writeOldOnly := writeNew.Only().ToArray(), writeOld.Only().ToArray()
			if len(writeNewOnly) > len(writeOldOnly) {
				return false
			}
			common.SortUInt32(writeNewOnly).Sort()
			common.SortUInt32(writeOldOnly).Sort()
			for idx, indexNew := range writeNewOnly {
				indexOld := writeOldOnly[0]
				writeOldOnly = writeOldOnly[1:]
				if indexNew < indexOld {
					return false
				} else {
					for ; indexNew > indexOld && len(writeOldOnly) > 0; writeOldOnly = writeOldOnly[1:] {
						indexOld = writeOldOnly[0]
					}
					if len(writeNewOnly)-idx > len(writeOldOnly) {
						return false
					}
				}
			}
		}

		return true
	} else {
		return true
	}
}

func (vc versionCache) UpdateFromCommit(txn *eng.TxnReader, outcome *msgs.Outcome) {
	txnId := txn.Id
	clock := eng.VectorClockFromData(outcome.Commit(), false)
	actions := txn.Actions(true).Actions()
	for idx, l := 0, actions.Len(); idx < l; idx++ {
		action := actions.At(idx)
		if act := action.Which(); act != msgs.ACTION_READ {
			vUUId := common.MakeVarUUId(action.VarId())
			if c, found := vc[*vUUId]; found {
				c.txnId = txnId
				c.clockElem = clock.At(vUUId)
			} else if act == msgs.ACTION_CREATE {
				vc[*vUUId] = &cached{
					txnId:     txnId,
					clockElem: clock.At(vUUId),
					caps:      maxCapsCap,
				}
			} else {
				panic(fmt.Sprintf("%v contained action (%v) for unknown %v", txnId, act, vUUId))
			}
		}
	}
}

func (vc versionCache) UpdateFromAbort(updatesCap *msgs.Update_List) map[common.TxnId]*[]*update {
	l := updatesCap.Len()
	validUpdates := make(map[common.TxnId]*[]*update)
	unreachedMap := make(map[common.VarUUId]unreached, l)

	for idx := 0; idx < l; idx++ {
		updateCap := updatesCap.At(idx)
		txnId := common.MakeTxnId(updateCap.TxnId())
		clock := eng.VectorClockFromData(updateCap.Clock(), true)
		actionsCap := eng.TxnActionsFromData(updateCap.Actions(), true).Actions()
		updatesList := make([]*update, 0, actionsCap.Len())
		updatesListPtr := &updatesList
		validUpdates[*txnId] = updatesListPtr

		for idy, m := 0, actionsCap.Len(); idy < m; idy++ {
			actionCap := actionsCap.At(idy)
			vUUId := common.MakeVarUUId(actionCap.VarId())
			clockElem := clock.At(vUUId)

			switch actionCap.Which() {
			case msgs.ACTION_MISSING:
				// In this context, ACTION_MISSING means we know there was
				// a write of vUUId by txnId, but we have no idea what the
				// value written was. The only safe thing we can do is
				// remove it from the client.
				// log.Printf("%v contains missing write action of %v\n", txnId, vUUId)
				if c, found := vc[*vUUId]; found && c.txnId != nil {
					cmp := c.txnId.Compare(txnId)
					if cmp == common.EQ && clockElem != c.clockElem {
						panic(fmt.Sprintf("Clock version changed on missing for %v@%v (new:%v != old:%v)", vUUId, txnId, clockElem, c.clockElem))
					}
					if clockElem > c.clockElem || (clockElem == c.clockElem && cmp == common.LT) {
						c.txnId = nil
						c.clockElem = 0
						c.value = nil
						c.references = nil
						*updatesListPtr = append(*updatesListPtr, &update{
							cached:  c,
							varUUId: vUUId,
						})
					}
				}

			case msgs.ACTION_WRITE:
				write := actionCap.Write()
				if c, found := vc[*vUUId]; found {
					cmp := c.txnId.Compare(txnId)
					if cmp == common.EQ && clockElem != c.clockElem {
						panic(fmt.Sprintf("Clock version changed on write for %v@%v (new:%v != old:%v)", vUUId, txnId, clockElem, c.clockElem))
					}
					if c.txnId == nil || clockElem > c.clockElem || (clockElem == c.clockElem && cmp == common.LT) {
						c.txnId = txnId
						c.clockElem = clockElem
						c.value = write.Value()
						refs := write.References().ToArray()
						c.references = refs
						*updatesListPtr = append(*updatesListPtr, &update{
							cached:  c,
							varUUId: vUUId,
						})
						for ; len(refs) > 0; refs = refs[1:] {
							ref := refs[0]
							caps := ref.Capabilities()
							vUUId := common.MakeVarUUId(ref.Id())
							if c, found := vc[*vUUId]; found {
								c.caps = mergeCaps(c.caps, &caps)
							} else if ur, found := unreachedMap[*vUUId]; found {
								delete(unreachedMap, *vUUId)
								c := ur.update.cached
								c.caps = &caps
								vc[*vUUId] = c
								*ur.updates = append(*ur.updates, ur.update)
								refs = append(refs, ur.update.references...)
							} else {
								vc[*vUUId] = &cached{caps: &caps}
							}
						}
					}
				} else if _, found := unreachedMap[*vUUId]; found {
					panic(fmt.Sprintf("%v reported twice in same update (and appeared in unreachedMap twice!)", vUUId))
				} else {
					//log.Printf("%v contains write action of %v\n", txnId, vUUId)
					unreachedMap[*vUUId] = unreached{
						update: &update{
							cached: &cached{
								txnId:      txnId,
								clockElem:  clockElem,
								value:      write.Value(),
								references: write.References().ToArray(),
							},
							varUUId: vUUId,
						},
						updates: updatesListPtr,
					}
				}

			default:
				panic(fmt.Sprintf("Unexpected action for %v on %v: %v", txnId, vUUId, actionCap.Which()))
			}
		}
	}

	for txnId, updates := range validUpdates {
		if len(*updates) == 0 {
			delete(validUpdates, txnId)
		}
	}
	return validUpdates
}

func mergeCaps(a, b *cmsgs.Capabilities) *cmsgs.Capabilities {
	if a == maxCapsCap || b == maxCapsCap {
		return maxCapsCap
	}

	aValue := a.Value()
	aRefsRead := a.References().Read()
	aRefsWrite := a.References().Write()

	bValue := b.Value()
	bRefsRead := b.References().Read()
	bRefsWrite := b.References().Write()

	valueRead := aValue == cmsgs.VALUECAPABILITY_READWRITE || aValue == cmsgs.VALUECAPABILITY_READ ||
		bValue == cmsgs.VALUECAPABILITY_READWRITE || bValue == cmsgs.VALUECAPABILITY_READ
	valueWrite := aValue == cmsgs.VALUECAPABILITY_READWRITE || aValue == cmsgs.VALUECAPABILITY_WRITE ||
		bValue == cmsgs.VALUECAPABILITY_READWRITE || bValue == cmsgs.VALUECAPABILITY_WRITE
	refsReadAll := aRefsRead.Which() == cmsgs.CAPABILITIESREFERENCESREAD_ALL || bRefsRead.Which() == cmsgs.CAPABILITIESREFERENCESREAD_ONLY
	refsWriteAll := aRefsWrite.Which() == cmsgs.CAPABILITIESREFERENCESWRITE_ALL || bRefsWrite.Which() == cmsgs.CAPABILITIESREFERENCESWRITE_ALL

	if valueRead && valueWrite && refsReadAll && refsWriteAll {
		return maxCapsCap
	}

	seg := capn.NewBuffer(nil)
	cap := cmsgs.NewCapabilities(seg)
	switch {
	case valueRead && valueWrite:
		cap.SetValue(cmsgs.VALUECAPABILITY_READWRITE)
	case valueWrite:
		cap.SetValue(cmsgs.VALUECAPABILITY_WRITE)
	case valueRead:
		cap.SetValue(cmsgs.VALUECAPABILITY_WRITE)
	default:
		cap.SetValue(cmsgs.VALUECAPABILITY_NONE)
	}

	if refsReadAll {
		cap.References().Read().SetAll()
	} else {
		aOnly, bOnly := aRefsRead.Only().ToArray(), bRefsRead.Only().ToArray()
		cap.References().Read().SetOnly(mergeOnlies(seg, aOnly, bOnly))
	}

	if refsWriteAll {
		cap.References().Write().SetAll()
	} else {
		aOnly, bOnly := aRefsWrite.Only().ToArray(), bRefsWrite.Only().ToArray()
		cap.References().Write().SetOnly(mergeOnlies(seg, aOnly, bOnly))
	}

	return &cap
}

func mergeOnlies(seg *capn.Segment, a, b []uint32) capn.UInt32List {
	only := make([]uint32, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		aIndex, bIndex := a[0], b[0]
		switch {
		case aIndex < bIndex:
			only = append(only, aIndex)
			a = a[1:]
		case aIndex > bIndex:
			only = append(only, bIndex)
			b = b[1:]
		default:
			only = append(only, bIndex)
			a = a[1:]
			b = b[1:]
		}
	}
	if len(a) > 0 {
		only = append(only, a...)
	} else {
		only = append(only, b...)
	}

	cap := seg.NewUInt32List(len(only))
	for idx, index := range only {
		cap.Set(idx, index)
	}
	return cap
}

func (u *update) Value() []byte {
	if u.value == nil {
		return nil
	}
	switch u.caps.Value() {
	case cmsgs.VALUECAPABILITY_READ, cmsgs.VALUECAPABILITY_READWRITE:
		return u.value
	default:
		return []byte{}
	}
}

func (u *update) ReferencesReadMask() []uint32 {
	if u.value == nil {
		return nil
	}
	read := u.caps.References().Read()
	switch read.Which() {
	case cmsgs.CAPABILITIESREFERENCESREAD_ALL:
		mask := make([]uint32, len(u.references))
		for idx := range mask {
			mask[idx] = uint32(idx)
		}
		return mask
	default:
		return read.Only().ToArray()
	}
}
