package mongodb

import (
	"context"
	"time"

	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/persistence/mongodb/client"
)

// Test-only access to the index migration internals for the mongodb_test package.

type (
	IndexDefinitionForTest        = indexDefinition
	IndexMigrationForTest         = indexMigration
	IndexMigrationDocumentForTest = indexMigrationDocument

	// IndexMigrationOptionsForTest overrides the timing of a migration run.
	// Zero values keep the defaults.
	IndexMigrationOptionsForTest struct {
		Owner        string
		Lease        time.Duration
		Heartbeat    time.Duration
		Poll         time.Duration
		RetryBackoff *time.Duration
	}
)

const (
	SchemaMigrationsCollectionForTest = collectionSchemaMigrations
	IndexNameFallbackPrefixForTest    = indexNameFallbackPrefix
)

func PrimaryIndexMigrationForTest() IndexMigrationForTest {
	return primaryIndexMigration()
}

func VisibilityIndexMigrationForTest() IndexMigrationForTest {
	return visibilityIndexMigration()
}

func RunIndexMigrationForTest(
	ctx context.Context,
	db client.Database,
	logger log.Logger,
	migration IndexMigrationForTest,
	opts IndexMigrationOptionsForTest,
) string {
	m := newIndexMigrator(db, logger)
	if opts.Owner != "" {
		m.owner = opts.Owner
	}
	if opts.Lease > 0 {
		m.lease = opts.Lease
	}
	if opts.Heartbeat > 0 {
		m.heartbeat = opts.Heartbeat
	}
	if opts.Poll > 0 {
		m.poll = opts.Poll
	}
	if opts.RetryBackoff != nil {
		m.retryBackoff = *opts.RetryBackoff
	}
	return string(m.run(ctx, migration))
}
