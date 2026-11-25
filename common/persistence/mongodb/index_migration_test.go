package mongodb_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence/mongodb"
	"go.temporal.io/server/common/persistence/mongodb/client"
)

// These tests run the background index migration against a real MongoDB
// replica set. They are skipped when MongoDB is not reachable. See
// MONGODB_SEEDS / TEMPORAL_MONGODB_TEST_* in test_config_test.go for
// connection settings.

type (
	indexMigrationEnv struct {
		cfg    config.MongoDB
		raw    client.Database
		logger *recordingLogger
	}

	// recordingLogger keeps the messages logged at each level.
	recordingLogger struct {
		log.Logger
		mu     sync.Mutex
		infos  []string
		warns  []string
		errors []string
	}

	storedIndex struct {
		Name               string   `bson:"name"`
		Key                bson.D   `bson:"key"`
		Unique             bool     `bson:"unique"`
		ExpireAfterSeconds *float64 `bson:"expireAfterSeconds"`
	}
)

func (l *recordingLogger) Info(msg string, _ ...tag.Tag) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}

func (l *recordingLogger) Warn(msg string, _ ...tag.Tag) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *recordingLogger) Error(msg string, _ ...tag.Tag) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, msg)
}

func (l *recordingLogger) count(messages *[]string, prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, msg := range *messages {
		if strings.HasPrefix(msg, prefix) {
			n++
		}
	}
	return n
}

func newIndexMigrationEnv(t *testing.T) *indexMigrationEnv {
	t.Helper()

	cfg := newMongoTestConfig(fmt.Sprintf("test_idx_mig_%d", time.Now().UnixNano()))
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

	return &indexMigrationEnv{cfg: cfg, raw: raw, logger: &recordingLogger{Logger: log.NewNoopLogger()}}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (e *indexMigrationEnv) run(t *testing.T, migration mongodb.IndexMigrationForTest, opts mongodb.IndexMigrationOptionsForTest) string {
	t.Helper()
	return mongodb.RunIndexMigrationForTest(testCtx(t), e.raw, e.logger, migration, opts)
}

func (e *indexMigrationEnv) indexes(t *testing.T, collection string) map[string]storedIndex {
	t.Helper()
	ctx := testCtx(t)
	cursor, err := e.raw.Collection(collection).Indexes().List(ctx)
	require.NoError(t, err)
	var list []storedIndex
	require.NoError(t, cursor.All(ctx, &list))
	byName := make(map[string]storedIndex, len(list))
	for _, idx := range list {
		byName[idx.Name] = idx
	}
	return byName
}

func (e *indexMigrationEnv) record(t *testing.T, id string) mongodb.IndexMigrationDocumentForTest {
	t.Helper()
	var doc mongodb.IndexMigrationDocumentForTest
	require.NoError(t, e.raw.Collection(mongodb.SchemaMigrationsCollectionForTest).
		FindOne(testCtx(t), bson.M{"_id": id}).Decode(&doc))
	return doc
}

func (e *indexMigrationEnv) createIndex(t *testing.T, collection, name string, keys bson.D, unique bool) {
	t.Helper()
	opts := options.Index().SetName(name)
	if unique {
		opts.SetUnique(true)
	}
	_, err := e.raw.Collection(collection).Indexes().CreateOne(testCtx(t), mongo.IndexModel{Keys: keys, Options: opts})
	require.NoError(t, err)
}

func (e *indexMigrationEnv) outcomes(t *testing.T, id string) map[string]string {
	t.Helper()
	outcomes := map[string]string{}
	for _, idx := range e.record(t, id).Indexes {
		outcomes[idx.Name] = idx.Outcome
	}
	return outcomes
}

func noBackoff() *time.Duration {
	d := time.Duration(0)
	return &d
}

var abKeys = bson.D{{Key: "a", Value: 1}, {Key: "b", Value: -1}}

func oneIndexMigration(id string, def mongodb.IndexDefinitionForTest) mongodb.IndexMigrationForTest {
	return mongodb.IndexMigrationForTest{ID: id, Indexes: []mongodb.IndexDefinitionForTest{def}}
}

// The shipped migrations create every index, record completion, log it, and
// are a no-op the second time.
func TestIndexMigration_CreatesIndexesAndRecordsCompletion(t *testing.T) {
	env := newIndexMigrationEnv(t)

	for _, migration := range []mongodb.IndexMigrationForTest{
		mongodb.PrimaryIndexMigrationForTest(),
		mongodb.VisibilityIndexMigrationForTest(),
	} {
		require.Equal(t, "completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))

		for _, def := range migration.Indexes {
			idx, ok := env.indexes(t, def.Collection)[def.Name]
			require.Truef(t, ok, "index %s.%s was not created", def.Collection, def.Name)
			require.Equalf(t, def.Unique, idx.Unique, "%s.%s unique flag", def.Collection, def.Name)
			require.Equalf(t, def.ExpireAfterSeconds != nil, idx.ExpireAfterSeconds != nil, "%s.%s TTL", def.Collection, def.Name)
		}

		doc := env.record(t, migration.ID)
		require.Equal(t, "completed", doc.Status)
		require.NotNil(t, doc.FinishedAt)
		require.Len(t, doc.Indexes, len(migration.Indexes))
		for _, idx := range doc.Indexes {
			require.Equalf(t, "created", idx.Outcome, "%s.%s", idx.Collection, idx.Name)
		}

		require.Equal(t, "already_completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))
	}

	require.Equal(t, 2, env.logger.count(&env.logger.infos, "MongoDB index migration completed"))
	require.Empty(t, env.logger.errors)
}

// An index that is already there is accepted without an error, also when it
// has the same keys and options under another name.
func TestIndexMigration_ExistingIndexIsAccepted(t *testing.T) {
	env := newIndexMigrationEnv(t)
	env.createIndex(t, "coll_same", "wanted", abKeys, true)
	env.createIndex(t, "coll_other_name", "custom_name", abKeys, true)

	migration := mongodb.IndexMigrationForTest{ID: "existing", Indexes: []mongodb.IndexDefinitionForTest{
		{Collection: "coll_same", Name: "wanted", Keys: abKeys, Unique: true},
		{Collection: "coll_other_name", Name: "wanted", Keys: abKeys, Unique: true},
	}}
	require.Equal(t, "completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))

	doc := env.record(t, "existing")
	require.Equal(t, "exists", doc.Indexes[0].Outcome)
	require.Equal(t, "exists_other_name", doc.Indexes[1].Outcome)
	require.Equal(t, "custom_name", doc.Indexes[1].CreatedAs)

	// No second index on the same keys was built.
	require.Len(t, env.indexes(t, "coll_other_name"), 2) // _id_ and custom_name
	require.Empty(t, env.logger.errors)
}

// When the intended name is used by a different index, the new index is
// created with the idx_ prefix and the other index is left alone.
func TestIndexMigration_NameTakenFallsBackToPrefix(t *testing.T) {
	env := newIndexMigrationEnv(t)
	otherKeys := bson.D{{Key: "x", Value: 1}}
	env.createIndex(t, "coll", "wanted", otherKeys, false)

	def := mongodb.IndexDefinitionForTest{Collection: "coll", Name: "wanted", Keys: abKeys, Unique: true}
	require.Equal(t, "completed", env.run(t, oneIndexMigration("fallback-1", def), mongodb.IndexMigrationOptionsForTest{}))

	fallback := mongodb.IndexNameFallbackPrefixForTest + "wanted"
	indexes := env.indexes(t, "coll")
	require.Contains(t, indexes, fallback)
	require.True(t, indexes[fallback].Unique)
	require.Contains(t, indexes, "wanted", "the index that owns the name must be left alone")
	require.False(t, indexes["wanted"].Unique)
	require.Equal(t, fallback, env.record(t, "fallback-1").Indexes[0].CreatedAs)

	// A later run finds the prefixed index and accepts it.
	require.Equal(t, "completed", env.run(t, oneIndexMigration("fallback-2", def), mongodb.IndexMigrationOptionsForTest{}))
	require.Equal(t, "exists", env.record(t, "fallback-2").Indexes[0].Outcome)
	require.Len(t, env.indexes(t, "coll"), 3) // _id_, wanted, idx_wanted
}

// When the prefixed name is taken by a different index as well, nothing is
// changed and the conflict is reported.
func TestIndexMigration_NameAndFallbackTakenIsReported(t *testing.T) {
	env := newIndexMigrationEnv(t)
	env.createIndex(t, "coll", "wanted", bson.D{{Key: "x", Value: 1}}, false)
	env.createIndex(t, "coll", mongodb.IndexNameFallbackPrefixForTest+"wanted", bson.D{{Key: "y", Value: 1}}, false)

	def := mongodb.IndexDefinitionForTest{Collection: "coll", Name: "wanted", Keys: abKeys}
	require.Equal(t, "completed_with_errors", env.run(t, oneIndexMigration("conflict", def), mongodb.IndexMigrationOptionsForTest{}))

	doc := env.record(t, "conflict")
	require.Equal(t, "completed_with_errors", doc.Status)
	require.Equal(t, "conflict", doc.Indexes[0].Outcome)
	require.NotEmpty(t, doc.Indexes[0].Error)
	require.Len(t, env.indexes(t, "coll"), 3) // unchanged: _id_, wanted, idx_wanted
	require.Equal(t, 1, env.logger.count(&env.logger.errors, "MongoDB index migration finished with errors"))
}

// A non-unique index is replaced by a unique one: the new index is built
// first and the old one is dropped afterwards, but only when it is named in
// Replaces.
func TestIndexMigration_ReplacesNonUniqueIndex(t *testing.T) {
	env := newIndexMigrationEnv(t)
	env.createIndex(t, "coll_listed", "old_idx", abKeys, false)
	env.createIndex(t, "coll_same_name", "wanted", abKeys, false)
	env.createIndex(t, "coll_not_listed", "old_idx", abKeys, false)

	migration := mongodb.IndexMigrationForTest{ID: "replace", Indexes: []mongodb.IndexDefinitionForTest{
		{Collection: "coll_listed", Name: "wanted", Keys: abKeys, Unique: true, Replaces: []string{"old_idx"}},
		// The old index owns the intended name: the new one gets the prefix.
		{Collection: "coll_same_name", Name: "wanted", Keys: abKeys, Unique: true, Replaces: []string{"wanted"}},
		{Collection: "coll_not_listed", Name: "wanted", Keys: abKeys, Unique: true},
	}}
	require.Equal(t, "completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))

	listed := env.indexes(t, "coll_listed")
	require.True(t, listed["wanted"].Unique)
	require.NotContains(t, listed, "old_idx", "the replaced index must be dropped")

	fallback := mongodb.IndexNameFallbackPrefixForTest + "wanted"
	sameName := env.indexes(t, "coll_same_name")
	require.True(t, sameName[fallback].Unique)
	require.NotContains(t, sameName, "wanted", "the replaced index must be dropped")

	notListed := env.indexes(t, "coll_not_listed")
	require.True(t, notListed["wanted"].Unique)
	require.Contains(t, notListed, "old_idx", "an index that is not named in Replaces must never be dropped")

	doc := env.record(t, "replace")
	require.Equal(t, []string{"old_idx"}, doc.Indexes[0].Dropped)
	require.Equal(t, []string{"wanted"}, doc.Indexes[1].Dropped)
	require.Empty(t, doc.Indexes[2].Dropped)
}

// A unique index over duplicate data is not created: the failure is logged
// and recorded, the index it should replace stays, and the other indexes of
// the migration are still created. After the data is fixed the migration
// completes, but not before the retry backoff has passed.
func TestIndexMigration_DuplicateDataBlocksOnlyThatIndex(t *testing.T) {
	env := newIndexMigrationEnv(t)
	ctx := testCtx(t)

	dup := env.raw.Collection("coll_dup")
	_, err := dup.InsertOne(ctx, bson.M{"_id": 1, "a": 1, "b": 1})
	require.NoError(t, err)
	_, err = dup.InsertOne(ctx, bson.M{"_id": 2, "a": 1, "b": 1})
	require.NoError(t, err)
	env.createIndex(t, "coll_dup", "old_idx", abKeys, false)

	migration := mongodb.IndexMigrationForTest{ID: "duplicates", Indexes: []mongodb.IndexDefinitionForTest{
		{Collection: "coll_dup", Name: "wanted", Keys: abKeys, Unique: true, Replaces: []string{"old_idx"}},
		{Collection: "coll_ok", Name: "wanted", Keys: abKeys},
	}}
	require.Equal(t, "completed_with_errors", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))

	require.NotContains(t, env.indexes(t, "coll_dup"), "wanted", "a unique index over duplicates must not exist")
	require.Contains(t, env.indexes(t, "coll_dup"), "old_idx", "the old index must stay when its replacement failed")
	require.Contains(t, env.indexes(t, "coll_ok"), "wanted", "the other indexes must still be created")

	doc := env.record(t, "duplicates")
	require.Equal(t, "completed_with_errors", doc.Status)
	require.Equal(t, "duplicate_data", doc.Indexes[0].Outcome)
	require.Contains(t, doc.Indexes[0].Error, "E11000")
	require.Equal(t, "created", doc.Indexes[1].Outcome)
	require.Equal(t, 1, env.logger.count(&env.logger.errors, "MongoDB index migration: index not created"))
	require.Equal(t, 1, env.logger.count(&env.logger.errors, "MongoDB index migration finished with errors"))
	require.Zero(t, env.logger.count(&env.logger.infos, "MongoDB index migration completed"))

	// Within the backoff the failed build is not attempted again.
	require.Equal(t, "skipped_after_errors", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))

	// Still failing after the backoff while the duplicates are there.
	require.Equal(t, "completed_with_errors", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{RetryBackoff: noBackoff()}))

	_, err = dup.DeleteOne(ctx, bson.M{"_id": 2})
	require.NoError(t, err)
	require.Equal(t, "completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{RetryBackoff: noBackoff()}))

	indexes := env.indexes(t, "coll_dup")
	require.True(t, indexes["wanted"].Unique)
	require.NotContains(t, indexes, "old_idx")
	require.Equal(t, "completed", env.record(t, "duplicates").Status)
	require.Equal(t, 1, env.logger.count(&env.logger.infos, "MongoDB index migration completed"))
}

// Several processes start at once: exactly one executes the migration, the
// others wait and see it completed.
func TestIndexMigration_ConcurrentRunnersExecuteOnce(t *testing.T) {
	env := newIndexMigrationEnv(t)
	migration := mongodb.PrimaryIndexMigrationForTest()

	const runners = 6
	results := make([]string, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = env.run(t, migration, mongodb.IndexMigrationOptionsForTest{
				Owner: fmt.Sprintf("runner-%d", i),
				Poll:  50 * time.Millisecond,
			})
		}(i)
	}
	wg.Wait()

	executed := 0
	for _, result := range results {
		switch result {
		case "completed":
			executed++
		case "already_completed":
		default:
			t.Errorf("unexpected outcome %q", result)
		}
	}
	require.Equal(t, 1, executed, "exactly one runner must execute the migration: %v", results)
	require.Equal(t, 1, env.logger.count(&env.logger.infos, "MongoDB index migration started"))
	require.Equal(t, "completed", env.record(t, migration.ID).Status)
}

// A migration whose owner died is taken over once its lease has expired, and
// not before.
func TestIndexMigration_LeaseTakeover(t *testing.T) {
	env := newIndexMigrationEnv(t)
	migration := oneIndexMigration("lease", mongodb.IndexDefinitionForTest{Collection: "coll", Name: "wanted", Keys: abKeys})
	records := env.raw.Collection(mongodb.SchemaMigrationsCollectionForTest)
	now := time.Now().UTC()

	_, err := records.InsertOne(testCtx(t), bson.M{
		"_id":         "lease",
		"status":      "running",
		"owner":       "another-process",
		"lease_until": now.Add(time.Hour),
		"started_at":  now,
	})
	require.NoError(t, err)

	// The lease is alive: this runner waits until its context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	outcome := mongodb.RunIndexMigrationForTest(ctx, env.raw, env.logger, migration,
		mongodb.IndexMigrationOptionsForTest{Poll: 100 * time.Millisecond})
	require.Equal(t, "interrupted", outcome)
	require.NotContains(t, env.indexes(t, "coll"), "wanted")
	require.Equal(t, "another-process", env.record(t, "lease").Owner)

	// The owner stopped renewing: the lease expires and is taken over.
	_, err = records.UpdateOne(testCtx(t), bson.M{"_id": "lease"},
		bson.M{"$set": bson.M{"lease_until": now.Add(-time.Minute)}})
	require.NoError(t, err)

	require.Equal(t, "completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{Owner: "survivor"}))
	require.Contains(t, env.indexes(t, "coll"), "wanted")
	doc := env.record(t, "lease")
	require.Equal(t, "completed", doc.Status)
	require.Equal(t, "survivor", doc.Owner)
}

func (e *indexMigrationEnv) explain(t *testing.T, collection string, filter, sort bson.D) string {
	t.Helper()
	var plan bson.M
	require.NoError(t, e.raw.RunCommand(testCtx(t), bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "find", Value: collection},
			{Key: "filter", Value: filter},
			{Key: "sort", Value: sort},
		}},
		{Key: "verbosity", Value: "queryPlanner"},
	}).Decode(&plan))
	planner, ok := plan["queryPlanner"].(bson.M)
	require.True(t, ok, "explain output has no queryPlanner section")
	winning, err := bson.MarshalExtJSON(bson.M{"winningPlan": planner["winningPlan"]}, false, false)
	require.NoError(t, err)
	return string(winning)
}

// The history queries that used to scan the whole collection are served by
// the new indexes.
func TestIndexMigration_HistoryQueriesUseNewIndexes(t *testing.T) {
	env := newIndexMigrationEnv(t)
	require.Equal(t, "completed", env.run(t, mongodb.PrimaryIndexMigrationForTest(), mongodb.IndexMigrationOptionsForTest{}))

	// ReadHistoryBranch, forward order, second page onwards.
	nodesPage := env.explain(t, "history_nodes",
		bson.D{
			{Key: "shard_id", Value: 1},
			{Key: "tree_id", Value: "tree"},
			{Key: "branch_id", Value: "branch"},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "node_id", Value: bson.D{{Key: "$gt", Value: 5}}}},
				bson.D{{Key: "node_id", Value: 5}, {Key: "txn_id", Value: bson.D{{Key: "$lt", Value: 9}}}},
			}},
		},
		bson.D{{Key: "node_id", Value: 1}, {Key: "txn_id", Value: -1}},
	)
	t.Logf("history_nodes page plan: %s", nodesPage)
	require.Contains(t, nodesPage, "IXSCAN")
	require.Contains(t, nodesPage, "history_nodes_branch_node_txn")
	require.NotContains(t, nodesPage, "COLLSCAN")

	// DeleteHistoryBranch node range.
	nodesDelete := env.explain(t, "history_nodes",
		bson.D{
			{Key: "shard_id", Value: 1},
			{Key: "tree_id", Value: "tree"},
			{Key: "branch_id", Value: "branch"},
			{Key: "node_id", Value: bson.D{{Key: "$gte", Value: 3}}},
		},
		bson.D{},
	)
	require.Contains(t, nodesDelete, "history_nodes_branch_node_txn")
	require.NotContains(t, nodesDelete, "COLLSCAN")

	// GetHistoryTreeContainingBranch.
	branches := env.explain(t, "history_branches",
		bson.D{{Key: "shard_id", Value: 1}, {Key: "tree_id", Value: "tree"}},
		bson.D{{Key: "branch_id", Value: 1}},
	)
	t.Logf("history_branches plan: %s", branches)
	require.Contains(t, branches, "history_branches_shard_tree_branch")
	require.NotContains(t, branches, "COLLSCAN")
	require.NotContains(t, branches, `"SORT"`, "the index must provide the sort order")

	// ListConcreteExecutions.
	executions := env.explain(t, "executions",
		bson.D{{Key: "shard_id", Value: 1}, {Key: "_id", Value: bson.D{{Key: "$gt", Value: "ns|wf|run"}}}},
		bson.D{{Key: "_id", Value: 1}},
	)
	t.Logf("executions plan: %s", executions)
	require.Contains(t, executions, "executions_shard_id")
	require.NotContains(t, executions, "COLLSCAN")
	require.NotContains(t, executions, `"SORT"`, "the index must provide the sort order")
}

// The factory starts both migrations in the background when the stores are
// created, without the caller waiting for them, and Close waits for them.
func TestIndexMigration_FactoryRunsMigrationsInBackground(t *testing.T) {
	env := newIndexMigrationEnv(t)

	factory, err := mongodb.NewFactory(env.cfg, "index-migration-test", log.NewNoopLogger(), metrics.NoopMetricsHandler)
	require.NoError(t, err)
	closed := false
	defer func() {
		if !closed {
			factory.Close()
		}
	}()

	_, err = factory.NewExecutionStore()
	require.NoError(t, err)
	_, err = factory.NewVisibilityStore(config.CustomDatastoreConfig{}, nil, nil, nil, nil, nil, log.NewNoopLogger(), metrics.NoopMetricsHandler)
	require.NoError(t, err)

	ids := []string{mongodb.PrimaryIndexMigrationForTest().ID, mongodb.VisibilityIndexMigrationForTest().ID}
	require.Eventually(t, func() bool {
		for _, id := range ids {
			var doc mongodb.IndexMigrationDocumentForTest
			err := env.raw.Collection(mongodb.SchemaMigrationsCollectionForTest).
				FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc)
			if err != nil || doc.Status != "completed" {
				return false
			}
		}
		return true
	}, 60*time.Second, 100*time.Millisecond, "the background migrations did not complete")

	require.Contains(t, env.indexes(t, "history_nodes"), "history_nodes_branch_node_txn")
	require.Contains(t, env.indexes(t, "visibility_executions"), "visibility_status_time")

	factory.Close()
	closed = true
}

// init.js lets operators build the schema ahead of the server. It must list
// every index of the shipped migrations, under the same name and collection.
func TestIndexMigration_InitScriptListsMigrationIndexes(t *testing.T) {
	script, err := os.ReadFile("../../../schema/mongodb/temporal/versioned/v1.0/init.js")
	require.NoError(t, err)
	content := string(script)

	for _, migration := range []mongodb.IndexMigrationForTest{
		mongodb.PrimaryIndexMigrationForTest(),
		mongodb.VisibilityIndexMigrationForTest(),
	} {
		require.Containsf(t, content, migration.ID, "init.js does not mention migration %s", migration.ID)
		for _, def := range migration.Indexes {
			require.Containsf(t, content, `"`+def.Name+`"`,
				"init.js does not list index %s.%s of migration %s", def.Collection, def.Name, migration.ID)
			require.Containsf(t, content, `name: "`+def.Collection+`"`,
				"init.js does not create collection %s", def.Collection)
		}
	}
}

// Indexes that were built ahead of the server (for example with init.js) are
// accepted as they are: the migration builds nothing.
func TestIndexMigration_PrebuiltIndexesAreNotRebuilt(t *testing.T) {
	env := newIndexMigrationEnv(t)

	migrations := []mongodb.IndexMigrationForTest{
		mongodb.PrimaryIndexMigrationForTest(),
		mongodb.VisibilityIndexMigrationForTest(),
	}
	for _, migration := range migrations {
		for _, def := range migration.Indexes {
			opts := options.Index().SetName(def.Name)
			if def.Unique {
				opts.SetUnique(true)
			}
			if def.ExpireAfterSeconds != nil {
				opts.SetExpireAfterSeconds(*def.ExpireAfterSeconds)
			}
			_, err := env.raw.Collection(def.Collection).Indexes().
				CreateOne(testCtx(t), mongo.IndexModel{Keys: def.Keys, Options: opts})
			require.NoError(t, err)
		}
	}

	for _, migration := range migrations {
		require.Equal(t, "completed", env.run(t, migration, mongodb.IndexMigrationOptionsForTest{}))
		doc := env.record(t, migration.ID)
		require.Len(t, doc.Indexes, len(migration.Indexes))
		for _, idx := range doc.Indexes {
			require.Equalf(t, "exists", idx.Outcome, "%s.%s must be accepted, not built", idx.Collection, idx.Name)
			require.Equal(t, idx.Name, idx.CreatedAs)
		}
	}
	require.Empty(t, env.logger.errors)
	require.Empty(t, env.logger.warns)
}

// RunIndexMigrations builds every index before it returns, and the background
// migrations the factory starts afterwards find nothing left to do.
func TestIndexMigration_FactoryRunIndexMigrations(t *testing.T) {
	env := newIndexMigrationEnv(t)

	factory, err := mongodb.NewFactory(env.cfg, "index-migration-test", log.NewNoopLogger(), metrics.NoopMetricsHandler)
	require.NoError(t, err)
	defer factory.Close()

	require.NoError(t, factory.RunIndexMigrations(testCtx(t)))

	for _, migration := range []mongodb.IndexMigrationForTest{
		mongodb.PrimaryIndexMigrationForTest(),
		mongodb.VisibilityIndexMigrationForTest(),
	} {
		require.Equal(t, "completed", env.record(t, migration.ID).Status)
		for _, def := range migration.Indexes {
			require.Containsf(t, env.indexes(t, def.Collection), def.Name, "%s.%s", def.Collection, def.Name)
		}
	}

	require.NoError(t, factory.RunIndexMigrations(testCtx(t)), "a second run is a no-op")
}
