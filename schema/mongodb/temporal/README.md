# Temporal MongoDB Schema

This directory contains versioned MongoDB DDL artifacts for the Temporal persistence plugin.

## Layout

```
schema/mongodb/temporal/
├── README.md              # This file
└── versioned/
    └── v1.0/              # Initial schema definition (subject to updates)
        ├── README.md
        └── init.js        # Helper script creating collections and baseline indexes
```

## Conventions

- Scripts are written for `mongosh`. They can be executed locally or as part of a deployment pipeline.
- Each version directory should contain idempotent helpers. Future migrations can be added as additional scripts within the same version folder or as new version folders.
- Collections and indexes defined here mirror the names used in `common/persistence/mongodb/*`.
- Running the scripts is optional. The server creates its collections on first use, creates the baseline indexes when its stores start, and adds the remaining indexes with a background index migration (`common/persistence/mongodb/index_migration.go`). Use the scripts to prepare a database before the first server starts, or to build indexes ahead of an upgrade.

## Applying The Schema

```
# Example: apply v1.0 baseline
env MONGO_URI="mongodb://localhost:27017" \
env TEMPORAL_MONGO_DB="temporal" \
mongosh "$MONGO_URI" schema/mongodb/temporal/versioned/v1.0/init.js
```

> **Note**
> The scripts do not drop or mutate existing collections. They create collections and indexes when missing, so they are safe to rerun. The only indexes they drop are the ones an earlier version of the script created by mistake (listed as `obsoleteIndexes` in `init.js`). A script exits with a non-zero status when an index could not be created, for example a unique index over data that contains duplicates.

## Index migrations

After it starts, a server builds the indexes that are missing in the background and records the outcome in the `schema_migrations` collection, one document per migration:

- `status: "completed"`: every index is in place. The server logs `MongoDB index migration completed`.
- `status: "completed_with_errors"`: at least one index is missing; `indexes[].outcome` and `indexes[].error` say which and why. Transient errors are retried by the servers after a backoff.
- `needs_operator: true`: a unique index could not be built because the collection contains duplicates. This is not retried on its own. Fix the data, delete the migration's document from `schema_migrations`, and restart a server.

Indexes that already exist with the same definition, for example after running `init.js`, are accepted and not built again.

To keep servers from building indexes at all, set `disableIndexMigration: true` in the MongoDB datastore config (`MONGODB_DISABLE_INDEX_MIGRATION=true` with the Docker config template). A server then only logs which indexes are missing, and `init.js` is the way to build them.

## Next Steps

- Extend `init.js` as new collections or indexes are introduced. An index added to an index migration in the Go code must be added to `init.js` as well; `TestIndexMigration_InitScriptListsMigrationIndexes` checks this.
- Add migration notes under new version directories when backward-incompatible changes are required.
- Coordinate schema updates with documentation updates under `docs/development/persistence/mongodb-persistence-plugin.md`.
