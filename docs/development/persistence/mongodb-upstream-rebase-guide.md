# MongoDB Plugin — Upstream Rebase Guide

> Procedure for rebasing the MongoDB persistence plugin onto a new upstream
> Temporal Server nightly tag.

## Table of Contents

- [MongoDB Plugin — Upstream Rebase Guide](#mongodb-plugin--upstream-rebase-guide)
  - [Table of Contents](#table-of-contents)
  - [Background](#background)
  - [Prerequisites](#prerequisites)
  - [Naming Conventions](#naming-conventions)
  - [Procedure](#procedure)
    - [1. Identify Target Tag](#1-identify-target-tag)
    - [2. Create Branch](#2-create-branch)
    - [3. Transfer MongoDB Commits](#3-transfer-mongodb-commits)
    - [4. Resolve go.mod Conflicts](#4-resolve-gomod-conflicts)
    - [5. Verification](#5-verification)
      - [5a. Lint](#5a-lint)
      - [5b. Unit Tests](#5b-unit-tests)
      - [5c. Functional Tests (MongoDB)](#5c-functional-tests-mongodb)
      - [5d. Integration Tests (MongoDB) — optional](#5d-integration-tests-mongodb--optional)
    - [6. Publish](#6-publish)
  - [Known Pitfalls](#known-pitfalls)
  - [AI Agent Prompt Template](#ai-agent-prompt-template)
  - [Version History](#version-history)

---

## Background

The MongoDB persistence plugin lives as a fork of the upstream Temporal Server.
Because the upstream `main` branch changes constantly — with potential breaking changes
between nightly tags — the plugin is maintained by **rebasing onto specific nightly tags**
rather than merging from `main`.

This approach was adopted after encountering the following problems:

- Merging from `main` pulled in upstream's own broken/flaky tests unrelated to MongoDB
- Resolving `go.mod` conflicts would accidentally downgrade critical dependencies like
  `aws-sdk-go` (v1.55.8 → v1.43.21), breaking S3 mocks
- It became increasingly difficult to distinguish MongoDB-specific changes from upstream
  merge artifacts

**Solution:** MongoDB changes are maintained as a single squash commit and cleanly
cherry-picked onto each new upstream tag.

## Prerequisites

- Git remotes configured:
  ```
  origin    git@github.com:sagdelen/temporal.git       (fork)
  upstream  org-56493103@github.com:temporalio/temporal.git  (upstream)
  ```
- Go toolchain installed (compatible with the go directive in go.mod, currently `go 1.25.0`)
- `make` commands functional
- MongoDB test infrastructure (Docker via `make start-dependencies`)

## Naming Conventions

| Item              | Pattern                                    | Example                                       |
| ----------------- | ------------------------------------------ | --------------------------------------------- |
| **Dev branch**    | `mongo/{upstream-tag}-dev`                 | `mongo/v1.31.0-151.5-dev`                     |
| **Stable branch** | `mongo/{upstream-tag}`                     | `mongo/v1.31.0-151.5`                         |
| **Release tag**   | `{upstream-tag}-mongo.{month}-{timestamp}` | `v1.31.0-151.5-mongo.3-20260315T140000Z`      |
| **Fast tag**      | `{release-tag}-fast`                       | `v1.31.0-151.5-mongo.3-20260315T140000Z-fast` |

Notes:

- The number after `mongo.` is the **calendar month** of the release (e.g., `mongo.3` = March).
- The `-dev` branch is for active development; it gets promoted to the stable branch once tests pass.
- Tags with the `-fast` suffix indicate CI builds that only ran MongoDB-specific tests.
- Upstream tags may include **major/minor version bumps** (e.g., `v1.30.0` → `v1.31.0`). The procedure
  handles this transparently — the branch/tag naming always mirrors the upstream tag verbatim.

## Procedure

### 1. Identify Target Tag

```bash
# Fetch all upstream tags
git fetch upstream --tags

# List recent nightly tags — adjust the major.minor prefix as needed
# For v1.30.x:
git tag -l "v1.30*" | sort -V | tail -10
# For v1.31.x:
git tag -l "v1.31*" | sort -V | tail -10
# Or list all recent tags regardless of major/minor:
git tag -l "v1.*" | sort -V | tail -20
```

Choose the target tag (e.g., `v1.31.0-151.5`). This will become the new base for the MongoDB branch.

> **Note:** When the upstream bumps the minor version (e.g., `v1.30.0` → `v1.31.0`),
> expect more conflicts than a patch-level nightly bump. The safe go.mod resolution
> method in Step 4 remains the same regardless.

### 2. Create Branch

```bash
# Record current state
CURRENT_BRANCH=$(git branch --show-current)
CURRENT_TAG=$(git describe --tags --abbrev=0)

# Create a new dev branch from the target tag
TARGET_TAG="v1.31.0-151.5"  # ← change this
git checkout -b "mongo/${TARGET_TAG}-dev" "${TARGET_TAG}"
```

### 3. Transfer MongoDB Commits

Use the squash commit from the current stable branch (or the last known good commit)
as the source.

```bash
# Find the base MongoDB commit on the current branch
# (typically the commit with message "Add MongoDB persistence plugin")
SOURCE_BRANCH="mongo/v1.30.0-148.3"  # ← previous stable branch (update each time)
MONGO_BASE=$(git log --oneline "${SOURCE_BRANCH}" --grep="Add MongoDB persistence plugin" --format=%H | head -1)

# Cherry-pick
git cherry-pick "${MONGO_BASE}"
```

**If no conflicts arise:** Cherry-pick subsequent bug-fix commits in order:

```bash
# List all MongoDB commits on the source branch
git log --oneline "${SOURCE_BRANCH}" --not "${TARGET_TAG}" | tac

# Cherry-pick sequentially (base commit already applied)
git cherry-pick <commit-2>
git cherry-pick <commit-3>
# ...
```

**If conflicts arise:** Proceed to the next section.

### 4. Resolve go.mod Conflicts

> ⚠️ **CRITICAL:** This is the most error-prone step. During manual conflict resolution,
> dependency versions can be accidentally downgraded.

**Safe method — reset go.mod from base, add only MongoDB:**

```bash
# Restore go.mod and go.sum from the target tag (clean base)
git checkout "${TARGET_TAG}" -- go.mod go.sum

# Add the MongoDB driver
go mod edit -require=go.mongodb.org/mongo-driver@v1.17.6

# Resolve transitive dependencies
go mod tidy

# Verify — only MongoDB and its transitive deps should be added
git diff "${TARGET_TAG}" -- go.mod
```

The expected diff should contain only:

- `go.mongodb.org/mongo-driver` (direct)
- `github.com/montanaflynn/stats` (indirect)
- `github.com/xdg-go/pbkdf2` (indirect)
- `github.com/xdg-go/scram` (indirect)
- `github.com/xdg-go/stringprep` (indirect)
- `github.com/youmark/pkcs8` (indirect)

If there are other changes, **stop and investigate**.

```bash
# Stage the resolved go.mod and complete the cherry-pick
git add go.mod go.sum
git cherry-pick --continue
```

### 5. Verification

Three levels of verification are applied:

#### 5a. Lint

```bash
make lint-code
# Expected: 0 issues
```

#### 5b. Unit Tests

```bash
TEST_PARALLEL_FLAGS="-parallel 1" \
TEST_SHUFFLE_FLAG=off \
TEST_RACE_FLAG=off \
make unit-test-coverage
# Expected: PASS
```

> Note: On the first run, tests may be killed due to OOM; the gotestsum retry mechanism
> will automatically re-run them.

#### 5c. Functional Tests (MongoDB)

```bash
# Start MongoDB dependencies
PERSISTENCE_DRIVER=mongodb make start-dependencies

# Run tests
PERSISTENCE_DRIVER=mongodb \
VISIBILITY_STORE_DRIVER=mongodb \
make functional-test-coverage
# Expected: PASS
```

#### 5d. Integration Tests (MongoDB) — optional

```bash
PERSISTENCE_DRIVER=mongodb \
make integration-test-coverage
```

### 6. Publish

```bash
# Squash commits (if multiple cherry-picks were applied)
git reset --soft "${TARGET_TAG}"
git commit -m "Add MongoDB persistence plugin"

# Push
git push -f origin "mongo/${TARGET_TAG}-dev"
```

Once all tests pass, promote to the stable branch:

```bash
git branch -f "mongo/${TARGET_TAG}" "mongo/${TARGET_TAG}-dev"
git push origin "mongo/${TARGET_TAG}"
```

---

## Known Pitfalls

| #   | Pitfall                      | Symptom                                 | Resolution                                                                 |
| --- | ---------------------------- | --------------------------------------- | -------------------------------------------------------------------------- |
| 1   | **go.mod conflict markers**  | `invalid char '<'` error                | Checkout go.mod from base tag, add only the mongo driver                   |
| 2   | **aws-sdk-go downgrade**     | `CreateSessionInput` undefined          | Verify go.mod diff; base tag version must be preserved                     |
| 3   | **Duplicate require blocks** | `unknown directive`                     | Reset go.mod from base; `go mod tidy` alone cannot fix structural issues   |
| 4   | **CHASM test conflict**      | `shardContext` field missing            | Upstream CHASM refactors change test structs; use the base tag's test code |
| 5   | **Empty cherry-pick**        | `The previous cherry-pick is now empty` | `git cherry-pick --skip` — the commit already exists in the base           |
| 6   | **Flaky upstream tests**     | Unit test failure (non-MongoDB)         | Avoid by basing on a stable tag rather than `main`                         |

---

## AI Agent Prompt Template

The following prompt can be given to an AI agent (GitHub Copilot, etc.) for transitioning
to a new upstream tag. Replace `{PLACEHOLDER}` values.

````
## Task: MongoDB Plugin Upstream Rebase

### Context
- Repository: temporalio/temporal (fork: sagdelen/temporal)
- Remotes: origin = sagdelen/temporal, upstream = temporalio/temporal
- Current MongoDB dev branch: `mongo/{CURRENT_TAG}-dev` (e.g., `mongo/v1.30.0-148.3-dev`)
- Target upstream tag: `{TARGET_TAG}` (e.g., `v1.31.0-151.5`)
- MongoDB Go Driver version: `go.mongodb.org/mongo-driver@v1.17.6`
- MongoDB tag month suffix: `mongo.{MONTH}` (e.g., `mongo.3` for March)

### Instructions

1. **Fetch and create branch:**
   ```
   git fetch upstream --tags
   git checkout -b mongo/{TARGET_TAG}-dev {TARGET_TAG}
   ```

2. **Cherry-pick MongoDB commits:**
   Source branch: `mongo/{CURRENT_TAG}-dev`
   Find the base MongoDB commit via `git log` (message: "Add MongoDB persistence plugin").
   Cherry-pick subsequent fix commits in order.

3. **If go.mod conflicts arise, use THE ONLY SAFE METHOD:**
   ```
   git checkout {TARGET_TAG} -- go.mod go.sum
   go mod edit -require=go.mongodb.org/mongo-driver@v1.17.6
   go mod tidy
   git add go.mod go.sum
   git cherry-pick --continue
   ```
   IMPORTANT: Verify with `git diff {TARGET_TAG} -- go.mod`.
   Only mongo-driver and 5 indirect deps (stats, pbkdf2, scram, stringprep, pkcs8) should be added.

4. **Other conflicts:** Prefer the upstream tag's code, then apply MongoDB
   changes on top. Never use the old MongoDB branch's upstream code.

5. **Verification sequence:**
   ```
   make lint-code
   TEST_PARALLEL_FLAGS="-parallel 1" TEST_SHUFFLE_FLAG=off TEST_RACE_FLAG=off make unit-test-coverage
   PERSISTENCE_DRIVER=mongodb VISIBILITY_STORE_DRIVER=mongodb make functional-test-coverage
   ```
   All three must pass.

6. **Squash and push:**
   ```
   git reset --soft {TARGET_TAG}
   git commit -m "Add MongoDB persistence plugin"
   git push -f origin mongo/{TARGET_TAG}-dev
   ```

### Important Notes
- The `aws-sdk-go` version in go.mod must match the base tag; NEVER downgrade it
- If cherry-pick is empty, skip with `--skip` (commit already exists in base)
- For CHASM test conflicts, use the base tag's test code, not the old branch's
- If unit tests OOM, the gotestsum retry mechanism will handle it
````

---

## Version History

| Date       | Base Tag      | Branch                  | Notes                                                |
| ---------- | ------------- | ----------------------- | ---------------------------------------------------- |
| 2026-01-16 | v1.30.0-148.2 | feature/mongodb-plugin  | First rebase; learned go.mod conflict resolution     |
| 2026-01-19 | v1.30.0-148.3 | mongo/v1.30.0-148.3-dev | Second rebase; established naming conventions        |
| 2026-03-xx | v1.31.0-151.5 | mongo/v1.31.0-151.5-dev | Third rebase; first cross-minor bump (v1.30 → v1.31) |
