//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2024 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package replication

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/weaviate/weaviate/cluster/proto/api"
)

type shardReplicationOpStatus struct {
	// state is the current state of the shard replication operation
	state api.ShardReplicationState
}

type ShardReplicationOp struct {
	ID uint64

	// Targeting information of the replication operation
	sourceShard shardFQDN
	targetShard shardFQDN
}

func NewShardReplicationOp(id uint64, sourceNode, targetNode, collectionId, shardId string) ShardReplicationOp {
	return ShardReplicationOp{
		ID:          id,
		sourceShard: newShardFQDN(sourceNode, collectionId, shardId),
		targetShard: newShardFQDN(targetNode, collectionId, shardId),
	}
}

type ShardReplicationFSM struct {
	opsLock sync.RWMutex

	// opsByNode stores the array of ShardReplicationOp for each "target" node
	opsByNode map[string][]ShardReplicationOp
	// opsByCollection stores the array of ShardReplicationOp for each collection
	opsByCollection map[string][]ShardReplicationOp
	// opsByShard stores the array of ShardReplicationOp for each shard
	opsByShard map[string][]ShardReplicationOp
	// opsByTargetFQDN stores the registered ShardReplicationOp (if any) for each destination replica
	opsByTargetFQDN map[shardFQDN]ShardReplicationOp
	// opsByShard stores opId -> replicationOp
	opsById map[uint64]ShardReplicationOp
	// opsStatus stores op -> opStatus
	opsStatus       map[ShardReplicationOp]shardReplicationOpStatus
	opsByStateGauge *prometheus.GaugeVec
}

func newShardReplicationFSM(reg prometheus.Registerer) *ShardReplicationFSM {
	fsm := &ShardReplicationFSM{
		opsByNode:       make(map[string][]ShardReplicationOp),
		opsByCollection: make(map[string][]ShardReplicationOp),
		opsByShard:      make(map[string][]ShardReplicationOp),
		opsByTargetFQDN: make(map[shardFQDN]ShardReplicationOp),
		opsById:         make(map[uint64]ShardReplicationOp),
		opsStatus:       make(map[ShardReplicationOp]shardReplicationOpStatus),
	}

	fsm.opsByStateGauge = promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "weaviate",
		Name:      "replication_operation_fsm_ops_by_state",
		Help:      "Current number of replication operations in each state of the FSM lifecycle",
	}, []string{"state"})

	return fsm
}

func (s *ShardReplicationFSM) GetOpsForNode(node string) []ShardReplicationOp {
	s.opsLock.RLock()
	defer s.opsLock.RUnlock()
	return s.opsByNode[node]
}

func (s shardReplicationOpStatus) ShouldRestartOp() bool {
	return s.state == api.REGISTERED || s.state == api.HYDRATING
}

func (s *ShardReplicationFSM) GetOpState(op ShardReplicationOp) shardReplicationOpStatus {
	s.opsLock.RLock()
	defer s.opsLock.RUnlock()
	return s.opsStatus[op]
}

func (s *ShardReplicationFSM) FilterOneShardReplicasReadWrite(collection string, shard string, shardReplicasLocation []string) ([]string, []string) {
	s.opsLock.RLock()
	defer s.opsLock.RUnlock()

	_, ok := s.opsByShard[shard]
	// Check if the specified shard is current undergoing replication at all.
	// If not we can return early as all replicas can be used for read/writes
	if !ok {
		return shardReplicasLocation, shardReplicasLocation
	}

	readReplicas := make([]string, 0, len(shardReplicasLocation))
	writeReplicas := make([]string, 0, len(shardReplicasLocation))
	for _, shardReplicaLocation := range shardReplicasLocation {
		readOk, writeOk := s.filterOneReplicaReadWrite(shardReplicaLocation, collection, shard)
		if readOk {
			readReplicas = append(readReplicas, shardReplicaLocation)
		}
		if writeOk {
			writeReplicas = append(writeReplicas, shardReplicaLocation)
		}
	}

	return readReplicas, writeReplicas
}

func (s *ShardReplicationFSM) filterOneReplicaReadWrite(node string, collection string, shard string) (bool, bool) {
	targetFQDN := newShardFQDN(node, collection, shard)
	op, ok := s.opsByTargetFQDN[targetFQDN]
	// There's no replication ops for that replicas, it can be used for both read and writes
	if !ok {
		return true, true
	}

	opState, ok := s.opsStatus[op]
	if !ok {
		// TODO: This should never happens
		return true, true
	}

	// Filter read/write based on the state of the replica
	readOk := false
	writeOk := false
	switch opState.state {
	case api.FINALIZING:
		writeOk = true
	case api.READY:
		readOk = true
		writeOk = true
	default:
	}
	return readOk, writeOk
}
