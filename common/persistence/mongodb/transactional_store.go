package mongodb

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/mongo/driver/topology"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence/mongodb/client"
)

type transactionalStore struct {
	client         client.Client
	metricsHandler metrics.Handler
}

func newTransactionalStore(mongoClient client.Client, handlers ...metrics.Handler) transactionalStore {
	metricsHandler := metrics.NoopMetricsHandler
	if len(handlers) > 0 && handlers[0] != nil {
		metricsHandler = handlers[0]
	}
	return transactionalStore{client: mongoClient, metricsHandler: metricsHandler}
}

func (s *transactionalStore) executeTransaction(ctx context.Context, fn func(context.Context) (interface{}, error)) (interface{}, error) {
	sess, err := s.client.StartSession(ctx)
	if err != nil {
		metrics.PersistenceMongoTransactionsFailed.With(s.metricsHandler).Record(1)
		metrics.PersistenceMongoSessionsInProgress.With(s.metricsHandler).Record(float64(s.client.NumberSessionsInProgress()))
		return nil, serviceerror.NewUnavailablef("failed to start mongo session: %v", err)
	}
	metrics.PersistenceMongoSessionsInProgress.With(s.metricsHandler).Record(float64(s.client.NumberSessionsInProgress()))
	defer func() {
		sess.EndSession(ctx)
		metrics.PersistenceMongoSessionsInProgress.With(s.metricsHandler).Record(float64(s.client.NumberSessionsInProgress()))
	}()

	metrics.PersistenceMongoTransactionsStarted.With(s.metricsHandler).Record(1)

	// The driver re-runs the callback immediately, without a limit other than
	// its own timeout, whenever the callback returns an error labelled
	// TransientTransactionError. The only such error the stores return is a
	// write conflict (see fenceShardRangeID). Bound those retries and back off
	// between them, so that conflicting transactions cannot spin against each
	// other until the caller's deadline.
	attempts := 0
	result, err := sess.WithTransaction(ctx, func(sessCtx context.Context) (interface{}, error) {
		attempts++
		if attempts > 1 {
			// Nothing has been sent for this attempt yet, so no snapshot or
			// lock is held while waiting.
			backoff := time.Duration(attempts-1)*transactionRetryBackoff +
				time.Duration(rand.Int63n(int64(transactionRetryBackoff)))
			select {
			case <-sessCtx.Done():
				return nil, sessCtx.Err()
			case <-time.After(backoff):
			}
		}
		res, err := fn(sessCtx)
		if err != nil && attempts >= maxTransactionAttempts && isTransientWriteConflict(err) {
			// Not labelled as transient, which ends the driver's retry loop.
			return nil, newTransactionConflictError(err)
		}
		return res, err
	})
	if err != nil {
		metrics.PersistenceMongoTransactionsFailed.With(s.metricsHandler).Record(1)
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			// The caller's own deadline or cancellation: report it as such.
			return nil, err
		}
		return nil, normalizeTransactionError(err)
	}
	return result, nil
}

const (
	// maxTransactionAttempts bounds how often a transaction is attempted when
	// it keeps failing with a write conflict.
	maxTransactionAttempts = 5
	// transactionRetryBackoff is the base delay before another attempt; the
	// delay grows linearly with the attempt number and is jittered.
	transactionRetryBackoff = 5 * time.Millisecond
)

// newTransactionConflictError reports a transaction that was aborted by write
// conflicts and was therefore not committed. ResourceExhausted is the error
// the history service treats as "definitely not committed, retry later"; an
// Unavailable would make it assume an unknown outcome and renew the shard range.
func newTransactionConflictError(cause error) error {
	return &serviceerror.ResourceExhausted{
		Cause:   enumspb.RESOURCE_EXHAUSTED_CAUSE_BUSY_WORKFLOW,
		Scope:   enumspb.RESOURCE_EXHAUSTED_SCOPE_SYSTEM,
		Message: "mongodb transaction aborted by write conflicts: " + cause.Error(),
	}
}

// normalizeTransactionError makes sure no driver error leaves the store layer.
// Errors returned by the transaction callbacks are service or persistence
// errors already and pass through. What can be left is an error of the driver
// itself: a write conflict the driver stopped retrying (its own time limit),
// which was not committed, or a failure to start or commit the transaction,
// whose outcome is unknown. The latter become Unavailable, as in the SQL
// stores: the history service treats it as an unknown outcome, and it is the
// error the persistence retry policy retries.
func normalizeTransactionError(err error) error {
	if isTransientWriteConflict(err) {
		return newTransactionConflictError(err)
	}
	var serverErr mongo.ServerError
	if errors.As(err, &serverErr) {
		return serviceerror.NewUnavailablef("mongodb transaction failed: %v", err)
	}
	// Not a server reply: no server could be selected, or the driver gave up
	// waiting for a connection or a reply.
	var selectionErr topology.ServerSelectionError
	if errors.As(err, &selectionErr) || mongo.IsTimeout(err) || mongo.IsNetworkError(err) {
		return serviceerror.NewUnavailablef("mongodb transaction failed: %v", err)
	}
	return err
}
