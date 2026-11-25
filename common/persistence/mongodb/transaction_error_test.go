package mongodb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/mongo/driver/topology"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/server/common/persistence"
)

// These tests need no MongoDB server.

// Errors of the driver itself must leave the store as service errors: the
// persistence retry policy only retries Unavailable, and the SQL stores report
// connection and commit failures the same way.
func TestNormalizeTransactionError_DriverErrorsBecomeUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "server selection failed",
			err:  topology.ServerSelectionError{Wrapped: topology.ErrServerSelectionTimeout},
		},
		{
			name: "driver timeout",
			err:  fmt.Errorf("connection pool wait: %w", context.DeadlineExceeded),
		},
		{
			name: "network error",
			err:  mongo.CommandError{Message: "connection reset", Labels: []string{"NetworkError"}},
		},
		{
			name: "commit failed with unknown result",
			err:  mongo.CommandError{Code: 91, Message: "shutdown in progress", Labels: []string{"UnknownTransactionCommitResult"}},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var unavailable *serviceerror.Unavailable
			require.ErrorAs(t, normalizeTransactionError(tc.err), &unavailable)
		})
	}
}

func TestNormalizeTransactionError_WriteConflictIsNotCommitted(t *testing.T) {
	err := normalizeTransactionError(mongo.CommandError{
		Code:    mongoWriteConflictCode,
		Message: "write conflict",
		Labels:  []string{"TransientTransactionError"},
	})
	var exhausted *serviceerror.ResourceExhausted
	require.ErrorAs(t, err, &exhausted)
}

// Errors returned by the transaction callbacks are service or persistence
// errors already and must keep their type.
func TestNormalizeTransactionError_CallbackErrorsPassThrough(t *testing.T) {
	ownershipLost := &persistence.ShardOwnershipLostError{ShardID: 1, Msg: "lost"}
	require.Same(t, ownershipLost, normalizeTransactionError(ownershipLost))

	conditionFailed := &persistence.WorkflowConditionFailedError{Msg: "version"}
	require.Same(t, conditionFailed, normalizeTransactionError(conditionFailed))

	timeout := &persistence.TimeoutError{Msg: "timeout"}
	require.Same(t, timeout, normalizeTransactionError(timeout))

	require.Same(t, errHistoryTasksAlreadyPersisted, normalizeTransactionError(errHistoryTasksAlreadyPersisted))

	plain := errors.New("callback failed")
	require.Same(t, plain, normalizeTransactionError(plain))
}
