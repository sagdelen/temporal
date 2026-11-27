package mongodb

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
	"go.temporal.io/server/common/persistence"
)

// These tests need no MongoDB server.

func TestIsSizeLimitError(t *testing.T) {
	for _, code := range []int32{mongoBSONObjectTooLargeCode, mongoTransactionTooLargeCode, mongoUpdateResultTooLargeCode} {
		require.True(t, isSizeLimitError(mongo.CommandError{Code: code, Message: "too large"}), code)
		require.True(t, isSizeLimitError(mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: int(code)}}}), code)
	}
	require.True(t, isSizeLimitError(errors.New("document is too large")))

	require.False(t, isSizeLimitError(nil))
	require.False(t, isSizeLimitError(mongo.CommandError{Code: 112, Message: "WriteConflict"}))
	require.False(t, isSizeLimitError(errors.New("connection reset")))
}

// An oversized write must reach the history service as a
// TransactionSizeLimitError, not as a retryable Unavailable.
func TestNormalizeTransactionError_SizeLimit(t *testing.T) {
	err := normalizeTransactionError(mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: mongoBSONObjectTooLargeCode, Message: "object to insert too large"}}})
	var sizeErr *persistence.TransactionSizeLimitError
	require.ErrorAs(t, err, &sizeErr)

	require.ErrorAs(t, appendHistoryNodesError(mongo.CommandError{Code: mongoBSONObjectTooLargeCode}), &sizeErr)
}
