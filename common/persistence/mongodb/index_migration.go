package mongodb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/persistence/mongodb/client"
)

// Index migrations add indexes to a database that is already in use.
//
// The store constructors create their baseline indexes synchronously with a
// short timeout, which is only safe for indexes that already exist or for
// empty collections. An index that has to be built on a populated collection
// goes through a migration instead:
//
//   - it runs in the background and never blocks or fails service start-up;
//   - a lease in the schema_migrations collection makes sure only one process
//     of the cluster builds indexes at a time;
//   - the outcome of every index is logged and recorded in schema_migrations,
//     so later starts skip a completed migration;
//   - an index that cannot be created (for example a unique index over data
//     that contains duplicates) is reported and skipped, the rest still runs;
//   - transient errors (network, primary change) are retried, first for the
//     index at hand and then for the migration as a whole after a backoff. A
//     unique index that failed on duplicate data is not retried on its own,
//     because every attempt scans the collection: an operator fixes the data
//     and deletes the migration's record to have it run again.
//
// Setting disableIndexMigration in the MongoDB datastore config turns the
// builds off. The server then only reports which indexes are missing, and the
// operator builds them with schema/mongodb/temporal/versioned/v1.0/init.js.
//
// A migration never drops an index unless the definition of its replacement
// names it in Replaces, and then only after the replacement exists.

const (
	collectionSchemaMigrations = "schema_migrations"

	// indexNameFallbackPrefix is put in front of an index name that is already
	// used by a different index. Index names are not part of query planning, so
	// the fallback name is as good as the intended one.
	indexNameFallbackPrefix = "idx_"

	defaultIndexMigrationLease        = 2 * time.Minute
	defaultIndexMigrationHeartbeat    = 30 * time.Second
	defaultIndexMigrationPoll         = time.Minute
	defaultIndexMigrationRetryBackoff = 30 * time.Minute

	// A transient failure of one index is retried this many times, waiting the
	// attempt number times the interval in between, before the index is given
	// up for this run of the migration.
	defaultIndexBuildAttempts      = 5
	defaultIndexBuildRetryInterval = 5 * time.Second

	indexMigrationStatusRunning             = "running"
	indexMigrationStatusCompleted           = "completed"
	indexMigrationStatusCompletedWithErrors = "completed_with_errors"

	mongoIndexOptionsConflictCode  = 85
	mongoIndexKeySpecsConflictCode = 86
	mongoDuplicateKeyCode          = 11000
	mongoNamespaceNotFoundCode     = 26
)

// Outcomes of a single index of a migration.
const (
	// indexOutcomeCreated: the index was built by this run.
	indexOutcomeCreated = "created"
	// indexOutcomeExists: an identical index with the expected name was there.
	indexOutcomeExists = "exists"
	// indexOutcomeExistsOtherName: an index with the same keys and options was
	// there under another name, which serves the same queries.
	indexOutcomeExistsOtherName = "exists_other_name"
	// indexOutcomeSkippedUnsupported: the server is older than the version the
	// index needs. Nothing was changed, and this is not an error.
	indexOutcomeSkippedUnsupported = "skipped_unsupported"
	// indexOutcomeConflict: the name (and the fallback name) is used by a
	// different index, or the server refused the definition next to an existing
	// index. Nothing was changed.
	indexOutcomeConflict = "conflict"
	// indexOutcomeDuplicateData: a unique index could not be built because the
	// collection contains duplicates. Nothing was changed.
	indexOutcomeDuplicateData = "duplicate_data"
	// indexOutcomeFailed: any other error. Nothing was changed.
	indexOutcomeFailed = "failed"
)

// indexMigrationOutcome is the result of one attempt to run a migration.
type indexMigrationOutcome string

const (
	// indexMigrationCompleted: this run executed the migration, every index is in place.
	indexMigrationCompleted indexMigrationOutcome = "completed"
	// indexMigrationCompletedWithErrors: this run executed the migration, at
	// least one index is not in place.
	indexMigrationCompletedWithErrors indexMigrationOutcome = "completed_with_errors"
	// indexMigrationAlreadyCompleted: the migration was completed earlier or by another process.
	indexMigrationAlreadyCompleted indexMigrationOutcome = "already_completed"
	// indexMigrationBlocked: an earlier attempt could not build a unique index
	// because of duplicate data. It is not retried until an operator deletes
	// the migration's record.
	indexMigrationBlocked indexMigrationOutcome = "blocked_needs_operator"
	// indexMigrationInterrupted: the context was cancelled before the migration finished.
	indexMigrationInterrupted indexMigrationOutcome = "interrupted"
)

type (
	// indexDefinition describes one index a migration has to put in place.
	indexDefinition struct {
		Collection string
		Name       string
		Keys       bson.D
		Unique     bool
		// ExpireAfterSeconds makes the index a TTL index when set.
		ExpireAfterSeconds *int32
		// Replaces lists the names of older indexes on the same keys that are
		// dropped once this index exists. An index named here is only dropped
		// when its keys are identical to Keys.
		Replaces []string
		// MinServerVersion is the lowest MongoDB version (major, minor) that
		// supports the index. On an older server the index is skipped.
		MinServerVersion []int
	}

	// indexMigration is a named, ordered set of index definitions. The ID is
	// the key of its record in schema_migrations: a migration that has been
	// released must not be edited, new indexes go into a new migration.
	indexMigration struct {
		ID      string
		Indexes []indexDefinition
	}

	indexMigrationResult struct {
		Collection string   `bson:"collection"`
		Name       string   `bson:"name"`
		CreatedAs  string   `bson:"created_as,omitempty"`
		Outcome    string   `bson:"outcome"`
		Error      string   `bson:"error,omitempty"`
		Dropped    []string `bson:"dropped,omitempty"`
		Notes      []string `bson:"notes,omitempty"`
	}

	indexMigrationDocument struct {
		ID         string     `bson:"_id"`
		Status     string     `bson:"status"`
		Owner      string     `bson:"owner"`
		LeaseUntil time.Time  `bson:"lease_until"`
		StartedAt  time.Time  `bson:"started_at"`
		FinishedAt *time.Time `bson:"finished_at,omitempty"`
		// NeedsOperator is set when the migration ended with an error that a
		// retry cannot fix (duplicate data under a unique index).
		NeedsOperator bool                   `bson:"needs_operator,omitempty"`
		Indexes       []indexMigrationResult `bson:"indexes,omitempty"`
	}

	existingIndex struct {
		Name                    string   `bson:"name"`
		Key                     bson.D   `bson:"key"`
		Unique                  bool     `bson:"unique"`
		Sparse                  bool     `bson:"sparse"`
		ExpireAfterSeconds      *float64 `bson:"expireAfterSeconds"`
		PartialFilterExpression bson.Raw `bson:"partialFilterExpression"`
	}

	indexMigrator struct {
		db     client.Database
		logger log.Logger
		owner  string
		now    func() time.Time

		lease        time.Duration
		heartbeat    time.Duration
		poll         time.Duration
		retryBackoff time.Duration

		buildAttempts      int
		buildRetryInterval time.Duration

		// serverVersion is the MongoDB version (major, minor, ...), read from
		// the server on first use.
		serverVersion []int
	}

	indexMigrationAcquireState int
)

const (
	indexMigrationAcquired indexMigrationAcquireState = iota
	indexMigrationHeldByOther
	indexMigrationDone
	indexMigrationWaitingForRetry
	indexMigrationNeedsOperator
)

// primaryIndexMigration adds the indexes the execution store queries need on
// collections that have no secondary index for them.
func primaryIndexMigration() indexMigration {
	zero := int32(0)
	return indexMigration{
		ID: "primary-indexes-001",
		Indexes: []indexDefinition{
			{
				// ReadHistoryBranch pagination and DeleteHistoryBranch filter on
				// these fields; mirrors the SQL history_node primary key.
				Collection: collectionHistoryNodes,
				Name:       "history_nodes_branch_node_txn",
				Keys: bson.D{
					{Key: "shard_id", Value: 1},
					{Key: "tree_id", Value: 1},
					{Key: "branch_id", Value: 1},
					{Key: "node_id", Value: 1},
					{Key: "txn_id", Value: -1},
				},
				Unique: true,
			},
			{
				// GetHistoryTreeContainingBranch and GetAllHistoryTreeBranches;
				// mirrors the SQL history_tree primary key.
				Collection: collectionHistoryBranches,
				Name:       "history_branches_shard_tree_branch",
				Keys: bson.D{
					{Key: "shard_id", Value: 1},
					{Key: "tree_id", Value: 1},
					{Key: "branch_id", Value: 1},
				},
				Unique: true,
			},
			{
				// ListConcreteExecutions: shard equality, _id range and sort.
				Collection: collectionExecutions,
				Name:       "executions_shard_id",
				Keys: bson.D{
					{Key: "shard_id", Value: 1},
					{Key: "_id", Value: 1},
				},
			},
			{
				// Expired membership records are removed by the server instead
				// of a collection scan in PruneClusterMembership.
				Collection:         collectionClusterMembership,
				Name:               "cluster_membership_ttl",
				Keys:               bson.D{{Key: "record_expiry", Value: 1}},
				ExpireAfterSeconds: &zero,
			},
		},
	}
}

// visibilityIndexMigration adds indexes for filtered visibility queries. Every
// compound index is an equality field followed by the default sort order
// (visibility_time descending, run_id), so a filtered list needs no in-memory
// sort.
func visibilityIndexMigration() indexMigration {
	filtered := func(name, field string) indexDefinition {
		return indexDefinition{
			Collection: collectionVisibilityExecutions,
			Name:       name,
			Keys: bson.D{
				{Key: "namespace_id", Value: 1},
				{Key: field, Value: 1},
				{Key: "visibility_time", Value: -1},
				{Key: "run_id", Value: 1},
			},
		}
	}
	return indexMigration{
		ID: "visibility-indexes-001",
		Indexes: []indexDefinition{
			filtered("visibility_status_time", "status"),
			filtered("visibility_workflow_type_time", "workflow_type_name"),
			filtered("visibility_workflow_id_time", "workflow_id"),
			filtered("visibility_task_queue_time", "task_queue"),
			{
				// GetWorkflowExecution without a workflow ID and RunId filters.
				Collection: collectionVisibilityExecutions,
				Name:       "visibility_namespace_run",
				Keys: bson.D{
					{Key: "namespace_id", Value: 1},
					{Key: "run_id", Value: 1},
				},
			},
			{
				// Search attribute names are dynamic, so one wildcard index
				// covers equality and range filters on all of them. It is a
				// compound wildcard index led by namespace_id, so a filter on
				// a search attribute only scans its own namespace. Compound
				// wildcard indexes exist since MongoDB 7.0; on older servers
				// the index is left out and such filters keep scanning the
				// namespace through the visibility_time index.
				Collection: collectionVisibilityExecutions,
				Name:       "visibility_namespace_search_attributes_wildcard",
				Keys: bson.D{
					{Key: "namespace_id", Value: 1},
					{Key: "search_attributes.$**", Value: 1},
				},
				MinServerVersion: []int{7, 0},
			},
		},
	}
}

func newIndexMigrator(db client.Database, logger log.Logger) *indexMigrator {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return &indexMigrator{
		db:           db,
		logger:       logger,
		owner:        fmt.Sprintf("%s-%d-%s", host, os.Getpid(), primitive.NewObjectID().Hex()),
		now:          func() time.Time { return time.Now().UTC() },
		lease:        defaultIndexMigrationLease,
		heartbeat:    defaultIndexMigrationHeartbeat,
		poll:         defaultIndexMigrationPoll,
		retryBackoff: defaultIndexMigrationRetryBackoff,

		buildAttempts:      defaultIndexBuildAttempts,
		buildRetryInterval: defaultIndexBuildRetryInterval,
	}
}

// run executes the migration unless it is already completed. While another
// process holds the lease, or while the migration is in its backoff after an
// attempt that ended with errors, it waits and checks again, so the migration
// still finishes if the other process dies or once the errors have gone away.
// It returns when the migration is completed (by anyone), was attempted by
// this process, is blocked until an operator acts, or ctx is cancelled.
func (m *indexMigrator) run(ctx context.Context, migration indexMigration) indexMigrationOutcome {
	migrationTag := tag.NewStringTag("migration", migration.ID)
	loggedWaiting := false
	for {
		state, err := m.tryAcquire(ctx, migration.ID)
		switch {
		case ctx.Err() != nil:
			return indexMigrationInterrupted
		case err != nil:
			m.logger.Warn("MongoDB index migration could not read or take its lease, will retry",
				migrationTag, tag.Error(err))
		case state == indexMigrationAcquired:
			return m.execute(ctx, migration)
		case state == indexMigrationDone:
			m.logger.Debug("MongoDB index migration already completed", migrationTag)
			return indexMigrationAlreadyCompleted
		case state == indexMigrationNeedsOperator:
			m.logger.Error("MongoDB index migration is blocked: a unique index could not be built because the "+
				"collection contains duplicates, and it is not retried automatically. Fix the data, then delete "+
				"this migration's record from the schema_migrations collection and restart a server. "+
				"The record lists the affected indexes",
				migrationTag)
			return indexMigrationBlocked
		case state == indexMigrationWaitingForRetry:
			if !loggedWaiting {
				loggedWaiting = true
				m.logger.Warn("MongoDB index migration ended with errors earlier; it is retried after its backoff. "+
					"See the schema_migrations collection for details",
					migrationTag)
			}
		}

		select {
		case <-ctx.Done():
			return indexMigrationInterrupted
		case <-time.After(m.poll):
		}
	}
}

func (m *indexMigrator) tryAcquire(ctx context.Context, id string) (indexMigrationAcquireState, error) {
	col := m.db.Collection(collectionSchemaMigrations)
	now := m.now()

	_, err := col.InsertOne(ctx, indexMigrationDocument{
		ID:         id,
		Status:     indexMigrationStatusRunning,
		Owner:      m.owner,
		LeaseUntil: now.Add(m.lease),
		StartedAt:  now,
	})
	if err == nil {
		return indexMigrationAcquired, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return indexMigrationHeldByOther, err
	}

	// The record exists. Take it over if its owner stopped renewing the lease,
	// or if the last attempt ended with errors long enough ago.
	res, err := col.UpdateOne(ctx,
		bson.M{
			"_id": id,
			"$or": bson.A{
				bson.M{"status": indexMigrationStatusRunning, "lease_until": bson.M{"$lt": now}},
				bson.M{
					"status":         indexMigrationStatusCompletedWithErrors,
					"needs_operator": bson.M{"$ne": true},
					"finished_at":    bson.M{"$lte": now.Add(-m.retryBackoff)},
				},
			},
		},
		bson.M{
			"$set": bson.M{
				"status":      indexMigrationStatusRunning,
				"owner":       m.owner,
				"lease_until": now.Add(m.lease),
				"started_at":  now,
			},
			"$unset": bson.M{"finished_at": ""},
		},
	)
	if err != nil {
		return indexMigrationHeldByOther, err
	}
	if res.MatchedCount == 1 {
		return indexMigrationAcquired, nil
	}

	var doc indexMigrationDocument
	if err := col.FindOne(ctx, bson.M{"_id": id}).Decode(&doc); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return indexMigrationHeldByOther, nil
		}
		return indexMigrationHeldByOther, err
	}
	switch doc.Status {
	case indexMigrationStatusCompleted:
		return indexMigrationDone, nil
	case indexMigrationStatusCompletedWithErrors:
		if doc.NeedsOperator {
			return indexMigrationNeedsOperator, nil
		}
		return indexMigrationWaitingForRetry, nil
	default:
		return indexMigrationHeldByOther, nil
	}
}

func (m *indexMigrator) execute(ctx context.Context, migration indexMigration) indexMigrationOutcome {
	migrationTag := tag.NewStringTag("migration", migration.ID)
	m.logger.Info("MongoDB index migration started", migrationTag, tag.NewInt("indexes", len(migration.Indexes)))

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		m.renewLease(heartbeatCtx, migration.ID)
	}()

	results := make([]indexMigrationResult, 0, len(migration.Indexes))
	interrupted := false
	for _, def := range migration.Indexes {
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		result := m.ensureIndex(ctx, def)
		if ctx.Err() != nil {
			// The result of an interrupted call says nothing about the index.
			interrupted = true
			break
		}
		m.logIndexResult(migration.ID, result)
		results = append(results, result)
	}

	stopHeartbeat()
	<-heartbeatDone

	if interrupted {
		m.releaseLease(migration.ID)
		m.logger.Info("MongoDB index migration interrupted, it continues on a later start", migrationTag)
		return indexMigrationInterrupted
	}

	status := indexMigrationStatusCompleted
	needsOperator := false
	var notCreated []string
	for _, result := range results {
		if !result.ok() {
			status = indexMigrationStatusCompletedWithErrors
			notCreated = append(notCreated, result.Collection+"."+result.Name)
		}
		if result.Outcome == indexOutcomeDuplicateData {
			needsOperator = true
		}
	}

	now := m.now()
	res, err := m.db.Collection(collectionSchemaMigrations).UpdateOne(ctx,
		bson.M{"_id": migration.ID, "owner": m.owner},
		bson.M{"$set": bson.M{
			"status":         status,
			"finished_at":    now,
			"lease_until":    now,
			"needs_operator": needsOperator,
			"indexes":        results,
		}},
	)
	switch {
	case err != nil:
		m.logger.Warn("MongoDB index migration could not record its result", migrationTag, tag.Error(err))
	case res.MatchedCount == 0:
		m.logger.Warn("MongoDB index migration lost its lease before recording its result", migrationTag)
	}

	if status == indexMigrationStatusCompleted {
		m.logger.Info("MongoDB index migration completed", migrationTag, tag.NewInt("indexes", len(results)))
		return indexMigrationCompleted
	}
	m.logger.Error("MongoDB index migration finished with errors: some indexes were not created. "+
		"See the schema_migrations collection for details",
		migrationTag, tag.NewStringsTag("indexes-not-created", notCreated))
	return indexMigrationCompletedWithErrors
}

func (r indexMigrationResult) ok() bool {
	switch r.Outcome {
	case indexOutcomeCreated, indexOutcomeExists, indexOutcomeExistsOtherName, indexOutcomeSkippedUnsupported:
		return true
	default:
		return false
	}
}

func (m *indexMigrator) logIndexResult(migrationID string, result indexMigrationResult) {
	tags := []tag.Tag{
		tag.NewStringTag("migration", migrationID),
		tag.NewStringTag("collection", result.Collection),
		tag.NewStringTag("index", result.Name),
		tag.NewStringTag("outcome", result.Outcome),
	}
	if result.CreatedAs != "" && result.CreatedAs != result.Name {
		tags = append(tags, tag.NewStringTag("index-name-in-database", result.CreatedAs))
	}
	if len(result.Dropped) > 0 {
		tags = append(tags, tag.NewStringsTag("dropped-indexes", result.Dropped))
	}
	if len(result.Notes) > 0 {
		tags = append(tags, tag.NewStringsTag("notes", result.Notes))
	}
	if result.Error != "" {
		tags = append(tags, tag.NewStringTag("index-error", result.Error))
	}

	switch {
	case !result.ok():
		m.logger.Error("MongoDB index migration: index not created", tags...)
	case len(result.Notes) > 0:
		m.logger.Warn("MongoDB index migration: index in place", tags...)
	default:
		m.logger.Info("MongoDB index migration: index in place", tags...)
	}
}

// ensureIndex puts one index in place and reports what it did. A transient
// error (network, primary change) is retried a few times before the index is
// given up for this run.
func (m *indexMigrator) ensureIndex(ctx context.Context, def indexDefinition) indexMigrationResult {
	for attempt := 1; ; attempt++ {
		result, err := m.ensureIndexOnce(ctx, def)
		if err == nil || ctx.Err() != nil || attempt >= m.buildAttempts || !isRetryableIndexBuildError(err) {
			return result
		}
		m.logger.Warn("MongoDB index migration: transient error, retrying the index",
			tag.NewStringTag("collection", def.Collection),
			tag.NewStringTag("index", def.Name),
			tag.NewInt("attempt", attempt),
			tag.Error(err))
		select {
		case <-ctx.Done():
			return result
		case <-time.After(time.Duration(attempt) * m.buildRetryInterval):
		}
	}
}

// ensureIndexOnce makes one attempt at an index. It never modifies an existing
// index and only drops the indexes named in Replaces. The error is the one
// behind a failed outcome, for the caller to decide about a retry.
func (m *indexMigrator) ensureIndexOnce(ctx context.Context, def indexDefinition) (indexMigrationResult, error) {
	result := indexMigrationResult{Collection: def.Collection, Name: def.Name}
	col := m.db.Collection(def.Collection)

	if len(def.MinServerVersion) > 0 {
		version, err := m.getServerVersion(ctx)
		if err != nil {
			result.Outcome = indexOutcomeFailed
			result.Error = err.Error()
			return result, err
		}
		if versionLess(version, def.MinServerVersion) {
			result.Outcome = indexOutcomeSkippedUnsupported
			result.Notes = append(result.Notes, fmt.Sprintf(
				"the index needs MongoDB %s or later, the server is %s",
				formatVersion(def.MinServerVersion), formatVersion(version)))
			return result, nil
		}
	}

	existing, err := listExistingIndexes(ctx, col)
	if err != nil {
		result.Outcome = indexOutcomeFailed
		result.Error = err.Error()
		return result, err
	}

	// Pick the name: the intended one, or the prefixed one if the intended
	// name is used by a different index.
	target := def.Name
	sameName := findIndexByName(existing, target)
	if sameName != nil && !def.matches(*sameName) {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"index name %q is used by a different index, using %q", def.Name, indexNameFallbackPrefix+def.Name))
		target = indexNameFallbackPrefix + def.Name
		sameName = findIndexByName(existing, target)
		if sameName != nil && !def.matches(*sameName) {
			result.Outcome = indexOutcomeConflict
			result.Error = fmt.Sprintf("index names %q and %q are both used by different indexes", def.Name, target)
			return result, nil
		}
	}

	switch equivalent := findEquivalentIndex(existing, def); {
	case sameName != nil:
		result.Outcome = indexOutcomeExists
		result.CreatedAs = target
	case equivalent != nil:
		result.Outcome = indexOutcomeExistsOtherName
		result.CreatedAs = equivalent.Name
	default:
		m.logger.Info("MongoDB index migration: building index",
			tag.NewStringTag("collection", def.Collection),
			tag.NewStringTag("index", target))
		if _, err := col.Indexes().CreateOne(ctx, def.model(target)); err != nil {
			result.Outcome = classifyIndexBuildError(err)
			result.Error = err.Error()
			return result, err
		}
		result.Outcome = indexOutcomeCreated
		result.CreatedAs = target
	}

	// The index is in place: drop the indexes it replaces.
	for _, oldName := range def.Replaces {
		if oldName == result.CreatedAs {
			continue
		}
		old := findIndexByName(existing, oldName)
		if old == nil {
			continue
		}
		if !sameIndexKeys(def.Keys, old.Key) {
			result.Notes = append(result.Notes, fmt.Sprintf(
				"index %q was not dropped: its keys differ from the index replacing it", oldName))
			continue
		}
		if err := col.Indexes().DropOne(ctx, oldName); err != nil {
			result.Notes = append(result.Notes, fmt.Sprintf("index %q could not be dropped: %v", oldName, err))
			continue
		}
		result.Dropped = append(result.Dropped, oldName)
	}

	return result, nil
}

// report lists the indexes of the migration that are not in place, without
// building anything. It is what a server does when index migrations are
// disabled in its configuration.
func (m *indexMigrator) report(ctx context.Context, migration indexMigration) []string {
	migrationTag := tag.NewStringTag("migration", migration.ID)
	var missing []string
	for _, def := range migration.Indexes {
		if len(def.MinServerVersion) > 0 {
			version, err := m.getServerVersion(ctx)
			if err == nil && versionLess(version, def.MinServerVersion) {
				continue
			}
		}
		existing, err := listExistingIndexes(ctx, m.db.Collection(def.Collection))
		if err != nil {
			if ctx.Err() == nil {
				m.logger.Warn("MongoDB index migration is disabled and the indexes could not be checked",
					migrationTag, tag.Error(err))
			}
			return nil
		}
		if findEquivalentIndex(existing, def) == nil {
			missing = append(missing, def.Collection+"."+def.Name)
		}
	}

	if len(missing) == 0 {
		m.logger.Info("MongoDB index migration is disabled; all its indexes are in place", migrationTag)
		return nil
	}
	m.logger.Warn("MongoDB index migration is disabled and some indexes are missing. Queries that need them "+
		"scan their collection. Build them with schema/mongodb/temporal/versioned/v1.0/init.js",
		migrationTag, tag.NewStringsTag("missing-indexes", missing))
	return missing
}

// getServerVersion returns the version of the MongoDB server as numbers.
func (m *indexMigrator) getServerVersion(ctx context.Context) ([]int, error) {
	if m.serverVersion != nil {
		return m.serverVersion, nil
	}
	var info struct {
		VersionArray []int32 `bson:"versionArray"`
	}
	if err := m.db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return nil, err
	}
	if len(info.VersionArray) == 0 {
		return nil, errors.New("the server did not report its version")
	}
	version := make([]int, len(info.VersionArray))
	for i, part := range info.VersionArray {
		version[i] = int(part)
	}
	m.serverVersion = version
	return version, nil
}

func versionLess(version, minimum []int) bool {
	for i, want := range minimum {
		have := 0
		if i < len(version) {
			have = version[i]
		}
		if have != want {
			return have < want
		}
	}
	return false
}

func formatVersion(version []int) string {
	parts := make([]string, len(version))
	for i, part := range version {
		parts[i] = strconv.Itoa(part)
	}
	return strings.Join(parts, ".")
}

// Server error codes that mean "try again": the node is shutting down or is
// no longer primary, the operation was interrupted by such a change, or it
// lost a lock or write conflict.
var retryableIndexBuildCodes = []int{
	24,    // LockTimeout
	91,    // ShutdownInProgress
	112,   // WriteConflict
	189,   // PrimarySteppedDown
	262,   // ExceededTimeLimit
	276,   // IndexBuildAborted
	10107, // NotWritablePrimary
	11600, // InterruptedAtShutdown
	11602, // InterruptedDueToReplStateChange
	13435, // NotPrimaryNoSecondaryOk
	13436, // NotPrimaryOrSecondary
}

// isRetryableIndexBuildError reports whether an index operation failed for a
// reason that another attempt may not hit.
func isRetryableIndexBuildError(err error) bool {
	if mongo.IsNetworkError(err) || mongo.IsTimeout(err) {
		return true
	}
	var serverErr mongo.ServerError
	if !errors.As(err, &serverErr) {
		return false
	}
	if serverErr.HasErrorLabel("RetryableWriteError") {
		return true
	}
	for _, code := range retryableIndexBuildCodes {
		if serverErr.HasErrorCode(code) {
			return true
		}
	}
	return false
}

func (def indexDefinition) model(name string) mongo.IndexModel {
	opts := options.Index().SetName(name)
	if def.Unique {
		opts.SetUnique(true)
	}
	if def.ExpireAfterSeconds != nil {
		opts.SetExpireAfterSeconds(*def.ExpireAfterSeconds)
	}
	return mongo.IndexModel{Keys: def.Keys, Options: opts}
}

// matches reports whether an existing index has the keys and options of def.
func (def indexDefinition) matches(existing existingIndex) bool {
	if !sameIndexKeys(def.Keys, existing.Key) || def.Unique != existing.Unique {
		return false
	}
	if existing.Sparse || len(existing.PartialFilterExpression) > 0 {
		return false
	}
	if (def.ExpireAfterSeconds == nil) != (existing.ExpireAfterSeconds == nil) {
		return false
	}
	return def.ExpireAfterSeconds == nil || float64(*def.ExpireAfterSeconds) == *existing.ExpireAfterSeconds
}

func listExistingIndexes(ctx context.Context, col client.Collection) ([]existingIndex, error) {
	cursor, err := col.Indexes().List(ctx)
	if err != nil {
		var serverErr mongo.ServerError
		if errors.As(err, &serverErr) && serverErr.HasErrorCode(mongoNamespaceNotFoundCode) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var indexes []existingIndex
	if err := cursor.All(ctx, &indexes); err != nil {
		return nil, err
	}
	return indexes, nil
}

func findIndexByName(indexes []existingIndex, name string) *existingIndex {
	for i := range indexes {
		if indexes[i].Name == name {
			return &indexes[i]
		}
	}
	return nil
}

func findEquivalentIndex(indexes []existingIndex, def indexDefinition) *existingIndex {
	for i := range indexes {
		if def.matches(indexes[i]) {
			return &indexes[i]
		}
	}
	return nil
}

func sameIndexKeys(a, b bson.D) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || indexKeyValue(a[i].Value) != indexKeyValue(b[i].Value) {
			return false
		}
	}
	return true
}

// indexKeyValue normalizes an index key value: the server may return the
// direction as int32, int64 or double depending on how the index was created.
func indexKeyValue(v interface{}) string {
	switch n := v.(type) {
	case int:
		return strconv.FormatInt(int64(n), 10)
	case int32:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func classifyIndexBuildError(err error) string {
	var serverErr mongo.ServerError
	if errors.As(err, &serverErr) {
		switch {
		case serverErr.HasErrorCode(mongoDuplicateKeyCode):
			return indexOutcomeDuplicateData
		case serverErr.HasErrorCode(mongoIndexOptionsConflictCode), serverErr.HasErrorCode(mongoIndexKeySpecsConflictCode):
			return indexOutcomeConflict
		}
	}
	if mongo.IsDuplicateKeyError(err) {
		return indexOutcomeDuplicateData
	}
	return indexOutcomeFailed
}

// renewLease extends the lease until ctx is cancelled.
func (m *indexMigrator) renewLease(ctx context.Context, id string) {
	ticker := time.NewTicker(m.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := m.db.Collection(collectionSchemaMigrations).UpdateOne(ctx,
				bson.M{"_id": id, "owner": m.owner, "status": indexMigrationStatusRunning},
				bson.M{"$set": bson.M{"lease_until": m.now().Add(m.lease)}},
			)
			switch {
			case err != nil && ctx.Err() == nil:
				m.logger.Warn("MongoDB index migration could not renew its lease",
					tag.NewStringTag("migration", id), tag.Error(err))
			case err == nil && res.MatchedCount == 0:
				m.logger.Warn("MongoDB index migration no longer holds its lease; another process may have taken it over",
					tag.NewStringTag("migration", id))
			}
		}
	}
}

// releaseLease lets another process take the migration over right away.
func (m *indexMigrator) releaseLease(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = m.db.Collection(collectionSchemaMigrations).UpdateOne(ctx,
		bson.M{"_id": id, "owner": m.owner, "status": indexMigrationStatusRunning},
		bson.M{"$set": bson.M{"lease_until": m.now().Add(-time.Second)}},
	)
}
