// Temporal MongoDB schema bootstrap (v1.0)
//
// Usage:
//   env TEMPORAL_MONGO_DB="temporal" \
//   env TEMPORAL_MONGO_VISIBILITY_DB="temporal_visibility" \
//   mongosh "mongodb://localhost:27017" init.js
//
// Running this script is optional. The server creates its collections on
// first use, creates the baseline indexes when its stores start, and adds the
// remaining indexes with a background index migration (see
// common/persistence/mongodb/index_migration.go). Use the script to prepare a
// database before the first server starts, or to build indexes ahead of an
// upgrade so that the server finds them in place.
//
// The index lists below mirror the Go code and must be kept in sync with it:
//   - "baseline": created by the store constructors (ensure*Indexes).
//   - "migration": created by the background index migrations.
//
// The script is idempotent and follows the same rules as the migration:
//   - an index that already exists with the same keys and options is accepted,
//     also when it has another name;
//   - if the intended name is used by a different index, the index is created
//     with the "idx_" prefix; if that name is taken as well, nothing is changed
//     and the conflict is reported;
//   - an index that cannot be created (for example a unique index over data
//     that contains duplicates) is reported and skipped, the rest still runs;
//   - no index is dropped, except the ones listed in obsoleteIndexes, which
//     earlier versions of this script created by mistake.
//
// The script exits with a non-zero status if any index could not be created.

(function () {
  const primaryDbName = (typeof process !== "undefined" && process.env.TEMPORAL_MONGO_DB) || "temporal";
  const visibilityDbName =
    (typeof process !== "undefined" && process.env.TEMPORAL_MONGO_VISIBILITY_DB) || primaryDbName;

  const INDEX_NAME_FALLBACK_PREFIX = "idx_";
  const problems = [];

  function sameKeys(a, b) {
    const aKeys = Object.keys(a);
    const bKeys = Object.keys(b);
    if (aKeys.length !== bKeys.length) {
      return false;
    }
    for (let i = 0; i < aKeys.length; i++) {
      if (aKeys[i] !== bKeys[i] || String(a[aKeys[i]]) !== String(b[bKeys[i]])) {
        return false;
      }
    }
    return true;
  }

  // matches reports whether an existing index has the keys and options of spec.
  function matches(spec, existing) {
    const options = spec.options || {};
    if (!sameKeys(spec.keys, existing.key)) {
      return false;
    }
    if (Boolean(options.unique) !== Boolean(existing.unique)) {
      return false;
    }
    if (existing.sparse || existing.partialFilterExpression) {
      return false;
    }
    const wantTtl = options.expireAfterSeconds;
    const haveTtl = existing.expireAfterSeconds;
    if ((wantTtl === undefined) !== (haveTtl === undefined)) {
      return false;
    }
    return wantTtl === undefined || Number(wantTtl) === Number(haveTtl);
  }

  function ensureIndex(collection, spec) {
    const fullName = `${collection.getFullName()}.${spec.name}`;
    const existing = collection.getIndexes();
    const byName = (name) => existing.find((idx) => idx.name === name);

    let target = spec.name;
    let sameName = byName(target);
    if (sameName && !matches(spec, sameName)) {
      target = INDEX_NAME_FALLBACK_PREFIX + spec.name;
      print(`Index name ${fullName} is used by a different index, using ${target}`);
      sameName = byName(target);
      if (sameName && !matches(spec, sameName)) {
        problems.push(`${fullName}: names ${spec.name} and ${target} are both used by different indexes`);
        return;
      }
    }

    if (sameName) {
      return;
    }
    const equivalent = existing.find((idx) => matches(spec, idx));
    if (equivalent) {
      print(`Index ${fullName} already exists as ${equivalent.name}`);
      return;
    }

    try {
      collection.createIndex(spec.keys, Object.assign({}, spec.options || {}, { name: target }));
      print(`Created index ${collection.getFullName()}.${target}`);
    } catch (err) {
      const reason = err.code === 11000 ? "the collection contains duplicates" : err.message;
      problems.push(`${fullName}: not created (${reason})`);
    }
  }

  function ensureCollection(database, spec) {
    const { name, options, indexes } = spec;
    if (!database.getCollectionNames().includes(name)) {
      database.createCollection(name, options || {});
      print(`Created collection ${database.getName()}.${name}`);
    }
    const collection = database.getCollection(name);
    (indexes || []).forEach((idx) => ensureIndex(collection, idx));
  }

  const primaryDb = db.getSiblingDB(primaryDbName);

  const primaryCollections = [
    {
      name: "shards",
    },
    {
      name: "namespaces",
      indexes: [
        // baseline
        { name: "namespaces_name_unique", keys: { name: 1 }, options: { unique: true } },
      ],
    },
    { name: "namespace_metadata" },
    { name: "cluster_metadata" },
    {
      name: "cluster_membership",
      indexes: [
        // migration primary-indexes-001
        { name: "cluster_membership_ttl", keys: { record_expiry: 1 }, options: { expireAfterSeconds: 0 } },
      ],
    },
    {
      name: "executions",
      indexes: [
        // baseline
        {
          name: "executions_shard_namespace_workflow_run",
          keys: { shard_id: 1, namespace_id: 1, workflow_id: 1, run_id: 1 },
        },
        {
          name: "executions_namespace_workflow_run",
          keys: { namespace_id: 1, workflow_id: 1, run_id: 1 },
        },
        // migration primary-indexes-001
        { name: "executions_shard_id", keys: { shard_id: 1, _id: 1 } },
      ],
    },
    {
      name: "current_executions",
      indexes: [
        // baseline
        {
          name: "current_executions_namespace_workflow",
          keys: { namespace_id: 1, workflow_id: 1 },
          options: { unique: true },
        },
        {
          name: "current_executions_shard",
          keys: { shard_id: 1, namespace_id: 1, workflow_id: 1 },
        },
      ],
    },
    {
      name: "history_branches",
      indexes: [
        // migration primary-indexes-001
        {
          name: "history_branches_shard_tree_branch",
          keys: { shard_id: 1, tree_id: 1, branch_id: 1 },
          options: { unique: true },
        },
      ],
    },
    {
      name: "history_nodes",
      indexes: [
        // migration primary-indexes-001
        {
          name: "history_nodes_branch_node_txn",
          keys: { shard_id: 1, tree_id: 1, branch_id: 1, node_id: 1, txn_id: -1 },
          options: { unique: true },
        },
      ],
    },
    { name: "transfer_tasks" },
    { name: "timer_tasks" },
    { name: "replication_tasks" },
    { name: "visibility_tasks" },
    { name: "replication_dlq_tasks" },
    {
      name: "task_queues",
      indexes: [
        // baseline. Not unique: the v1 and v2 (fairness) documents of one task
        // queue share these fields and differ only in _id.
        {
          name: "task_queue_lookup",
          keys: { namespace_id: 1, task_queue: 1, task_type: 1, subqueue: 1 },
        },
      ],
    },
    {
      name: "tasks",
      indexes: [
        // baseline
        {
          name: "tasks_lookup",
          keys: { namespace_id: 1, task_queue: 1, task_type: 1, subqueue: 1, task_pass: 1, task_id: 1 },
        },
        { name: "tasks_ttl", keys: { expiry_time: 1 }, options: { expireAfterSeconds: 0 } },
      ],
    },
    {
      name: "task_queue_user_data",
      indexes: [
        // baseline
        {
          name: "task_queue_user_data_unique",
          keys: { namespace_id: 1, task_queue: 1 },
          options: { unique: true },
        },
      ],
    },
    {
      name: "task_queue_build_id_map",
      indexes: [
        // baseline
        {
          name: "task_queue_build_id_unique",
          keys: { namespace_id: 1, build_id: 1, task_queue: 1 },
          options: { unique: true },
        },
        { name: "task_queue_build_id_lookup", keys: { namespace_id: 1, build_id: 1 } },
      ],
    },
    {
      name: "queue_metadata",
      indexes: [
        // baseline
        { name: "queue_metadata_lookup", keys: { queue_type: 1 }, options: { unique: true } },
      ],
    },
    {
      name: "queue_messages",
      indexes: [
        // baseline
        { name: "queue_messages_lookup", keys: { queue_type: 1, message_id: 1 } },
        { name: "queue_messages_lookup_desc", keys: { queue_type: 1, message_id: -1 } },
      ],
    },
    {
      name: "queue_v2_metadata",
      indexes: [
        // baseline
        { name: "queue_v2_metadata_lookup", keys: { queue_type: 1, queue_name: 1 }, options: { unique: true } },
      ],
    },
    {
      name: "queue_v2_messages",
      indexes: [
        // baseline
        {
          name: "queue_v2_messages_lookup",
          keys: { queue_type: 1, queue_name: 1, partition: 1, message_id: 1 },
        },
        {
          name: "queue_v2_messages_lookup_desc",
          keys: { queue_type: 1, queue_name: 1, partition: 1, message_id: -1 },
        },
      ],
    },
    // The Nexus collections are keyed by _id, which MongoDB always indexes
    // uniquely. An explicit _id index must not be declared: the server rejects
    // the "unique" option on it.
    { name: "nexus_endpoints_metadata" },
    { name: "nexus_endpoints" },
  ];

  const visibilityFiltered = (name, field) => ({
    name,
    keys: { namespace_id: 1, [field]: 1, visibility_time: -1, run_id: 1 },
  });

  const visibilityCollections = [
    {
      name: "visibility_executions",
      indexes: [
        // baseline
        {
          name: "visibility_namespace_workflow_run",
          keys: { namespace_id: 1, workflow_id: 1, run_id: 1 },
          options: { unique: true },
        },
        { name: "visibility_start_time", keys: { namespace_id: 1, start_time: -1, run_id: 1 } },
        { name: "visibility_close_time", keys: { namespace_id: 1, close_time: -1, run_id: 1 } },
        { name: "visibility_visibility_time", keys: { namespace_id: 1, visibility_time: -1, run_id: 1 } },
        // migration visibility-indexes-001
        visibilityFiltered("visibility_status_time", "status"),
        visibilityFiltered("visibility_workflow_type_time", "workflow_type_name"),
        visibilityFiltered("visibility_workflow_id_time", "workflow_id"),
        visibilityFiltered("visibility_task_queue_time", "task_queue"),
        { name: "visibility_namespace_run", keys: { namespace_id: 1, run_id: 1 } },
        { name: "visibility_search_attributes_wildcard", keys: { "search_attributes.$**": 1 } },
      ],
    },
    {
      name: "visibility_search_attributes",
      indexes: [
        // baseline
        { name: "visibility_sa_name_unique", keys: { name: 1 }, options: { unique: true } },
      ],
    },
  ];

  // Indexes that earlier versions of this script created and that must not
  // exist. They are dropped after the indexes above are in place.
  const obsoleteIndexes = [
    {
      database: primaryDb,
      collection: "task_queues",
      name: "task_queue_unique",
      // Same fields as task_queue_lookup but unique, which makes the creation
      // of the second version (v1/v2) of a task queue fail.
      keys: { namespace_id: 1, task_queue: 1, task_type: 1, subqueue: 1 },
    },
  ];

  primaryCollections.forEach((spec) => ensureCollection(primaryDb, spec));

  const visibilityDb = db.getSiblingDB(visibilityDbName);
  visibilityCollections.forEach((spec) => ensureCollection(visibilityDb, spec));

  obsoleteIndexes.forEach((obsolete) => {
    const collection = obsolete.database.getCollection(obsolete.collection);
    const existing = collection.getIndexes().find((idx) => idx.name === obsolete.name);
    if (!existing || !sameKeys(obsolete.keys, existing.key)) {
      return;
    }
    try {
      collection.dropIndex(obsolete.name);
      print(`Dropped obsolete index ${collection.getFullName()}.${obsolete.name}`);
    } catch (err) {
      problems.push(`${collection.getFullName()}.${obsolete.name}: obsolete index not dropped (${err.message})`);
    }
  });

  if (problems.length > 0) {
    print(`Temporal MongoDB schema v1.0 applied with ${problems.length} problem(s):`);
    problems.forEach((problem) => print(`  - ${problem}`));
    quit(1);
  }

  print(`Temporal MongoDB schema v1.0 applied to ${primaryDbName} (primary) and ${visibilityDbName} (visibility)`);
})();
