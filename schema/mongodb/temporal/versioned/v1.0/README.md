# MongoDB Temporal Schema v1.0

The `init.js` helper provisions the collections and indexes of the MongoDB persistence plugin. Running it is optional: the server creates the same schema itself (baseline indexes when its stores start, the rest through a background index migration). It is intentionally conservative:

- Creates collections if they do not already exist.
- Ensures the indexes defined in the Go implementation exist: the baseline indexes of the store constructors and the indexes of the index migrations.
- Accepts an index that already exists with the same keys and options, also under another name.
- Creates an index as `idx_<name>` when the intended name is used by a different index.
- Reports and skips an index it cannot create (for example a unique index over data that contains duplicates), continues with the rest, and exits with a non-zero status.
- Does **not** drop or recreate collections or indexes, except `task_queues.task_queue_unique`, which an earlier version of this script created by mistake: it made the creation of the second version (v1/v2) of a task queue fail.

When an index is added to the Go implementation, add it here as well.

## Running

```
MONGO_URI="mongodb://localhost:27017" \
TEMPORAL_MONGO_DB="temporal" \
mongosh "$MONGO_URI" schema/mongodb/temporal/versioned/v1.0/init.js
```

Environment variables:

- `TEMPORAL_MONGO_DB` (optional) defaults to `temporal`.
- `TEMPORAL_MONGO_VISIBILITY_DB` (optional) can override the database used for the visibility store collections.

> The script prints one line per collection or index it creates and a summary line at the end. A rerun on an up-to-date database prints only the summary.
