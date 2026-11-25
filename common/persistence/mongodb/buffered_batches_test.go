package mongodb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/server/common/persistence/serialization"
)

// These tests need no MongoDB server.

func serializedEventBatch(t *testing.T, eventIDs ...int64) dataBlobDoc {
	t.Helper()
	events := make([]*historypb.HistoryEvent, 0, len(eventIDs))
	for _, id := range eventIDs {
		events = append(events, &historypb.HistoryEvent{
			EventId:   id,
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionSignaledEventAttributes{
				WorkflowExecutionSignaledEventAttributes: &historypb.WorkflowExecutionSignaledEventAttributes{
					SignalName: "signal",
					Identity:   "test",
				},
			},
		})
	}
	blob, err := serialization.NewSerializer().SerializeEvents(events)
	require.NoError(t, err)
	doc := dataBlobToDoc(blob)
	require.NotNil(t, doc)
	return *doc
}

// Batches merged by the store must decode, with the serializer the history
// service uses, to every buffered event in the order the events were buffered.
func TestAppendBufferedBatch_MergedBatchDecodesToAllEvents(t *testing.T) {
	var buffer bufferedBatches
	buffer = appendBufferedBatch(buffer, serializedEventBatch(t, 5))
	buffer = appendBufferedBatch(buffer, serializedEventBatch(t, 6, 7))
	buffer = appendBufferedBatch(buffer, serializedEventBatch(t, 8))
	require.Len(t, buffer, 1, "proto3 batches are merged into one blob")

	events, err := serialization.NewSerializer().DeserializeEvents(dataBlobDocToBlob(&buffer[0]))
	require.NoError(t, err)
	ids := make([]int64, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.GetEventId())
		require.Equal(t, "signal", event.GetWorkflowExecutionSignaledEventAttributes().GetSignalName())
	}
	require.Equal(t, []int64{5, 6, 7, 8}, ids)
}

func TestAppendBufferedBatch_DoesNotModifyEarlierBlobs(t *testing.T) {
	first := serializedEventBatch(t, 5)
	firstCopy := append([]byte(nil), first.Data...)
	second := serializedEventBatch(t, 6)
	secondCopy := append([]byte(nil), second.Data...)

	buffer := appendBufferedBatch(bufferedBatches{first}, second)
	require.Len(t, buffer, 1)
	require.Equal(t, firstCopy, first.Data)
	require.Equal(t, secondCopy, second.Data)
	require.Equal(t, append(firstCopy, secondCopy...), buffer[0].Data)
}

// A batch in an encoding that cannot be joined byte-wise is kept as its own
// entry instead of being merged or dropped.
func TestAppendBufferedBatch_OtherEncodingIsKeptSeparate(t *testing.T) {
	jsonEncoding := enumspb.ENCODING_TYPE_JSON.String()
	proto3 := serializedEventBatch(t, 5)

	buffer := appendBufferedBatch(bufferedBatches{proto3}, dataBlobDoc{Data: []byte(`{}`), Encoding: jsonEncoding})
	require.Len(t, buffer, 2)

	buffer = appendBufferedBatch(buffer, serializedEventBatch(t, 6))
	require.Len(t, buffer, 3)
	require.Equal(t, jsonEncoding, buffer[1].Encoding)
}

func TestBufferedBatches_BSONShapes(t *testing.T) {
	type holder struct {
		BufferedEvents bufferedBatches `bson:"buffered_events"`
	}
	one := dataBlobDoc{Data: []byte("a"), Encoding: enumspb.ENCODING_TYPE_PROTO3.String()}
	two := dataBlobDoc{Data: []byte("b"), Encoding: enumspb.ENCODING_TYPE_JSON.String()}

	testCases := []struct {
		name     string
		batches  bufferedBatches
		wantType bsontype.Type
	}{
		{name: "empty is null", batches: nil, wantType: bsontype.Null},
		{name: "one batch is a sub-document", batches: bufferedBatches{one}, wantType: bsontype.EmbeddedDocument},
		{name: "several batches are an array", batches: bufferedBatches{one, two}, wantType: bsontype.Array},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := bson.Marshal(holder{BufferedEvents: tc.batches})
			require.NoError(t, err)
			require.Equal(t, tc.wantType, bson.Raw(raw).Lookup("buffered_events").Type)

			var decoded holder
			require.NoError(t, bson.Unmarshal(raw, &decoded))
			require.Len(t, decoded.BufferedEvents, len(tc.batches))
			for i := range tc.batches {
				require.Equal(t, tc.batches[i], decoded.BufferedEvents[i])
			}
		})
	}
}

// A document that never had the field, and one written by an older server
// with a single blob, both decode.
func TestBufferedBatches_DecodesLegacyDocuments(t *testing.T) {
	type holder struct {
		BufferedEvents bufferedBatches `bson:"buffered_events"`
	}

	raw, err := bson.Marshal(bson.M{"other": 1})
	require.NoError(t, err)
	var missing holder
	require.NoError(t, bson.Unmarshal(raw, &missing))
	require.Empty(t, missing.BufferedEvents)

	raw, err = bson.Marshal(bson.M{"buffered_events": bson.M{
		"data":     []byte("legacy"),
		"encoding": enumspb.ENCODING_TYPE_PROTO3.String(),
	}})
	require.NoError(t, err)
	var legacy holder
	require.NoError(t, bson.Unmarshal(raw, &legacy))
	require.Len(t, legacy.BufferedEvents, 1)
	require.Equal(t, []byte("legacy"), legacy.BufferedEvents[0].Data)

	raw, err = bson.Marshal(bson.M{"buffered_events": "not a blob"})
	require.NoError(t, err)
	var invalid holder
	require.Error(t, bson.Unmarshal(raw, &invalid))
}
