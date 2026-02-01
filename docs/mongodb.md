# MongoDB Persistence for Temporal

> **Production-grade MongoDB support for Temporal Server**

This fork adds MongoDB as a persistence backend for Temporal, providing an alternative to Cassandra, PostgreSQL, and MySQL.

---

## Overview

MongoDB persistence implements all required Temporal persistence interfaces:

| Store            | Status | Description                            |
| ---------------- | ------ | -------------------------------------- |
| Execution Store  | ✅     | Workflow state, history, mutable state |
| Task Store       | ✅     | Task queues, activity/workflow tasks   |
| Visibility Store | ✅     | Workflow listing, search attributes    |
| Shard Store      | ✅     | Shard ownership, range IDs             |
| Cluster Metadata | ✅     | Cluster configuration                  |
| Namespace Store  | ✅     | Namespace definitions                  |

---

## Quick Start

### Using Docker

```bash
# Pull the MongoDB-enabled Temporal image
docker pull agdelen/temporal:1.30.0

# Start with Docker Compose (includes MongoDB)
docker compose -f docker/docker-compose-mongodb.yml up -d
```

### Configuration

```yaml
persistence:
  defaultStore: mongodb-default
  visibilityStore: mongodb-visibility
  datastores:
    mongodb-default:
      mongodb:
        hosts:
          - mongodb:27017
        database: temporal
        user: temporal
        password: temporal
    mongodb-visibility:
      mongodb:
        hosts:
          - mongodb:27017
        database: temporal_visibility
        user: temporal
        password: temporal
```

---

## Docker Images

### Available Tags

| Tag                               | Description                        |
| --------------------------------- | ---------------------------------- |
| `agdelen/temporal:1.30.0`         | Latest stable with MongoDB support |
| `agdelen/temporal:1.30.0-mongo.X` | Specific MongoDB release           |

### Building Your Own Image

```bash
# Clone this repository
git clone https://github.com/sagdelen/temporal.git
cd temporal

# Build the image
make docker-build

# Or with custom tag
docker build -t my-temporal:mongodb .
```

---

## Versioning

This fork follows a versioning scheme that tracks upstream Temporal:

```
{upstream-version}-mongo.{patch}

Example: 1.30.0-mongo.1
         │       │    │
         │       │    └── MongoDB-specific patch number
         │       └─────── Indicates MongoDB support
         └─────────────── Base Temporal version
```

### Version Compatibility

| MongoDB Fork   | Upstream Temporal | MongoDB Server |
| -------------- | ----------------- | -------------- |
| 1.30.0-mongo.1 | 1.30.0            | 6.0+           |

---

## Testing & Validation

MongoDB persistence is validated through comprehensive testing:

### Server Tests (this repository)

```bash
# Unit tests
make unit-test

# Integration tests (requires MongoDB)
make integration-test

# Functional tests
make functional-test
```

### Load & E2E Tests

Extensive load testing is maintained in a separate repository:

**📦 [sagdelen/temporal-mongodb-tests](https://github.com/sagdelen/temporal-mongodb-tests)**

| Test Suite | Coverage                  |
| ---------- | ------------------------- |
| E2E Tests  | 329 functional tests      |
| Load Tests | Omes-based stress testing |

```bash
# Clone test repository
git clone https://github.com/sagdelen/temporal-mongodb-tests.git
cd temporal-mongodb-tests

# Run all tests
mise run setup
mise run tests      # 329 E2E tests
mise run load       # Load tests
```

See the [test repository README](https://github.com/sagdelen/temporal-mongodb-tests) for detailed instructions.

---

## Production Considerations

### Recommended MongoDB Configuration

```yaml
# Replica set is recommended for production
replicaSet: rs0

# Connection pool settings
maxPoolSize: 100
minPoolSize: 10

# Write concern for durability
writeConcern:
  w: majority
  j: true

# Read preference
readPreference: primaryPreferred
```

### Resource Requirements

| Component       | Minimum | Recommended  |
| --------------- | ------- | ------------ |
| MongoDB Memory  | 4GB     | 16GB+        |
| MongoDB Storage | 20GB    | 100GB+ (SSD) |
| MongoDB CPU     | 2 cores | 4+ cores     |

### Monitoring

Key MongoDB metrics to monitor:

- `mongodb_connections_current` - Active connections
- `mongodb_opcounters_*` - Operation rates
- `mongodb_wiredtiger_cache_*` - Cache utilization
- `mongodb_repl_*` - Replication lag (if replica set)

---

## Limitations

Current limitations compared to upstream persistence backends:

| Feature             | Status             | Notes                      |
| ------------------- | ------------------ | -------------------------- |
| Archival            | ⏳ Not implemented | Planned for future release |
| Advanced Search     | ✅ Supported       | Via MongoDB aggregation    |
| Multi-cluster (XDC) | ⏳ Not tested      | May work, not validated    |

---

## Contributing

1. Fork [sagdelen/temporal](https://github.com/sagdelen/temporal)
2. Create a feature branch
3. Run tests: `make test`
4. Submit a pull request

---

## Support

- **Issues**: [GitHub Issues](https://github.com/sagdelen/temporal/issues)
- **Discussions**: [GitHub Discussions](https://github.com/sagdelen/temporal/discussions)

---

## License

Same as upstream Temporal - MIT License
