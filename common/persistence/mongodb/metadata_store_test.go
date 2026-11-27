package mongodb_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/mongodb"
	"go.temporal.io/server/common/primitives"
)

type MetadataStoreSuite struct {
	suite.Suite
	factory *mongodb.Factory
	store   persistence.MetadataStore
	ctx     context.Context
	cancel  context.CancelFunc
	dbName  string
}

func TestMetadataStoreSuite(t *testing.T) {
	suite.Run(t, new(MetadataStoreSuite))
}

func (s *MetadataStoreSuite) SetupSuite() {
	s.dbName = fmt.Sprintf("temporal_test_metadata_%d", time.Now().UnixNano())
	cfg := newMongoTestConfig(s.dbName)
	cfg.ConnectTimeout = 10 * time.Second

	logger := log.NewTestLogger()
	var err error
	s.factory, err = mongodb.NewFactory(cfg, "test-cluster", logger, metrics.NoopMetricsHandler)
	if err != nil {
		s.T().Skipf("Skipping MetadataStoreSuite: %v", err)
	}
	s.Require().NoError(err)

	s.store, err = s.factory.NewMetadataStore()
	s.Require().NoError(err)
}

func (s *MetadataStoreSuite) TearDownSuite() {
	if s.factory != nil {
		s.factory.Close()
	}
}

func (s *MetadataStoreSuite) SetupTest() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 30*time.Second)
}

func (s *MetadataStoreSuite) TearDownTest() {
	s.cancel()
}

func (s *MetadataStoreSuite) TestCreateAndGetNamespace() {
	id := primitives.NewUUID().String()
	name := "test-namespace-create"
	data := &commonpb.DataBlob{Data: []byte("namespace-data"), EncodingType: 1}

	resp, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
		IsGlobal:  true,
	})
	s.Require().NoError(err)
	s.Require().Equal(id, resp.ID)

	byID, err := s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{ID: id})
	s.Require().NoError(err)
	s.Require().Equal(data.Data, byID.Namespace.Data)
	s.Require().True(byID.IsGlobal)
	s.Require().EqualValues(0, byID.NotificationVersion)

	byName, err := s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{Name: name})
	s.Require().NoError(err)
	s.Require().Equal(byID.Namespace.Data, byName.Namespace.Data)
	s.Require().Equal(byID.NotificationVersion, byName.NotificationVersion)

	metadata, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)
	s.Require().EqualValues(1, metadata.NotificationVersion)
}

func (s *MetadataStoreSuite) TestCreateNamespaceDuplicateName() {
	id := primitives.NewUUID().String()
	name := "duplicate-namespace"
	data := &commonpb.DataBlob{Data: []byte("data"), EncodingType: 1}

	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
	})
	s.Require().NoError(err)

	_, err = s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        primitives.NewUUID().String(),
		Name:      name,
		Namespace: data,
	})
	s.Require().Error(err)
	_, ok := err.(*serviceerror.NamespaceAlreadyExists)
	s.Require().True(ok)
}

// Concurrent creates must all succeed, as in SQL where they serialize on the
// metadata row, and every namespace must carry a distinct version below the
// final metadata version.
func (s *MetadataStoreSuite) TestCreateNamespaceConcurrent() {
	const n = 30
	before, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)

	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ids[i] = primitives.NewUUID().String()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
				ID:        ids[i],
				Name:      fmt.Sprintf("test-namespace-concurrent-%d-%s", i, ids[i]),
				Namespace: &commonpb.DataBlob{Data: []byte("namespace-data"), EncodingType: 1},
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		s.Require().NoErrorf(err, "create %d", i)
	}

	after, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(before.NotificationVersion+n, after.NotificationVersion)

	seen := map[int64]bool{}
	for _, id := range ids {
		resp, err := s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{ID: id})
		s.Require().NoError(err)
		s.Require().GreaterOrEqual(resp.NotificationVersion, before.NotificationVersion)
		s.Require().Less(resp.NotificationVersion, after.NotificationVersion)
		s.Require().False(seen[resp.NotificationVersion], "version %d used twice", resp.NotificationVersion)
		seen[resp.NotificationVersion] = true
	}
}

// A create that fails on a duplicate name rolls back with its metadata bump:
// the version stays put and the second ID is not stored.
func (s *MetadataStoreSuite) TestCreateNamespaceDuplicateNameIsAtomic() {
	name := "test-namespace-dup-" + primitives.NewUUID().String()
	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        primitives.NewUUID().String(),
		Name:      name,
		Namespace: &commonpb.DataBlob{Data: []byte("namespace-data"), EncodingType: 1},
	})
	s.Require().NoError(err)

	before, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)

	secondID := primitives.NewUUID().String()
	_, err = s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        secondID,
		Name:      name,
		Namespace: &commonpb.DataBlob{Data: []byte("namespace-data"), EncodingType: 1},
	})
	var alreadyExists *serviceerror.NamespaceAlreadyExists
	s.Require().ErrorAs(err, &alreadyExists)

	after, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(before.NotificationVersion, after.NotificationVersion)

	_, err = s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{ID: secondID})
	var notFound *serviceerror.NamespaceNotFound
	s.Require().ErrorAs(err, &notFound)
}

func (s *MetadataStoreSuite) TestUpdateNamespace() {
	id := primitives.NewUUID().String()
	name := "update-namespace"
	data := &commonpb.DataBlob{Data: []byte("initial"), EncodingType: 1}

	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
	})
	s.Require().NoError(err)

	metadata, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)

	updateData := &commonpb.DataBlob{Data: []byte("updated"), EncodingType: 1}
	err = s.store.UpdateNamespace(s.ctx, &persistence.InternalUpdateNamespaceRequest{
		Id:                  id,
		Name:                name,
		Namespace:           updateData,
		NotificationVersion: metadata.NotificationVersion,
	})
	s.Require().NoError(err)

	ns, err := s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{ID: id})
	s.Require().NoError(err)
	s.Require().Equal(updateData.Data, ns.Namespace.Data)

	metadataAfter, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(metadata.NotificationVersion+1, metadataAfter.NotificationVersion)
}

func (s *MetadataStoreSuite) TestUpdateNamespaceVersionMismatch() {
	id := primitives.NewUUID().String()
	name := "update-mismatch"
	data := &commonpb.DataBlob{Data: []byte("value"), EncodingType: 1}

	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
	})
	s.Require().NoError(err)

	err = s.store.UpdateNamespace(s.ctx, &persistence.InternalUpdateNamespaceRequest{
		Id:                  id,
		Name:                name,
		Namespace:           data,
		NotificationVersion: 0,
	})
	s.Require().Error(err)
}

func (s *MetadataStoreSuite) TestRenameNamespace() {
	id := primitives.NewUUID().String()
	name := "rename-before"
	data := &commonpb.DataBlob{Data: []byte("value"), EncodingType: 1}

	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
	})
	s.Require().NoError(err)

	metadata, err := s.store.GetMetadata(s.ctx)
	s.Require().NoError(err)

	newName := "rename-after"
	err = s.store.RenameNamespace(s.ctx, &persistence.InternalRenameNamespaceRequest{
		InternalUpdateNamespaceRequest: &persistence.InternalUpdateNamespaceRequest{
			Id:                  id,
			Name:                newName,
			Namespace:           data,
			NotificationVersion: metadata.NotificationVersion,
		},
		PreviousName: name,
	})
	s.Require().NoError(err)

	_, err = s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{Name: name})
	s.Require().Error(err)

	updated, err := s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{Name: newName})
	s.Require().NoError(err)
	s.Require().Equal(data.Data, updated.Namespace.Data)
}

func (s *MetadataStoreSuite) TestListNamespacesPagination() {
	const total = 5
	data := &commonpb.DataBlob{Data: []byte("page"), EncodingType: 1}

	for i := 0; i < total; i++ {
		name := fmt.Sprintf("list-namespace-%d", i)
		_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
			ID:        primitives.NewUUID().String(),
			Name:      name,
			Namespace: data,
		})
		s.Require().NoError(err)
	}

	resp, err := s.store.ListNamespaces(s.ctx, &persistence.InternalListNamespacesRequest{PageSize: 2})
	s.Require().NoError(err)
	s.Require().LessOrEqual(len(resp.Namespaces), 2)

	if len(resp.NextPageToken) > 0 {
		next, err := s.store.ListNamespaces(s.ctx, &persistence.InternalListNamespacesRequest{
			PageSize:      2,
			NextPageToken: resp.NextPageToken,
		})
		s.Require().NoError(err)
		s.Require().NotEmpty(next.Namespaces)
	}
}

func (s *MetadataStoreSuite) TestDeleteNamespace() {
	id := primitives.NewUUID().String()
	name := "delete-namespace"
	data := &commonpb.DataBlob{Data: []byte("delete"), EncodingType: 1}

	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
	})
	s.Require().NoError(err)

	s.Require().NoError(s.store.DeleteNamespace(s.ctx, &persistence.DeleteNamespaceRequest{ID: id}))
	_, err = s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{ID: id})
	s.Require().Error(err)
}

func (s *MetadataStoreSuite) TestDeleteNamespaceByName() {
	id := primitives.NewUUID().String()
	name := "delete-by-name"
	data := &commonpb.DataBlob{Data: []byte("delete"), EncodingType: 1}

	_, err := s.store.CreateNamespace(s.ctx, &persistence.InternalCreateNamespaceRequest{
		ID:        id,
		Name:      name,
		Namespace: data,
	})
	s.Require().NoError(err)

	s.Require().NoError(s.store.DeleteNamespaceByName(s.ctx, &persistence.DeleteNamespaceByNameRequest{Name: name}))
	_, err = s.store.GetNamespace(s.ctx, &persistence.GetNamespaceRequest{Name: name})
	s.Require().Error(err)
}
