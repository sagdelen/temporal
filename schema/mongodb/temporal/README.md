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

## Next Steps

- Extend `init.js` as new collections or indexes are introduced. An index added to an index migration in the Go code must be added to `init.js` as well; `TestIndexMigration_InitScriptListsMigrationIndexes` checks this.
- Add migration notes under new version directories when backward-incompatible changes are required.
- Coordinate schema updates with documentation updates under `docs/development/persistence/mongodb-persistence-plugin.md`.
