package mongodb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/temporalio/sqlparser"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/common/persistence/visibility/store/query"
	"go.temporal.io/server/common/searchattribute"
)

func TestMongoQueryConverterKeywordStartsWith(t *testing.T) {
	converter := newMongoQueryConverter()
	col := query.NewSAColumn(searchattribute.WorkflowType, searchattribute.WorkflowType, enumspb.INDEXED_VALUE_TYPE_KEYWORD)

	filter, err := converter.ConvertKeywordComparisonExpr(sqlparser.StartsWithStr, col, "Workflow")
	require.NoError(t, err)

	regex, ok := filter["workflow_type_name"].(primitive.Regex)
	require.True(t, ok)
	require.Equal(t, "^Workflow", regex.Pattern)
}

func TestMongoQueryConverterKeywordListNotStartsWith(t *testing.T) {
	converter := newMongoQueryConverter()
	col := query.NewSAColumn(searchattribute.TemporalPauseInfo, searchattribute.TemporalPauseInfo, enumspb.INDEXED_VALUE_TYPE_KEYWORD_LIST)

	filter, err := converter.ConvertKeywordListComparisonExpr(sqlparser.NotStartsWithStr, col, "property:activityType=")
	require.NoError(t, err)

	clause, ok := filter["search_attributes."+searchattribute.TemporalPauseInfo].(bson.M)
	require.True(t, ok)

	regex, ok := clause["$not"].(primitive.Regex)
	require.True(t, ok)
	require.Equal(t, "^property:activityType=", regex.Pattern)
}
