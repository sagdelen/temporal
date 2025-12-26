# Bug #14 Solution: Schedule Visibility Filtering

## Problem

ListWorkflowExecutions returns scheduler workflows when querying user workflows.

Test: `TestScheduleFunctionalSuite/TestBasics`

Error:

```
expected "sched-test-basics-wf-*" but got "temporal-sys-scheduler-workflow"
```

## Root Cause

MongoDB visibility store was not filtering system workflows from user queries.

SQL visibility store automatically adds `TemporalNamespaceDivision IS NULL` filter when no namespace division is specified in the query. MongoDB was missing this behavior:

1. **ConvertIsExpr returned empty filter**: When `ConvertIsExpr` was called for `TemporalNamespaceDivision IS NULL`, it returned `bson.M{}` (empty map) instead of an actual filter.

2. **convertQueryParams skipped empty queries**: When `request.Query` was empty, the function returned `nil, nil` without calling the query converter, so the automatic TemporalNamespaceDivision filter was never added.

## Solution

### 1. Fix ConvertIsExpr in visibility_query_converter.go

**Before:**

```go
func (c *mongoQueryConverter) ConvertIsExpr(operator string, col *query.SAColumn) (bson.M, error) {
    if col.FieldName == sadefs.TemporalNamespaceDivision {
        if operator == sqlparser.IsNullStr || operator == sqlparser.IsNotNullStr {
            return bson.M{}, nil  // ← Empty filter!
        }
        return nil, serviceerror.NewInvalidArgument(fmt.Sprintf("unsupported IS operator %q", operator))
    }
    // ...
}
```

**After:**

```go
func (c *mongoQueryConverter) ConvertIsExpr(operator string, col *query.SAColumn) (bson.M, error) {
    if col.FieldName == sadefs.TemporalNamespaceDivision {
        switch operator {
        case sqlparser.IsNullStr:
            return bson.M{sadefs.TemporalNamespaceDivision: bson.M{"$exists": false}}, nil
        case sqlparser.IsNotNullStr:
            return bson.M{sadefs.TemporalNamespaceDivision: bson.M{"$exists": true}}, nil
        default:
            return nil, serviceerror.NewInvalidArgument(fmt.Sprintf("unsupported IS operator %q", operator))
        }
    }
    // ...
}
```

### 2. Fix convertQueryParams in visibility_store.go

**Before:**

```go
func (s *visibilityStore) convertQueryParams(...) (*query.QueryParams[bson.M], error) {
    if queryString == "" {
        return nil, nil  // ← Skipped query conversion!
    }
    // ...
}
```

**After:**

```go
func (s *visibilityStore) convertQueryParams(...) (*query.QueryParams[bson.M], error) {
    // Always call converter - even for empty query
    // This ensures TemporalNamespaceDivision filter is applied
    // ...
}
```

### 3. Fix ListWorkflowExecutions in visibility_store.go

**Before:**

```go
var sort bson.D
if request.Query != "" {
    params, err := s.convertQueryParams(...)
    // ...
}
```

**After:**

```go
params, err := s.convertQueryParams(...)  // Always call, not conditional
// ...
```

### 4. Fix ListChasmExecutions in visibility_store.go (same pattern)

Removed the `if request.Query != ""` condition to always call the query converter.

## Files Modified

- [visibility_query_converter.go](../../common/persistence/mongodb/visibility_query_converter.go) - ConvertIsExpr now returns proper filter for TemporalNamespaceDivision
- [visibility_store.go](../../common/persistence/mongodb/visibility_store.go) - Always call convertQueryParams for namespace division filtering

## Verification

```bash
MONGODB_SEEDS=localhost:27017 PERSISTENCE_DRIVER=mongodb VISIBILITY_STORE_DRIVER=mongodb \
go test -tags test_dep,integration -v -run TestScheduleFunctionalSuite/TestBasics ./tests/ -count=1
```

Result: **PASS**

## Technical Details

The query converter (in `common/persistence/visibility/store/query/converter.go`) has built-in logic to track whether `TemporalNamespaceDivision` was explicitly queried. If not, it automatically adds:

- For regular workflow queries: `TemporalNamespaceDivision IS NULL` (filters out system workflows)
- For CHASM queries: `TemporalNamespaceDivision = archetypeID` (filters to specific archetype)

MongoDB's implementation was bypassing this automatic behavior by:

1. Returning empty filter for TemporalNamespaceDivision IS NULL
2. Skipping query conversion for empty queries

Both issues needed to be fixed to properly filter system workflows from user visibility queries.
