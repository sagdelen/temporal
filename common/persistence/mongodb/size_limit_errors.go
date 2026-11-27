package mongodb

import (
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/mongo"
	"go.temporal.io/server/common/persistence"
)

// MongoDB error codes for writes that exceed a size limit.
const (
	mongoBSONObjectTooLargeCode   = 10334 // a document is larger than 16 MB
	mongoTransactionTooLargeCode  = 257   // a transaction is larger than allowed
	mongoUpdateResultTooLargeCode = 17419 // an update would make a document larger than 16 MB
)

// isSizeLimitError reports whether err says that a document or a transaction
// is too large to be written. Such a write can never succeed as is.
func isSizeLimitError(err error) bool {
	if err == nil {
		return false
	}
	var serverErr mongo.ServerError
	if errors.As(err, &serverErr) {
		return serverErr.HasErrorCode(mongoBSONObjectTooLargeCode) ||
			serverErr.HasErrorCode(mongoTransactionTooLargeCode) ||
			serverErr.HasErrorCode(mongoUpdateResultTooLargeCode)
	}
	// The driver refuses to send a document larger than the server's limit and
	// reports that without a server error code.
	return strings.Contains(strings.ToLower(err.Error()), "document is too large") ||
		strings.Contains(strings.ToLower(err.Error()), "larger than the maximum")
}

// newSizeLimitError reports a write that is too large. The history service
// treats TransactionSizeLimitError as "not committed" and fails the workflow
// task or terminates the workflow, as with the SQL stores' size limits. An
// Unavailable would make it retry the same oversized write forever.
func newSizeLimitError(err error) error {
	return &persistence.TransactionSizeLimitError{Msg: "mongodb write exceeds a size limit: " + err.Error()}
}
