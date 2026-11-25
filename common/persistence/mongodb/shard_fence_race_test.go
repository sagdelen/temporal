package mongodb_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	enumsspb "go.temporal.io/server/api/enums/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/mongodb"
	"go.temporal.io/server/common/persistence/mongodb/client"
	"go.temporal.io/server/service/history/tasks"
)

// These tests exercise the shard range_id write fence against a real MongoDB
// replica set (transactions are required). They are skipped when MongoDB is
// not reachable. See MONGODB_SEEDS / TEMPORAL_MONGODB_TEST_* in
// test_config_test.go for connection settings.
//
// SQL holds a shared row lock on the shard row for the whole transaction and
// Cassandra adds "IF range_id = ?" to every batch, so in both a stale shard
// owner can never commit after the range has moved. The same must hold here.

const fenceShardID = int32(1)

type (
	// hookDatabase wraps a real database and runs a one-shot hook right after
	// the first operation executed inside a transaction, i.e. once the
	// transaction snapshot exists and before it commits.
	hookDatabase struct {
		client.Database
		mu   sync.Mutex
		hook func()
	}

	hookCollection struct {
		client.Collection
		db *hookDatabase
	}

	fenceEnv struct {
		cfg    config.MongoDB
		raw    client.Database
		hooked *hookDatabase
		exec   persistence.ExecutionStore
		shard  persistence.ShardStore
	}
)

func (d *hookDatabase) Collection(name string) client.Collection {
	return &hookCollection{Collection: d.Database.Collection(name), db: d}
}

func (d *hookDatabase) arm(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hook = fn
}

func (d *hookDatabase) fire(ctx context.Context) {
	if mongo.SessionFromContext(ctx) == nil {
		return
	}
	d.mu.Lock()
	fn := d.hook
	d.hook = nil
	d.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (c *hookCollection) FindOne(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) client.SingleResult {
	res := c.Collection.FindOne(ctx, filter, opts...)
	c.db.fire(ctx)
	return res
}

func (c *hookCollection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (client.Cursor, error) {
	cur, err := c.Collection.Find(ctx, filter, opts...)
	c.db.fire(ctx)
	return cur, err
}

func (c *hookCollection) InsertOne(ctx context.Context, document interface{}, opts ...*options.InsertOneOptions) (*mongo.InsertOneResult, error) {
	res, err := c.Collection.InsertOne(ctx, document, opts...)
	c.db.fire(ctx)
	return res, err
}

func (c *hookCollection) UpdateOne(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	res, err := c.Collection.UpdateOne(ctx, filter, update, opts...)
	c.db.fire(ctx)
	return res, err
}

func newFenceEnv(t *testing.T) *fenceEnv {
	t.Helper()

	cfg := newMongoTestConfig(fmt.Sprintf("test_fence_%d", time.Now().UnixNano()))
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
		"_id":           fenceShardID,
		"range_id":      int64(1),
		"data":          []byte{},
		"data_encoding": enumspb.ENCODING_TYPE_PROTO3.String(),
	})
	require.NoError(t, err)

	hooked := &hookDatabase{Database: raw}
	logger := log.NewNoopLogger()

	exec, err := mongodb.NewExecutionStore(hooked, cl, cfg, logger, metrics.NoopMetricsHandler, true)
	require.NoError(t, err)
	shard, err := mongodb.NewShardStore(raw, cfg, logger, "fence-test", true)
	require.NoError(t, err)

	return &fenceEnv{cfg: cfg, raw: raw, hooked: hooked, exec: exec, shard: shard}
}

// bumpRange moves the shard from range 1 to range 2 through the production
// UpdateShard path, as a new shard owner would.
func (e *fenceEnv) bumpRange(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e.shard.UpdateShard(ctx, &persistence.InternalUpdateShardRequest{
		ShardID:         fenceShardID,
		RangeID:         2,
		PreviousRangeID: 1,
		ShardInfo:       &commonpb.DataBlob{Data: []byte("shard"), EncodingType: enumspb.ENCODING_TYPE_PROTO3},
	})
}

func (e *fenceEnv) count(t *testing.T, collection string, filter bson.M) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := e.raw.Collection(collection).CountDocuments(ctx, filter)
	require.NoError(t, err)
	return n
}

func fenceTask(taskID int64) persistence.InternalHistoryTask {
	return persistence.InternalHistoryTask{
		Key:  tasks.NewImmediateKey(taskID),
		Blob: &commonpb.DataBlob{Data: []byte("task"), EncodingType: enumspb.ENCODING_TYPE_PROTO3},
	}
}

func fenceCreateRequest(rangeID int64, workflowID string) *persistence.InternalCreateWorkflowExecutionRequest {
	runID := "run-" + workflowID
	return &persistence.InternalCreateWorkflowExecutionRequest{
		ShardID: fenceShardID,
		RangeID: rangeID,
		Mode:    persistence.CreateWorkflowModeBrandNew,
		NewWorkflowSnapshot: persistence.InternalWorkflowSnapshot{
			NamespaceID:   "ns-fence",
			WorkflowID:    workflowID,
			RunID:         runID,
			ExecutionInfo: &persistencespb.WorkflowExecutionInfo{},
			ExecutionState: &persistencespb.WorkflowExecutionState{
				RunId:           runID,
				CreateRequestId: "req-" + workflowID,
				State:           enumsspb.WORKFLOW_EXECUTION_STATE_RUNNING,
				Status:          enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			},
			NextEventID:      3,
			LastWriteVersion: 1,
			DBRecordVersion:  1,
			Tasks: map[tasks.Category][]persistence.InternalHistoryTask{
				tasks.CategoryTransfer: {fenceTask(100)},
			},
		},
	}
}

// Control: the range moved before the stale owner even starts. This must be
// rejected by any fence implementation.
func TestShardFence_StaleRangeRejectedBeforeTransaction(t *testing.T) {
	env := newFenceEnv(t)
	require.NoError(t, env.bumpRange(10*time.Second))

	_, err := env.exec.CreateWorkflowExecution(context.Background(), fenceCreateRequest(1, "wf-sequential"))

	var lost *persistence.ShardOwnershipLostError
	require.Truef(t, errors.As(err, &lost), "expected ShardOwnershipLostError, got %v", err)
	require.Zero(t, env.count(t, "executions", bson.M{"workflow_id": "wf-sequential"}))
	require.Zero(t, env.count(t, "transfer_tasks", bson.M{"shard_id": fenceShardID}))
}

// The race: the stale owner's transaction is open (snapshot taken, range 1
// observed) when the new owner moves the range to 2. The stale transaction
// must not commit its execution and task writes.
func TestShardFence_RangeBumpDuringTransaction(t *testing.T) {
	env := newFenceEnv(t)

	var bumpErr error
	hookRan := false
	env.hooked.arm(func() {
		hookRan = true
		// Short timeout: a fence that write-locks the shard document makes
		// the ownership change wait for the transaction, which is also safe.
		bumpErr = env.bumpRange(3 * time.Second)
	})

	_, err := env.exec.CreateWorkflowExecution(context.Background(), fenceCreateRequest(1, "wf-race"))
	require.True(t, hookRan, "hook never ran inside the transaction")

	executions := env.count(t, "executions", bson.M{"workflow_id": "wf-race"})
	transferTasks := env.count(t, "transfer_tasks", bson.M{"shard_id": fenceShardID})
	rangeNow := env.count(t, "shards", bson.M{"_id": fenceShardID, "range_id": int64(2)})
	t.Logf("bumpErr=%v createErr=%v executions=%d transfer_tasks=%d shard_at_range_2=%d",
		bumpErr, err, executions, transferTasks, rangeNow)

	if bumpErr != nil {
		// The ownership change could not get past the open transaction, so the
		// write legitimately belongs to range 1 (SQL shared-lock semantics).
		require.NoError(t, err)
		return
	}

	var lost *persistence.ShardOwnershipLostError
	require.Truef(t, errors.As(err, &lost),
		"stale owner (range 1) committed after the shard moved to range 2: createErr=%v executions=%d transfer_tasks=%d",
		err, executions, transferTasks)
	require.Zero(t, executions, "stale owner wrote an execution document")
	require.Zero(t, transferTasks, "stale owner wrote a transfer task")
}

// AddHistoryTasks carries a RangeID and is fenced in SQL and Cassandra.
func TestShardFence_AddHistoryTasksStaleRange(t *testing.T) {
	env := newFenceEnv(t)
	require.NoError(t, env.bumpRange(10*time.Second))

	err := env.exec.AddHistoryTasks(context.Background(), &persistence.InternalAddHistoryTasksRequest{
		ShardID:     fenceShardID,
		RangeID:     1,
		NamespaceID: "ns-fence",
		WorkflowID:  "wf-add-tasks",
		Tasks: map[tasks.Category][]persistence.InternalHistoryTask{
			tasks.CategoryTransfer: {fenceTask(200)},
		},
	})

	transferTasks := env.count(t, "transfer_tasks", bson.M{"shard_id": fenceShardID})
	t.Logf("addErr=%v transfer_tasks=%d", err, transferTasks)

	var lost *persistence.ShardOwnershipLostError
	require.Truef(t, errors.As(err, &lost),
		"AddHistoryTasks with stale range 1 was accepted after the shard moved to range 2: err=%v transfer_tasks=%d",
		err, transferTasks)
	require.Zero(t, transferTasks, "stale owner wrote a transfer task")
}
