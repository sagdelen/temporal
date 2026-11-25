package mongodb_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	enumsspb "go.temporal.io/server/api/enums/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/mongodb"
	"go.temporal.io/server/common/persistence/mongodb/client"
	"go.temporal.io/server/service/history/tasks"
)

// These tests cover a history task key collision inside an execution
// transaction against a real MongoDB replica set (transactions are required).
// They are skipped when MongoDB is not reachable. See MONGODB_SEEDS /
// TEMPORAL_MONGODB_TEST_* in test_config_test.go for connection settings.
//
// A duplicate key error aborts a MongoDB transaction. If the store ignores it
// and carries on, every later operation and the commit fail with
// NoSuchTransaction, which the driver treats as transient and retries until
// its 120 second limit. The SQL stores fail the transaction on a duplicate
// task key; the same must happen here, immediately and without partial writes.

const (
	duplicateTaskShardID     = int32(1)
	duplicateTaskNamespaceID = "ns-duplicate-task"

	// A healthy transaction takes milliseconds; the retry loop this guards
	// against takes as long as the context allows.
	duplicateTaskFailFastLimit = 10 * time.Second
	duplicateTaskCallTimeout   = 40 * time.Second
)

type duplicateTaskEnv struct {
	raw  client.Database
	exec persistence.ExecutionStore
}

func newDuplicateTaskEnv(t *testing.T) *duplicateTaskEnv {
	t.Helper()

	cfg := newMongoTestConfig(fmt.Sprintf("test_dup_task_%d", time.Now().UnixNano()))
	uri := fmt.Sprintf("mongodb://%s:%s@%s/%s?replicaSet=%s&authSource=%s",
		cfg.User, cfg.Password, strings.Join(cfg.Hosts, ","), cfg.DatabaseName, cfg.ReplicaSet, cfg.AuthSource)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cl, err := client.NewClient(ctx, uri)
	if err != nil {
		t.Skipf("MongoDB not available: %v", err)
	}
	if err := cl.Ping(ctx); err != nil {
		_ = cl.Close(ctx)
		t.Skipf("MongoDB not reachable at %v: %v", cfg.Hosts, err)
	}

	raw := cl.Database(cfg.DatabaseName)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = raw.RunCommand(cctx, bson.D{{Key: "dropDatabase", Value: 1}}).Err()
		_ = cl.Close(cctx)
	})

	_, err = raw.Collection("shards").InsertOne(ctx, bson.M{
		"_id":           duplicateTaskShardID,
		"range_id":      int64(1),
		"data":          []byte{},
		"data_encoding": enumspb.ENCODING_TYPE_PROTO3.String(),
	})
	require.NoError(t, err)

	exec, err := mongodb.NewExecutionStore(raw, cl, cfg, log.NewNoopLogger(), metrics.NoopMetricsHandler, true)
	require.NoError(t, err)

	return &duplicateTaskEnv{raw: raw, exec: exec}
}

func duplicateTaskState(workflowID string) *persistencespb.WorkflowExecutionState {
	return &persistencespb.WorkflowExecutionState{
		RunId:           "run-" + workflowID,
		CreateRequestId: "req-" + workflowID,
		State:           enumsspb.WORKFLOW_EXECUTION_STATE_RUNNING,
		Status:          enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
	}
}

func duplicateTransferTask(taskID int64) map[tasks.Category][]persistence.InternalHistoryTask {
	return map[tasks.Category][]persistence.InternalHistoryTask{
		tasks.CategoryTransfer: {{
			Key:  tasks.NewImmediateKey(taskID),
			Blob: &commonpb.DataBlob{Data: []byte("task"), EncodingType: enumspb.ENCODING_TYPE_PROTO3},
		}},
	}
}

// create writes a new execution (DBRecordVersion 1) carrying one transfer task.
func (e *duplicateTaskEnv) create(workflowID string, taskID int64) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duplicateTaskCallTimeout)
	defer cancel()
	start := time.Now()
	_, err := e.exec.CreateWorkflowExecution(ctx, &persistence.InternalCreateWorkflowExecutionRequest{
		ShardID: duplicateTaskShardID,
		RangeID: 1,
		Mode:    persistence.CreateWorkflowModeBrandNew,
		NewWorkflowSnapshot: persistence.InternalWorkflowSnapshot{
			NamespaceID:      duplicateTaskNamespaceID,
			WorkflowID:       workflowID,
			RunID:            "run-" + workflowID,
			ExecutionInfo:    &persistencespb.WorkflowExecutionInfo{},
			ExecutionState:   duplicateTaskState(workflowID),
			NextEventID:      3,
			LastWriteVersion: 1,
			DBRecordVersion:  1,
			Tasks:            duplicateTransferTask(taskID),
		},
	})
	return time.Since(start), err
}

// update moves the execution to DBRecordVersion 2 and carries one transfer task.
func (e *duplicateTaskEnv) update(workflowID string, taskID int64) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duplicateTaskCallTimeout)
	defer cancel()
	start := time.Now()
	err := e.exec.UpdateWorkflowExecution(ctx, &persistence.InternalUpdateWorkflowExecutionRequest{
		ShardID: duplicateTaskShardID,
		RangeID: 1,
		Mode:    persistence.UpdateWorkflowModeIgnoreCurrent,
		UpdateWorkflowMutation: persistence.InternalWorkflowMutation{
			NamespaceID:      duplicateTaskNamespaceID,
			WorkflowID:       workflowID,
			RunID:            "run-" + workflowID,
			ExecutionInfo:    &persistencespb.WorkflowExecutionInfo{},
			ExecutionState:   duplicateTaskState(workflowID),
			NextEventID:      4,
			LastWriteVersion: 1,
			DBRecordVersion:  2,
			Tasks:            duplicateTransferTask(taskID),
		},
	})
	return time.Since(start), err
}

func (e *duplicateTaskEnv) count(t *testing.T, collection string, filter bson.M) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := e.raw.Collection(collection).CountDocuments(ctx, filter)
	require.NoError(t, err)
	return n
}

func requireDuplicateTaskFailure(t *testing.T, elapsed time.Duration, err error) {
	t.Helper()
	t.Logf("elapsed=%v err=%v", elapsed, err)
	require.Error(t, err, "a history task key collision must fail the transaction")
	require.Lessf(t, elapsed, duplicateTaskFailFastLimit,
		"the transaction must fail immediately instead of being retried by the driver (err=%v)", err)
	var unavailable *serviceerror.Unavailable
	require.Truef(t, errors.As(err, &unavailable), "expected Unavailable, got %T: %v", err, err)
	require.Contains(t, err.Error(), "already exists")
}

// Control: distinct task keys are written.
func TestDuplicateTaskInTransaction_DistinctTasksAreWritten(t *testing.T) {
	env := newDuplicateTaskEnv(t)

	_, err := env.create("wf-a", 100)
	require.NoError(t, err)
	_, err = env.create("wf-b", 101)
	require.NoError(t, err)
	_, err = env.update("wf-a", 102)
	require.NoError(t, err)

	require.EqualValues(t, 3, env.count(t, "transfer_tasks", bson.M{"shard_id": duplicateTaskShardID}))
}

func TestDuplicateTaskInTransaction_CreateFailsFast(t *testing.T) {
	env := newDuplicateTaskEnv(t)

	_, err := env.create("wf-a", 100)
	require.NoError(t, err)

	elapsed, err := env.create("wf-b", 100)
	requireDuplicateTaskFailure(t, elapsed, err)

	// Nothing of the failed transaction may be visible.
	require.Zero(t, env.count(t, "executions", bson.M{"workflow_id": "wf-b"}))
	require.Zero(t, env.count(t, "current_executions", bson.M{"workflow_id": "wf-b"}))
	require.EqualValues(t, 1, env.count(t, "transfer_tasks", bson.M{"shard_id": duplicateTaskShardID}))
}

func TestDuplicateTaskInTransaction_UpdateFailsFast(t *testing.T) {
	env := newDuplicateTaskEnv(t)

	_, err := env.create("wf-a", 100)
	require.NoError(t, err)

	elapsed, err := env.update("wf-a", 100)
	requireDuplicateTaskFailure(t, elapsed, err)

	// The mutation must not have been applied.
	require.EqualValues(t, 1, env.count(t, "executions", bson.M{"workflow_id": "wf-a", "db_record_version": int64(1)}))
	require.EqualValues(t, 1, env.count(t, "transfer_tasks", bson.M{"shard_id": duplicateTaskShardID}))
}
