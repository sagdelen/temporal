package mongodb_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	enumsspb "go.temporal.io/server/api/enums/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/mongodb"
	"go.temporal.io/server/common/persistence/mongodb/client"
)

// These tests cover buffered event persistence against a real MongoDB replica
// set (transactions are required). They are skipped when MongoDB is not
// reachable. See MONGODB_SEEDS / TEMPORAL_MONGODB_TEST_* in
// test_config_test.go for connection settings.
//
// Buffered events are append-only between flushes: every transaction that
// buffers events sends only its new batch in NewBufferedEvents and expects the
// earlier batches to still be there. SQL inserts one buffered_events row per
// batch and Cassandra appends to buffered_events_list.

const (
	bufferedEventsShardID     = int32(1)
	bufferedEventsNamespaceID = "ns-buffered-events"
)

type bufferedEventsEnv struct {
	raw  client.Database
	exec persistence.ExecutionStore
}

func newBufferedEventsEnv(t *testing.T) *bufferedEventsEnv {
	t.Helper()

	cfg := newMongoTestConfig(fmt.Sprintf("test_buffered_%d", time.Now().UnixNano()))
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
		"_id":           bufferedEventsShardID,
		"range_id":      int64(1),
		"data":          []byte{},
		"data_encoding": enumspb.ENCODING_TYPE_PROTO3.String(),
	})
	require.NoError(t, err)

	exec, err := mongodb.NewExecutionStore(raw, cl, cfg, log.NewNoopLogger(), metrics.NoopMetricsHandler, true)
	require.NoError(t, err)

	return &bufferedEventsEnv{raw: raw, exec: exec}
}

func bufferedEventsState(workflowID string) *persistencespb.WorkflowExecutionState {
	return &persistencespb.WorkflowExecutionState{
		RunId:           "run-" + workflowID,
		CreateRequestId: "req-" + workflowID,
		State:           enumsspb.WORKFLOW_EXECUTION_STATE_RUNNING,
		Status:          enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
	}
}

func bufferedBatch(payload string) *commonpb.DataBlob {
	return &commonpb.DataBlob{Data: []byte(payload), EncodingType: enumspb.ENCODING_TYPE_PROTO3}
}

// create writes a new execution with DBRecordVersion 1.
func (e *bufferedEventsEnv) create(t *testing.T, workflowID string) {
	t.Helper()
	_, err := e.exec.CreateWorkflowExecution(context.Background(), &persistence.InternalCreateWorkflowExecutionRequest{
		ShardID: bufferedEventsShardID,
		RangeID: 1,
		Mode:    persistence.CreateWorkflowModeBrandNew,
		NewWorkflowSnapshot: persistence.InternalWorkflowSnapshot{
			NamespaceID:      bufferedEventsNamespaceID,
			WorkflowID:       workflowID,
			RunID:            "run-" + workflowID,
			ExecutionInfo:    &persistencespb.WorkflowExecutionInfo{},
			ExecutionState:   bufferedEventsState(workflowID),
			NextEventID:      3,
			LastWriteVersion: 1,
			DBRecordVersion:  1,
		},
	})
	require.NoError(t, err)
}

// update applies one mutation; dbRecordVersion must be the previous one plus 1.
func (e *bufferedEventsEnv) update(t *testing.T, workflowID string, dbRecordVersion int64, newBatch *commonpb.DataBlob, clearBuffer bool) {
	t.Helper()
	err := e.exec.UpdateWorkflowExecution(context.Background(), &persistence.InternalUpdateWorkflowExecutionRequest{
		ShardID: bufferedEventsShardID,
		RangeID: 1,
		Mode:    persistence.UpdateWorkflowModeIgnoreCurrent,
		UpdateWorkflowMutation: persistence.InternalWorkflowMutation{
			NamespaceID:         bufferedEventsNamespaceID,
			WorkflowID:          workflowID,
			RunID:               "run-" + workflowID,
			ExecutionInfo:       &persistencespb.WorkflowExecutionInfo{},
			ExecutionState:      bufferedEventsState(workflowID),
			NextEventID:         3,
			LastWriteVersion:    1,
			DBRecordVersion:     dbRecordVersion,
			NewBufferedEvents:   newBatch,
			ClearBufferedEvents: clearBuffer,
		},
	})
	require.NoError(t, err)
}

// buffered returns the payloads of the buffered event batches as read back
// through GetWorkflowExecution.
func (e *bufferedEventsEnv) buffered(t *testing.T, workflowID string) []string {
	t.Helper()
	resp, err := e.exec.GetWorkflowExecution(context.Background(), &persistence.GetWorkflowExecutionRequest{
		ShardID:     bufferedEventsShardID,
		NamespaceID: bufferedEventsNamespaceID,
		WorkflowID:  workflowID,
		RunID:       "run-" + workflowID,
	})
	require.NoError(t, err)
	payloads := make([]string, 0, len(resp.State.BufferedEvents))
	for _, blob := range resp.State.BufferedEvents {
		require.Equal(t, enumspb.ENCODING_TYPE_PROTO3, blob.GetEncodingType())
		payloads = append(payloads, string(blob.GetData()))
	}
	return payloads
}

// storedType returns the BSON type of the stored buffered_events field
// (0 when the field is absent).
func (e *bufferedEventsEnv) storedType(t *testing.T, workflowID string) bsontype.Type {
	t.Helper()
	var doc struct {
		BufferedEvents bson.RawValue `bson:"buffered_events"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.raw.Collection("executions").FindOne(ctx, bson.M{"workflow_id": workflowID}).Decode(&doc))
	return doc.BufferedEvents.Type
}

// Each transaction sends only its own new batch; earlier batches must survive.
func TestBufferedEvents_AppendAcrossUpdates(t *testing.T) {
	env := newBufferedEventsEnv(t)
	const wf = "wf-append"
	env.create(t, wf)
	require.Empty(t, env.buffered(t, wf))

	env.update(t, wf, 2, bufferedBatch("batch-1"), false)
	require.Equal(t, []string{"batch-1"}, env.buffered(t, wf))

	env.update(t, wf, 3, bufferedBatch("batch-2"), false)
	require.Equal(t, []string{"batch-1", "batch-2"}, env.buffered(t, wf),
		"the second buffered batch must be appended, not replace the first")

	// A mutation that does not touch the buffer leaves it alone.
	env.update(t, wf, 4, nil, false)
	require.Equal(t, []string{"batch-1", "batch-2"}, env.buffered(t, wf))

	env.update(t, wf, 5, bufferedBatch("batch-3"), false)
	require.Equal(t, []string{"batch-1", "batch-2", "batch-3"}, env.buffered(t, wf))
}

func TestBufferedEvents_ClearEmptiesBuffer(t *testing.T) {
	env := newBufferedEventsEnv(t)
	const wf = "wf-clear"
	env.create(t, wf)
	env.update(t, wf, 2, bufferedBatch("batch-1"), false)
	env.update(t, wf, 3, bufferedBatch("batch-2"), false)

	env.update(t, wf, 4, nil, true)
	require.Empty(t, env.buffered(t, wf))

	env.update(t, wf, 5, bufferedBatch("batch-3"), false)
	require.Equal(t, []string{"batch-3"}, env.buffered(t, wf))
}

// The SQL stores delete the buffered rows and then insert the new batch when a
// mutation carries both ClearBufferedEvents and NewBufferedEvents.
func TestBufferedEvents_ClearAndNewKeepsNewBatch(t *testing.T) {
	env := newBufferedEventsEnv(t)
	const wf = "wf-clear-and-new"
	env.create(t, wf)
	env.update(t, wf, 2, bufferedBatch("batch-1"), false)

	env.update(t, wf, 3, bufferedBatch("batch-2"), true)
	require.Equal(t, []string{"batch-2"}, env.buffered(t, wf))
}

// Documents written before this fix hold a single blob sub-document in
// buffered_events. They must still be readable, and appendable.
func TestBufferedEvents_LegacySingleBlobDocument(t *testing.T) {
	env := newBufferedEventsEnv(t)
	const wf = "wf-legacy"
	env.create(t, wf)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := env.raw.Collection("executions").UpdateOne(ctx,
		bson.M{"workflow_id": wf},
		bson.M{"$set": bson.M{"buffered_events": bson.M{
			"data":     []byte("legacy"),
			"encoding": enumspb.ENCODING_TYPE_PROTO3.String(),
		}}},
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.MatchedCount)

	require.Equal(t, []string{"legacy"}, env.buffered(t, wf))

	env.update(t, wf, 2, bufferedBatch("batch-2"), false)
	require.Equal(t, []string{"legacy", "batch-2"}, env.buffered(t, wf))
}

// A single buffered batch keeps the pre-fix on-disk shape (one sub-document),
// so servers that have not been upgraded yet can still read it. Only two or
// more batches, which those servers could not represent, are stored as an array.
func TestBufferedEvents_StoredShape(t *testing.T) {
	env := newBufferedEventsEnv(t)
	const wf = "wf-shape"
	env.create(t, wf)

	env.update(t, wf, 2, bufferedBatch("batch-1"), false)
	require.Equal(t, bsontype.EmbeddedDocument, env.storedType(t, wf))

	env.update(t, wf, 3, bufferedBatch("batch-2"), false)
	require.Equal(t, bsontype.Array, env.storedType(t, wf))

	env.update(t, wf, 4, nil, true)
	stored := env.storedType(t, wf)
	require.Truef(t, stored == bsontype.Null || stored == bsontype.Type(0),
		"cleared buffer should be stored as null or absent, got %v", stored)
}
