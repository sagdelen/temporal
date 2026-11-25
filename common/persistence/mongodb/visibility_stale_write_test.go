package mongodb_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence/mongodb"
	"go.temporal.io/server/common/persistence/mongodb/client"
	"go.temporal.io/server/common/persistence/visibility/store"
)

// These tests cover version-guarded visibility writes against a real MongoDB
// replica set (transactions are required). They are skipped when MongoDB is
// not reachable. See MONGODB_SEEDS / TEMPORAL_MONGODB_TEST_* in
// test_config_test.go for connection settings.
//
// Visibility tasks can be processed out of order and more than once. Each
// write carries a version (the task ID) and a write that is older than what is
// stored must be dropped without an error, as the SQL stores do with their
// _version column. Returning an error makes the visibility queue retry a task
// that can never succeed.

const visibilityStaleNamespaceID = "ns-visibility-stale"

type (
	visibilityStaleEnv struct {
		raw client.Database
		vis store.VisibilityStore
	}

	storedVisibilityDoc struct {
		Version   int64      `bson:"version"`
		Status    int32      `bson:"status"`
		CloseTime *time.Time `bson:"close_time"`
	}
)

func newVisibilityStaleEnv(t *testing.T) *visibilityStaleEnv {
	t.Helper()

	cfg := newMongoTestConfig(fmt.Sprintf("test_vis_stale_%d", time.Now().UnixNano()))
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

	vis, err := mongodb.NewVisibilityStore(raw, cl, cfg, log.NewNoopLogger(), metrics.NoopMetricsHandler, nil, nil, nil, true)
	require.NoError(t, err)

	return &visibilityStaleEnv{raw: raw, vis: vis}
}

var visibilityStaleStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func visibilityStaleBase(workflowID string, version int64, status enumspb.WorkflowExecutionStatus) *store.InternalVisibilityRequestBase {
	return &store.InternalVisibilityRequestBase{
		NamespaceID:      visibilityStaleNamespaceID,
		WorkflowID:       workflowID,
		RunID:            "run-" + workflowID,
		WorkflowTypeName: "stale-write-test",
		StartTime:        visibilityStaleStart,
		ExecutionTime:    visibilityStaleStart,
		Status:           status,
		TaskID:           version,
		ShardID:          1,
		TaskQueue:        "tq",
		RootWorkflowID:   workflowID,
		RootRunID:        "run-" + workflowID,
	}
}

func (e *visibilityStaleEnv) started(workflowID string, version int64) error {
	return e.vis.RecordWorkflowExecutionStarted(context.Background(), &store.InternalRecordWorkflowExecutionStartedRequest{
		InternalVisibilityRequestBase: visibilityStaleBase(workflowID, version, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
	})
}

func (e *visibilityStaleEnv) upsert(workflowID string, version int64) error {
	return e.vis.UpsertWorkflowExecution(context.Background(), &store.InternalUpsertWorkflowExecutionRequest{
		InternalVisibilityRequestBase: visibilityStaleBase(workflowID, version, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING),
	})
}

func (e *visibilityStaleEnv) closed(workflowID string, version int64) error {
	return e.vis.RecordWorkflowExecutionClosed(context.Background(), &store.InternalRecordWorkflowExecutionClosedRequest{
		InternalVisibilityRequestBase: visibilityStaleBase(workflowID, version, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED),
		CloseTime:                     visibilityStaleStart.Add(time.Minute),
		HistoryLength:                 10,
		HistorySizeBytes:              100,
		ExecutionDuration:             time.Minute,
		StateTransitionCount:          5,
	})
}

func (e *visibilityStaleEnv) stored(t *testing.T, workflowID string) storedVisibilityDoc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var doc storedVisibilityDoc
	require.NoError(t, e.raw.Collection("visibility_executions").FindOne(ctx, bson.M{
		"namespace_id": visibilityStaleNamespaceID,
		"workflow_id":  workflowID,
	}).Decode(&doc))
	return doc
}

// Control: writes arriving in order are all applied.
func TestVisibilityStaleWrite_InOrderWritesApply(t *testing.T) {
	env := newVisibilityStaleEnv(t)
	const wf = "wf-in-order"

	require.NoError(t, env.started(wf, 1))
	require.NoError(t, env.upsert(wf, 2))
	doc := env.stored(t, wf)
	require.EqualValues(t, 2, doc.Version)
	require.EqualValues(t, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, doc.Status)

	require.NoError(t, env.closed(wf, 3))
	doc = env.stored(t, wf)
	require.EqualValues(t, 3, doc.Version)
	require.EqualValues(t, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, doc.Status)
	require.NotNil(t, doc.CloseTime)
}

// An upsert that is older than the stored close record must be dropped
// silently and must not reopen the execution.
func TestVisibilityStaleWrite_StaleUpsertAfterCloseIsIgnored(t *testing.T) {
	env := newVisibilityStaleEnv(t)
	const wf = "wf-stale-upsert"

	require.NoError(t, env.closed(wf, 10))
	require.NoError(t, env.upsert(wf, 5), "a stale upsert must be dropped without an error")

	doc := env.stored(t, wf)
	require.EqualValues(t, 10, doc.Version)
	require.EqualValues(t, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, doc.Status)
	require.NotNil(t, doc.CloseTime)
}

// A close record that is older than what is stored must be dropped silently.
func TestVisibilityStaleWrite_StaleCloseIsIgnored(t *testing.T) {
	env := newVisibilityStaleEnv(t)
	const wf = "wf-stale-close"

	require.NoError(t, env.upsert(wf, 10))
	require.NoError(t, env.closed(wf, 5), "a stale close must be dropped without an error")

	doc := env.stored(t, wf)
	require.EqualValues(t, 10, doc.Version)
	require.EqualValues(t, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, doc.Status)
	require.Nil(t, doc.CloseTime)
}

// A start record for an execution that already has a record is a no-op.
func TestVisibilityStaleWrite_DuplicateStartIsIgnored(t *testing.T) {
	env := newVisibilityStaleEnv(t)
	const wf = "wf-duplicate-start"

	require.NoError(t, env.started(wf, 1))
	require.NoError(t, env.started(wf, 1), "a repeated start must be dropped without an error")

	require.NoError(t, env.closed(wf, 3))
	require.NoError(t, env.started(wf, 1), "a start arriving after the close must be dropped without an error")

	doc := env.stored(t, wf)
	require.EqualValues(t, 3, doc.Version)
	require.EqualValues(t, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, doc.Status)
}

// Two writers race to create the record. Whichever loses the insert must not
// be mistaken for a stale write: the newer version has to end up stored.
func TestVisibilityStaleWrite_ConcurrentFirstWritesKeepNewest(t *testing.T) {
	env := newVisibilityStaleEnv(t)

	const workflows = 40
	var wg sync.WaitGroup
	errs := make(chan error, workflows*2)
	for i := 0; i < workflows; i++ {
		wf := fmt.Sprintf("wf-concurrent-%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := env.upsert(wf, 5); err != nil {
				errs <- fmt.Errorf("%s upsert: %w", wf, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := env.closed(wf, 7); err != nil {
				errs <- fmt.Errorf("%s close: %w", wf, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for i := 0; i < workflows; i++ {
		wf := fmt.Sprintf("wf-concurrent-%d", i)
		doc := env.stored(t, wf)
		require.EqualValuesf(t, 7, doc.Version, "%s: newest version must win", wf)
		require.EqualValuesf(t, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, doc.Status, "%s", wf)
	}
}
