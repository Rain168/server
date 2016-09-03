package client

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"goshawkdb.io/common"
	cmsgs "goshawkdb.io/common/capnp"
	msgs "goshawkdb.io/server/capnp"
	ch "goshawkdb.io/server/consistenthash"
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

type cacheOverlay struct {
	*cached
	// we only duplicate the txnId here for the MISSING case
	txnId  *common.TxnId
	stored bool
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
			} else if c, found := vc[*vUUId]; !found || c.txnId == nil {
				return fmt.Errorf("Retry transaction has attempted to read from unknown object: %v", vUUId)
			}
		}

	} else {
		for idx, l := 0, actions.Len(); idx < l; idx++ {
			action := actions.At(idx)
			vUUId := common.MakeVarUUId(action.VarId())
			c, found := vc[*vUUId]
			switch action.Which() {
			case cmsgs.CLIENTACTION_READ, cmsgs.CLIENTACTION_WRITE, cmsgs.CLIENTACTION_READWRITE:
				if !found || c.txnId == nil {
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

// the problem is that we can't distinguish between a client trying to write an empty value or not.
func (vc versionCache) ValueForWrite(vUUId *common.VarUUId, value []byte) ([]byte, error) {
	if vc == nil {
		return value, nil
	}
	if c, found := vc[*vUUId]; !found || c.txnId == nil {
		return nil, fmt.Errorf("Write attempted on unknown %v", vUUId)
	} else {
		switch valueCap := c.caps.Value(); {
		case valueCap == cmsgs.VALUECAPABILITY_WRITE || valueCap == cmsgs.VALUECAPABILITY_READWRITE:
			return value, nil
		case len(value) > 0: // fuzzy. The client could be attempting to write empty value illegally too.
			return nil, fmt.Errorf("Transaction illegally to write the value of an object. %v", vUUId)
		default:
			return c.value, nil
		}
	}
}

// returns the 'extra' refs, with holes in the list for valid client refs
func (vc versionCache) ReferencesForWrite(vUUId *common.VarUUId, clientRefs *cmsgs.ClientVarIdPos_List) ([]*msgs.VarIdPos, *cached, error) {
	if vc == nil {
		return nil, nil, nil
	}
	if c, found := vc[*vUUId]; !found || c.txnId == nil {
		return nil, nil, fmt.Errorf("ReferencesForWrite called for unknown %v", vUUId)
	} else {
		refsWriteCap := c.caps.References().Write()
		switch refsWriteCap.Which() {
		case cmsgs.CAPABILITIESREFERENCESWRITE_ALL:
			return nil, c, nil
		default:
			clientRefsLen := clientRefs.Len()
			only := refsWriteCap.Only().ToArray()
			// The client must provide refs for every index in only.
			reqLen := 0
			if l := len(only); l > 0 {
				reqLen = int(only[l-1]) + 1
			}
			if clientRefsLen != reqLen {
				return nil, nil, fmt.Errorf("Incorrect number of references provided for write of %v", vUUId)
			}
			// Where possible, we fill in the gaps in only with
			// c.references. Keep in mind that the client may have onlies
			// that are longer than the current number of
			// references. This can happen when a capability in a ref
			// includes writes to n refs, and then the object itself is
			// updated to only include m refs, where m < n. We change a
			// write to a readwrite iff c.references - onlies is not the
			// empty set.
			resultsLen := clientRefsLen
			if l := len(c.references); l > resultsLen {
				resultsLen = l
			}
			results := make([]*msgs.VarIdPos, resultsLen)
			nonNilAppended := false
			for idx := 0; idx < clientRefsLen; idx++ {
				if len(only) > 0 && uint32(idx) == only[0] {
					only = only[1:]
				} else if idx < len(c.references) {
					nonNilAppended = true
					results[idx] = &c.references[idx]
				}
			}
			// add on anything in c.references that's left over
			for idx := clientRefsLen; idx < resultsLen; idx++ {
				nonNilAppended = true
				results[idx] = &c.references[idx]
			}
			if nonNilAppended {
				return results, c, nil
			} else {
				return nil, c, nil
			}
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
		switch {
		case valueNew == valueOld:
		case valueNew == cmsgs.VALUECAPABILITY_NONE: // new is bottom, always fine
		case valueOld == cmsgs.VALUECAPABILITY_READWRITE: // old is top, always fine
		default:
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
			for idx, indexNew := range readNewOnly {
				indexOld := readOldOnly[0]
				readOldOnly = readOldOnly[1:]
				if indexNew < indexOld {
					return false
				} else if indexNew > indexOld {
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
			for idx, indexNew := range writeNewOnly {
				indexOld := writeOldOnly[0]
				writeOldOnly = writeOldOnly[1:]
				if indexNew < indexOld {
					return false
				} else if indexNew > indexOld {
					for ; indexNew > indexOld && len(writeOldOnly) > 0; writeOldOnly = writeOldOnly[1:] {
						indexOld = writeOldOnly[0]
					}
					if len(writeNewOnly)-idx > len(writeOldOnly) {
						return false
					}
				}
			}
		}
	}
	return true
}

func (vc versionCache) UpdateFromCommit(txn *eng.TxnReader, outcome *msgs.Outcome) {
	txnId := txn.Id
	clock := eng.VectorClockFromData(outcome.Commit(), false)
	actions := txn.Actions(true).Actions()
	for idx, l := 0, actions.Len(); idx < l; idx++ {
		action := actions.At(idx)
		if act := action.Which(); act != msgs.ACTION_READ {
			vUUId := common.MakeVarUUId(action.VarId())
			c, found := vc[*vUUId]
			if act == msgs.ACTION_CREATE && !found {
				create := action.Create()
				c = &cached{
					txnId:      txnId,
					clockElem:  clock.At(vUUId),
					caps:       maxCapsCap,
					value:      create.Value(),
					references: create.References().ToArray(),
				}
				vc[*vUUId] = c
			} else {
				panic(fmt.Sprintf("%v contained illegal action (%v) for %v", txnId, act, vUUId))
			}

			c.txnId = txnId
			c.clockElem = clock.At(vUUId)

			switch act {
			case msgs.ACTION_WRITE:
				write := action.Write()
				c.value = write.Value()
				c.references = write.References().ToArray()
			case msgs.ACTION_READWRITE:
				rw := action.Readwrite()
				c.value = rw.Value()
				c.references = rw.References().ToArray()
			default:
				panic(fmt.Sprintf("Unexpected action type on txn commit! %v %v", txnId, act))
			}
		}
	}
}

func (vc versionCache) UpdateFromAbort(updatesCap *msgs.Update_List) map[common.TxnId]*[]*update {
	updateGraph := make(map[common.VarUUId]*cacheOverlay)

	// 1. update everything we know we can already reach, and filter out erroneous updates
	vc.updateExisting(updatesCap, updateGraph)

	// 2. figure out what we can now reach, and propagate through extended caps
	vc.updateReachable(updateGraph)

	// 3. populate results
	updates := make([]update, len(updateGraph))
	validUpdates := make(map[common.TxnId]*[]*update, len(updateGraph))
	for vUUId, overlay := range updateGraph {
		if !overlay.stored {
			continue
		}
		updateListPtr, found := validUpdates[*overlay.txnId]
		if !found {
			updateList := []*update{}
			validUpdates[*overlay.txnId] = &updateList
		}
		vUUIdCopy := vUUId
		update := &updates[0]
		updates = updates[1:]
		update.cached = overlay.cached
		update.varUUId = &vUUIdCopy
		*updateListPtr = append(*updateListPtr, update)
	}

	return validUpdates
}

func (vc versionCache) updateExisting(updatesCap *msgs.Update_List, updateGraph map[common.VarUUId]*cacheOverlay) {
	for idx, l := 0, updatesCap.Len(); idx < l; idx++ {
		updateCap := updatesCap.At(idx)
		txnId := common.MakeTxnId(updateCap.TxnId())
		clock := eng.VectorClockFromData(updateCap.Clock(), true)
		actionsCap := eng.TxnActionsFromData(updateCap.Actions(), true).Actions()

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
						updateGraph[*vUUId] = &cacheOverlay{
							cached: c,
							txnId:  txnId,
							stored: true,
						}
					}
				}

			case msgs.ACTION_WRITE:
				write := actionCap.Write()
				if c, found := vc[*vUUId]; found {
					// If it's in vc then we can either reach it currently
					// or we have been able to in the past.
					updating := c.txnId == nil
					if !updating {
						cmp := c.txnId.Compare(txnId)
						if cmp == common.EQ && clockElem != c.clockElem {
							panic(fmt.Sprintf("Clock version changed on write for %v@%v (new:%v != old:%v)", vUUId, txnId, clockElem, c.clockElem))
						}
						updating = clockElem > c.clockElem || (clockElem == c.clockElem && cmp == common.LT)
					}
					// If we're not updating then the update must predate
					// our current knowledge of vUUId. So we're not going
					// to send it to the client in which case the
					// capabilities vUUId grants via its own refs can't
					// widen: we already know everything the client knows
					// and we're not extending that. So it's safe to
					// totally ignore it.
					if updating {
						c.txnId = txnId
						c.clockElem = clockElem
						c.value = write.Value()
						c.references = write.References().ToArray()
						updateGraph[*vUUId] = &cacheOverlay{
							cached: c,
							txnId:  txnId,
							stored: true,
						}
					}

				} else {
					//log.Printf("%v contains write action of %v\n", txnId, vUUId)
					updateGraph[*vUUId] = &cacheOverlay{
						cached: &cached{
							txnId:      txnId,
							clockElem:  clockElem,
							value:      write.Value(),
							references: write.References().ToArray(),
						},
						txnId:  txnId,
						stored: false,
					}
				}

			default:
				panic(fmt.Sprintf("Unexpected action for %v on %v: %v", txnId, vUUId, actionCap.Which()))
			}
		}
	}
}

func (vc versionCache) updateReachable(updateGraph map[common.VarUUId]*cacheOverlay) {
	reaches := make(map[common.VarUUId][]*msgs.VarIdPos)
	worklist := make([]common.VarUUId, 0, len(updateGraph))

	for vUUId, overlay := range updateGraph {
		if overlay.stored {
			reaches[vUUId] = overlay.reachableReferences()
			worklist = append(worklist, vUUId)
		}
	}

	for len(worklist) > 0 {
		vUUId := worklist[0]
		worklist = worklist[1:]
		for _, ref := range reaches[vUUId] {
			// Given the current vUUId.caps, we're looking at what we
			// can reach from there.
			vUUIdRef := common.MakeVarUUId(ref.Id())
			caps := ref.Capabilities()
			var c *cached
			overlay, found := updateGraph[*vUUIdRef]
			if found {
				if !overlay.stored {
					overlay.stored = true
					vc[*vUUIdRef] = overlay.cached
				}
				c = overlay.cached
			} else {
				// There's no update for vUUIdRef, but it's possible we're
				// adding to the capabilities the client now has on
				// vUUIdRef so we need to record that. That in turn can
				// mean we now have access to extra vars.
				c, found = vc[*vUUIdRef]
				if !found {
					// We have no idea though what this var (vUUIdRef)
					// actually points to. caps is just our capabilities to
					// act on this var, so there's no extra work to do
					// (c.reachableReferences will return []).
					c = &cached{caps: &caps}
					vc[*vUUIdRef] = c
				}
			}
			// We have two questions to answer: 1. Have we already
			// processed vUUIdRef?  2. If we have, do we have wider caps
			// now than before?
			before := reaches[*vUUIdRef]
			ensureUpdate := c.mergeCaps(&caps)
			after := c.reachableReferences()
			if len(after) > len(before) {
				reaches[*vUUIdRef] = after
				worklist = append(worklist, *vUUIdRef)
				ensureUpdate = true
			}
			if ensureUpdate && overlay == nil && c.txnId != nil {
				// Our access to vUUIdRef has expanded to the extent that
				// we can now see more of the refs from vUUIdRef, or we
				// can now see the value of vUUIdRef. So even though there
				// wasn't an actual update for vUUIdRef, we need to create
				// one.
				updateGraph[*vUUIdRef] = &cacheOverlay{
					cached: c,
					txnId:  c.txnId,
					stored: true,
				}
			}
		}
	}
}

// returns true iff we couldn't read the value before merge, but we
// can after
func (c *cached) mergeCaps(b *cmsgs.Capabilities) (gainedRead bool) {
	a := c.caps
	switch {
	case a == b:
		return false
	case a == maxCapsCap || b == maxCapsCap:
		c.caps = maxCapsCap
		return a != maxCapsCap
	case a == nil:
		c.caps = b
		return b.Value() == cmsgs.VALUECAPABILITY_READ || b.Value() == cmsgs.VALUECAPABILITY_READWRITE
	case b == nil:
		return false
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

	gainedRead = valueRead && aValue != cmsgs.VALUECAPABILITY_READ && aValue != cmsgs.VALUECAPABILITY_READWRITE

	if valueRead && valueWrite && refsReadAll && refsWriteAll {
		c.caps = maxCapsCap
		return
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
		cap.References().Read().SetOnly(mergeOnliesSeg(seg, aOnly, bOnly))
	}

	if refsWriteAll {
		cap.References().Write().SetAll()
	} else {
		aOnly, bOnly := aRefsWrite.Only().ToArray(), bRefsWrite.Only().ToArray()
		cap.References().Write().SetOnly(mergeOnliesSeg(seg, aOnly, bOnly))
	}

	c.caps = &cap
	return
}

func mergeOnliesSeg(seg *capn.Segment, a, b []uint32) capn.UInt32List {
	only := mergeOnlies(a, b)

	cap := seg.NewUInt32List(len(only))
	for idx, index := range only {
		cap.Set(idx, index)
	}
	return cap
}

func mergeOnlies(a, b []uint32) []uint32 {
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
			only = append(only, aIndex)
			a = a[1:]
			b = b[1:]
		}
	}
	if len(a) > 0 {
		only = append(only, a...)
	} else {
		only = append(only, b...)
	}

	return only
}

// does not leave holes in the result - compacted.
func (c *cached) reachableReferences() []*msgs.VarIdPos {
	if c.caps == nil || len(c.references) == 0 {
		return nil
	}

	refsReadCap := c.caps.References().Read()
	all := refsReadCap.Which() == cmsgs.CAPABILITIESREFERENCESREAD_ALL
	var only []uint32
	if !all {
		only = c.caps.References().Read().Only().ToArray()
	}

	result := make([]*msgs.VarIdPos, 0, len(c.references))
LOOP:
	for index, ref := range c.references {
		refCopy := ref
		switch {
		case all:
		case len(only) == 0:
			break LOOP
		case uint32(index) == only[0]:
			only = only[1:]
		default:
			continue
		}
		if len(ref.Id()) == common.KeyLen {
			result = append(result, &refCopy)
		}
	}
	return result
}

func (u *update) AddToClientAction(hashCache *ch.ConsistentHashCache, seg *capn.Segment, clientAction *cmsgs.ClientAction) {
	clientAction.SetVarId(u.varUUId[:])
	c := u.cached
	if c.txnId == nil {
		clientAction.SetDelete()
	} else {
		clientAction.SetWrite()
		clientWrite := clientAction.Write()

		switch c.caps.Value() {
		case cmsgs.VALUECAPABILITY_READ, cmsgs.VALUECAPABILITY_READWRITE:
			clientWrite.SetValue(c.value)
		default:
			clientWrite.SetValue([]byte{})
		}

		refsReadCaps := c.caps.References().Read()
		all := refsReadCaps.Which() == cmsgs.CAPABILITIESREFERENCESREAD_ALL
		var only []uint32
		if !all {
			only = refsReadCaps.Only().ToArray()
		}
		clientReferences := cmsgs.NewClientVarIdPosList(seg, len(c.references))
		for idx, ref := range c.references {
			switch {
			case all:
			case len(only) > 0 && only[0] == uint32(idx):
				only = only[1:]
			default:
				continue
			}
			varIdPos := clientReferences.At(idx)
			varIdPos.SetVarId(ref.Id())
			if len(ref.Id()) == common.KeyLen {
				varIdPos.SetCapabilities(ref.Capabilities())
				positions := common.Positions(ref.Positions())
				hashCache.AddPosition(common.MakeVarUUId(ref.Id()), &positions)
			}
		}
		clientWrite.SetReferences(clientReferences)
	}
}
