package consistenthash

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"goshawkdb.io/common"
	"goshawkdb.io/server"
	"math/rand"
)

var (
	VarUUIdNotKnown = fmt.Errorf("VarUUId Not Known to Cache")
)

type ConsistentHashCache struct {
	hashCodesPositions map[common.VarUUId]*hcPos
	resolver           *Resolver
	rng                *rand.Rand
	desiredLen         int
}

type hcPos struct {
	positions *common.Positions
	hashCodes []common.RMId
}

func NewCache(resolver *Resolver, desiredLen int, rng *rand.Rand) *ConsistentHashCache {
	return &ConsistentHashCache{
		hashCodesPositions: make(map[common.VarUUId]*hcPos),
		resolver:           resolver,
		rng:                rng,
		desiredLen:         desiredLen,
	}
}

func (chc *ConsistentHashCache) GetPositions(vUUId *common.VarUUId) *common.Positions {
	return chc.hashCodesPositions[*vUUId].positions
}

func (chc *ConsistentHashCache) GetHashCodes(vUUId *common.VarUUId) ([]common.RMId, error) {
	hcp, found := chc.hashCodesPositions[*vUUId]
	switch {
	case found && hcp.hashCodes != nil:
		return hcp.hashCodes, nil
	case found:
		positionsSlice := (*capn.UInt8List)(hcp.positions).ToArray()
		hashCodes, err := chc.resolver.ResolveHashCodes(positionsSlice, chc.desiredLen)
		if err != nil {
			return nil, err
		}
		hcp.hashCodes = hashCodes
		return hashCodes, nil
	default:
		return nil, VarUUIdNotKnown
	}
}

func (chc *ConsistentHashCache) AddPosition(vUUId *common.VarUUId, pos *common.Positions) {
	if pos == nil {
		panic(fmt.Sprintf("Added nil position for %v!", vUUId))
	}
	hcp, found := chc.hashCodesPositions[*vUUId]
	switch {
	case found && hcp.positions.Equal(pos):
		return
	case found:
		hcp.positions = pos
		hcp.hashCodes = nil
	default:
		chc.hashCodesPositions[*vUUId] = &hcPos{positions: pos}
	}
}

func (chc *ConsistentHashCache) Remove(vUUId *common.VarUUId) {
	delete(chc.hashCodesPositions, *vUUId)
}

func (chc *ConsistentHashCache) CreatePositions(vUUId *common.VarUUId, positionsLength int) (*common.Positions, []common.RMId, error) {
	positionsCap := capn.NewBuffer(nil).NewUInt8List(positionsLength)
	positionsSlice := make([]uint8, positionsLength)
	n, entropy := uint64(chc.rng.Int63()), uint64(server.TwoToTheSixtyThree)
	for idx := range positionsSlice {
		if idx == 0 {
			positionsCap.Set(idx, 0)
			positionsSlice[idx] = 0
		} else {
			idy := uint64(idx + 1)
			if entropy < uint64(idy) {
				n, entropy = uint64(chc.rng.Int63()), server.TwoToTheSixtyThree
			}
			pos := uint8(n % idy)
			n = n / idy
			entropy = entropy / uint64(idy)
			positionsCap.Set(idx, pos)
			positionsSlice[idx] = pos
		}
	}
	positions := (*common.Positions)(&positionsCap)
	hashCodes, err := chc.resolver.ResolveHashCodes(positionsSlice, chc.desiredLen)
	if err == nil {
		return positions, hashCodes, nil
	} else {
		return nil, nil, err
	}
}

func (chc *ConsistentHashCache) SetResolverDesiredLen(resolver *Resolver, desiredLen int) {
	chc.resolver = resolver
	chc.desiredLen = desiredLen
	for _, hcp := range chc.hashCodesPositions {
		hcp.hashCodes = nil
	}
}
