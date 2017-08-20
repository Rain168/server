package configuration

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"goshawkdb.io/common"
	msgs "goshawkdb.io/server/capnp"
)

var (
	TopologyVarUUId = common.MakeVarUUId([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	VersionOne      = common.MakeTxnId([]byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) // RMId 0 cannot exist so everything else is safe
)

type Topology struct {
	*Configuration
	FInc         uint8
	TwoFInc      uint16
	DBVersion    *common.TxnId
	RootVarUUIds Roots
}

type Roots []Root

func (r Roots) String() string {
	if r == nil || len(r) == 0 {
		return "No Roots"
	}
	roots := ""
	for _, root := range r {
		roots += fmt.Sprintf("%v@%v|", root.VarUUId, (*capn.UInt8List)(root.Positions).ToArray())
	}
	return roots[:len(roots)-1]
}

type Root struct {
	VarUUId   *common.VarUUId
	Positions *common.Positions
}

func BlankTopology(clusterId string, self common.RMId, port uint16, maxRMCount uint16) *Topology {
	return &Topology{
		Configuration: BlankConfiguration(clusterId, self, port, maxRMCount),
		FInc:          0,
		TwoFInc:       0,
		DBVersion:     VersionOne,
	}
}

func NewTopology(txnId *common.TxnId, rootsCap *msgs.VarIdPos_List, config *Configuration) *Topology {
	t := &Topology{
		Configuration: config,
		FInc:          config.F + 1,
		TwoFInc:       (2 * uint16(config.F)) + 1,
		DBVersion:     txnId,
	}
	if rootsCap != nil {
		if rootsCap.Len() < len(config.Roots) {
			panic(fmt.Sprintf("NewTopology expected to find at least %v roots by reference, but only found %v",
				len(config.Roots), rootsCap.Len()))
		}
		t.RootVarUUIds = make([]Root, rootsCap.Len())
		for idx := range t.RootVarUUIds {
			rootCap := rootsCap.At(idx)
			positions := rootCap.Positions()
			root := &t.RootVarUUIds[idx]
			root.VarUUId = common.MakeVarUUId(rootCap.Id())
			root.Positions = (*common.Positions)(&positions)
		}
	}
	return t
}

func (t *Topology) Clone() *Topology {
	c := &Topology{
		Configuration: t.Configuration.Clone(),
		FInc:          t.FInc,
		TwoFInc:       t.TwoFInc,
		DBVersion:     t.DBVersion,
		RootVarUUIds:  make([]Root, len(t.RootVarUUIds)),
	}
	copy(c.RootVarUUIds, t.RootVarUUIds)
	return c
}

func (t *Topology) SetConfiguration(config *Configuration) {
	t.Configuration = config
	t.FInc = config.F + 1
	t.TwoFInc = (2 * uint16(config.F)) + 1
}

func TopologyFromCap(txnId *common.TxnId, roots *msgs.VarIdPos_List, data []byte) (*Topology, error) {
	seg, _, err := capn.ReadFromMemoryZeroCopy(data)
	if err != nil {
		return nil, err
	}
	configCap := msgs.ReadRootConfiguration(seg)
	config := ConfigurationFromCap(configCap)
	return NewTopology(txnId, roots, config), nil
}

func (t *Topology) String() string {
	if t == nil {
		return "nil"
	}
	return fmt.Sprintf("Topology{%v, F+1: %v, 2F+1: %v, DBVersion: %v, RootVarUUIds: %v}",
		t.Configuration, t.FInc, t.TwoFInc, t.DBVersion, t.RootVarUUIds)
}

func (t *Topology) IsBlank() bool {
	return t == nil || t.MaxRMCount == 0 || t.RMs.NonEmptyLen() < int(t.TwoFInc)
}
