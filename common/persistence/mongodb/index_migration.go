package mongodb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
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
//     that contains duplicates) is reported and skipped, the rest still runs.
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
	// indexMigrationSkippedAfterErrors: an earlier attempt ended with errors
	// and the retry backoff has not passed yet.
	indexMigrationSkippedAfterErrors indexMigrationOutcome = "skipped_after_errors"
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
		ID         string                 `bson:"_id"`
		Status     string                 `bson:"status"`
		Owner      string                 `bson:"owner"`
		LeaseUntil time.Time              `bson:"lease_until"`
		StartedAt  time.Time              `bson:"started_at"`
		FinishedAt *time.Time             `bson:"finished_at,omitempty"`
		Indexes    []indexMigrationResult `bson:"indexes,omitempty"`
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
	}

	indexMigrationAcquireState int
)

const (
	indexMigrationAcquired indexMigrationAcquireState = iota
	indexMigrationHeldByOther
	indexMigrationDone
	indexMigrationFailedRecently
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
				// covers equality and range filters on all of them.
				Collection: collectionVisibilityExecutions,
				Name:       "visibility_search_attributes_wildcard",
				Keys:       bson.D{{Key: "search_attributes.$**", Value: 1}},
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
	}
}

// run executes the migration unless it is already completed. While another
// process holds the lease it waits and checks again, so the migration still
// finishes if that process dies. It returns when the migration is completed
// (by anyone), was attempted by this process, is in its retry backoff after an
// attempt with errors, or ctx is cancelled.
func (m *indexMigrator) run(ctx context.Context, migration indexMigration) indexMigrationOutcome {
	migrationTag := tag.NewStringTag("migration", migration.ID)
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
		case state == indexMigrationFailedRecently:
			m.logger.Warn("MongoDB index migration ended with errors earlier and is in its retry backoff; "+
				"it is retried when a server starts after the backoff. See the schema_migrations collection for details",
				migrationTag)
			return indexMigrationSkippedAfterErrors
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
				bson.M{"status": indexMigrationStatusCompletedWithErrors, "finished_at": bson.M{"$lte": now.Add(-m.retryBackoff)}},
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
		return indexMigrationFailedRecently, nil
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
	var notCreated []string
	for _, result := range results {
		if !result.ok() {
			status = indexMigrationStatusCompletedWithErrors
			notCreated = append(notCreated, result.Collection+"."+result.Name)
		}
	}

	now := m.now()
	res, err := m.db.Collection(collectionSchemaMigrations).UpdateOne(ctx,
		bson.M{"_id": migration.ID, "owner": m.owner},
		bson.M{"$set": bson.M{
			"status":      status,
			"finished_at": now,
			"lease_until": now,
			"indexes":     results,
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
	case indexOutcomeCreated, indexOutcomeExists, indexOutcomeExistsOtherName:
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

// ensureIndex puts one index in place and reports what it did. It never
// modifies an existing index and only drops the indexes named in Replaces.
func (m *indexMigrator) ensureIndex(ctx context.Context, def indexDefinition) indexMigrationResult {
	result := indexMigrationResult{Collection: def.Collection, Name: def.Name}
	col := m.db.Collection(def.Collection)

	existing, err := listExistingIndexes(ctx, col)
	if err != nil {
		result.Outcome = indexOutcomeFailed
		result.Error = err.Error()
		return result
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
			return result
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
		if _, err := col.Indexes().CreateOne(ctx, def.model(target)); err != nil {
			result.Outcome = classifyIndexBuildError(err)
			result.Error = err.Error()
			return result
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

	return result
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
			_, err := m.db.Collection(collectionSchemaMigrations).UpdateOne(ctx,
				bson.M{"_id": id, "owner": m.owner, "status": indexMigrationStatusRunning},
				bson.M{"$set": bson.M{"lease_until": m.now().Add(m.lease)}},
			)
			if err != nil && ctx.Err() == nil {
				m.logger.Warn("MongoDB index migration could not renew its lease",
					tag.NewStringTag("migration", id), tag.Error(err))
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
