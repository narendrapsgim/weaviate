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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/weaviate/weaviate/cluster/proto/api"
	"github.com/weaviate/weaviate/cluster/replication/types"
	enterrors "github.com/weaviate/weaviate/entities/errors"
)

// OpConsumer is an interface for consuming replication operations.
type OpConsumer interface {
	// Consume starts consuming operations from the provided channel.
	// The consumer processes operations, and a buffered channel is typically used to apply backpressure.
	// The consumer should return an error if it fails to process any operation.
	Consume(ctx context.Context, in <-chan ShardReplicationOp) error
}

// CopyOpConsumer is an implementation of the OpConsumer interface that processes replication operations
// by executing copy operations from a source shard to a target shard. It uses a ReplicaCopier to actually
// carry out the copy operation. Moreover, it supports configurable backoff, timeout and concurrency limits.
type CopyOpConsumer struct {
	// logger is used for structured logging throughout the consumer's lifecycle.
	// It provides detailed logs for each replication operation and any errors encountered.
	logger *logrus.Entry

	// leaderClient is responsible for interacting with the FSM to update the state of replication operations.
	// It is used to update the status of operations during the replication process (e.g. update to HYDRATING state).
	leaderClient types.FSMUpdater

	// replicaCopier is used to handle the actual copying of replica data from the source shard to the target shard.
	// It abstracts the mechanics of data replication and file copying.
	replicaCopier types.ReplicaCopier

	// backoffPolicy defines the retry mechanism for failed operations.
	// It allows the consumer to retry replication operations using a backoff strategy in case of failure.
	backoffPolicy backoff.BackOff

	// maxWorkers sets the maximum number of concurrent workers that will be used to process replication operations.
	// It controls the level of parallelism in the replication process allowing multiple replication operations to
	// run concurrently.
	maxWorkers int

	// opTimeout defines the timeout duration for each replication operation.
	// It ensures that operations do not hang indefinitely and are retried or terminated after the timeout period.
	opTimeout time.Duration

	// timeProvider abstracts time operations, allowing for easier testing and mocking of time-related functions.
	timeProvider TimeProvider

	// tokens controls the maximum number of concurrently running consumers
	tokens chan struct{}

	// nodeId uniquely identifies the node on which this consumer instance is running.
	nodeId string
}

// String returns a string representation of the CopyOpConsumer,
// including the node ID that uniquely identifies the consumer.
//
// The assumption is that each node runs one and only one replication engine,
// which means there is one consumer per node.
func (c *CopyOpConsumer) String() string {
	return fmt.Sprintf("replication engine copy consumer on node '%s'", c.nodeId)
}

// NewCopyOpConsumer creates a new CopyOpConsumer instance responsible for executing
// replication operations using a configurable worker pool.
//
// It uses a ReplicaCopier to perform the actual data copy.
func NewCopyOpConsumer(
	logger *logrus.Logger,
	leaderClient types.FSMUpdater,
	replicaCopier types.ReplicaCopier,
	timeProvider TimeProvider,
	nodeId string,
	backoffPolicy backoff.BackOff,
	opTimeout time.Duration,
	maxWorkers int,
) *CopyOpConsumer {
	c := &CopyOpConsumer{
		logger:        logger.WithFields(logrus.Fields{"component": "replication_consumer", "action": replicationEngineLogAction, "node": nodeId, "workers": maxWorkers, "timeout": opTimeout}),
		leaderClient:  leaderClient,
		replicaCopier: replicaCopier,
		backoffPolicy: backoffPolicy,
		opTimeout:     opTimeout,
		maxWorkers:    maxWorkers,
		nodeId:        nodeId,
		timeProvider:  timeProvider,
		tokens:        make(chan struct{}, maxWorkers),
	}
	return c
}

// Consume processes replication operations from the input channel, ensuring that only a limited number of consumers
// are active concurrently based on the maxWorkers value.
func (c *CopyOpConsumer) Consume(ctx context.Context, in <-chan ShardReplicationOp) error {
	c.logger.Info("starting replication operation consumer")

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			c.logger.WithFields(logrus.Fields{"consumer": c, "reason": ctx.Err()}).Info("context canceled, shutting down consumer")
			wg.Wait() // Waiting for pending operations before terminating
			return ctx.Err()

		case op, ok := <-in:
			if !ok {
				c.logger.WithFields(logrus.Fields{"consumer": c}).Info("operation channel closed, shutting down consumer")
				wg.Wait() // Waiting for pending operations before terminating
				return nil
			}

			select {
			// The 'tokens' channel limits the number of concurrent workers (`maxWorkers`).
			// Each worker acquires a token before processing an operation. If no tokens are available,
			// the worker blocks until one is released. After completing the task, the worker releases the token,
			// allowing another worker to proceed. This ensures only a limited number of workers is concurrently
			// running replication operations and avoids overloading the system.
			case c.tokens <- struct{}{}:

				wg.Add(1)

				// Here we capture the op argument used by the func below as the enterrors.GoWrapper requires calling
				// a function without arguments.
				operation := op

				enterrors.GoWrapper(func() {
					defer func() {
						<-c.tokens // Release token when completed
						wg.Done()
					}()

					opLogger := c.logger.WithFields(logrus.Fields{
						"consumer":          c,
						"op":                operation.ID,
						"source_node":       operation.sourceShard.nodeId,
						"target_node":       operation.targetShard.nodeId,
						"source_shard":      operation.sourceShard.shardId,
						"target_shard":      operation.targetShard.shardId,
						"source_collection": operation.sourceShard.collectionId,
						"target_collection": operation.targetShard.collectionId,
					})

					opLogger.Info("worker processing replication operation")

					// Start a replication operation with a timeout for completion to prevent replication operations
					// from running indefinitely
					opCtx, opCancel := context.WithTimeout(workerCtx, c.opTimeout)
					defer opCancel()

					err := c.processReplicationOp(opCtx, operation.ID, operation)
					if err != nil && errors.Is(err, context.DeadlineExceeded) {
						opLogger.WithError(err).Error("replication operation timed out")
					} else if err != nil {
						opLogger.WithError(err).Error("replication operation failed")
					}
				}, c.logger)

			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// processReplicationOp performs the full replication flow for a single operation.
//
// It performs of the following steps:
//  1. Updates the operation status to HYDRATING using the leader FSM updater.
//  2. Initiates the copy of replica data from the source node to the target shard.
//  3. Once the copy succeeds, updates the sharding state to reflect the added replica.
//
// If any step fails, the operation is retried using the configured backoff policy.
// Errors are logged and wrapped using the structured error group wrapper.
func (c *CopyOpConsumer) processReplicationOp(ctx context.Context, workerId uint64, op ShardReplicationOp) error {
	logger := c.logger.WithFields(logrus.Fields{
		"consumer":          c,
		"op":                op.ID,
		"source_node":       op.sourceShard.nodeId,
		"target_node":       op.targetShard.nodeId,
		"source_shard":      op.sourceShard.shardId,
		"target_shard":      op.targetShard.shardId,
		"source_collection": op.sourceShard.collectionId,
		"target_collection": op.targetShard.collectionId,
	})

	startTime := c.timeProvider.Now()

	return backoff.Retry(func() error {
		if ctx.Err() != nil {
			logger.WithField("consumer", c).WithError(ctx.Err()).Error("error while processing replication operation, shutting down")
			return backoff.Permanent(ctx.Err())
		}

		if err := c.leaderClient.ReplicationUpdateReplicaOpStatus(op.ID, api.HYDRATING); err != nil {
			logger.WithField("consumer", c).WithError(err).Error("failed to update replica status to 'HYDRATING'")
			return err
		}

		logger.WithField("consumer", c).Info("starting replication copy operation")

		if err := c.replicaCopier.CopyReplica(ctx, op.sourceShard.nodeId, op.sourceShard.collectionId, op.targetShard.shardId); err != nil {
			logger.WithField("consumer", c).WithError(err).Error("failure while copying replica shard")
			return err
		}

		if _, err := c.leaderClient.AddReplicaToShard(ctx, op.targetShard.collectionId, op.targetShard.shardId, op.targetShard.nodeId); err != nil {
			logger.WithField("consumer", c).WithError(err).Error("failure while updating sharding state")
			return err
		}

		c.logCompletedReplicationOp(workerId, startTime, c.timeProvider.Now(), op)

		return nil
	}, c.backoffPolicy)
}

func (c *CopyOpConsumer) logCompletedReplicationOp(workerId uint64, startTime time.Time, endTime time.Time, op ShardReplicationOp) {
	duration := endTime.Sub(startTime)

	c.logger.WithFields(logrus.Fields{
		"worker":            workerId,
		"op":                op.ID,
		"duration":          duration.String(),
		"start_time":        startTime.Format(time.RFC1123),
		"completed_since":   c.timeProvider.Now().Sub(endTime),
		"source_node":       op.sourceShard.nodeId,
		"target_node":       op.targetShard.nodeId,
		"source_shard":      op.sourceShard.shardId,
		"target_shard":      op.targetShard.shardId,
		"source_collection": op.sourceShard.collectionId,
		"target_collection": op.targetShard.collectionId,
	}).Info("Replication operation completed successfully")
}
