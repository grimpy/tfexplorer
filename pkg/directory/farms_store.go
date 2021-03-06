package directory

import (
	"context"

	"github.com/pkg/errors"
	directory "github.com/threefoldtech/tfexplorer/pkg/directory/types"
	"github.com/threefoldtech/tfexplorer/schema"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// FarmAPI holds farm releated handlers
type FarmAPI struct{}

// List farms
// TODO: add paging arguments
func (s *FarmAPI) List(ctx context.Context, db *mongo.Database, filter directory.FarmFilter, opts ...*options.FindOptions) ([]directory.Farm, int64, error) {

	cur, err := filter.Find(ctx, db, opts...)
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to list farms")
	}
	defer cur.Close(ctx)
	out := []directory.Farm{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, errors.Wrap(err, "failed to load farm list")
	}

	count, err := filter.Count(ctx, db)
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to count entries in farms collection")
	}

	return out, count, nil
}

// GetByName gets a farm by name
func (s *FarmAPI) GetByName(ctx context.Context, db *mongo.Database, name string) (directory.Farm, error) {
	var filter directory.FarmFilter
	filter = filter.WithName(name)

	return filter.Get(ctx, db)
}

// GetByID gets a farm by ID
func (s *FarmAPI) GetByID(ctx context.Context, db *mongo.Database, id int64) (directory.Farm, error) {
	var filter directory.FarmFilter
	filter = filter.WithID(schema.ID(id))

	return filter.Get(ctx, db)
}

// Add add farm to store
func (s *FarmAPI) Add(ctx context.Context, db *mongo.Database, farm directory.Farm) (schema.ID, error) {
	return directory.FarmCreate(ctx, db, farm)
}

// Update farm information
func (s *FarmAPI) Update(ctx context.Context, db *mongo.Database, id schema.ID, farm directory.Farm) error {
	return directory.FarmUpdate(ctx, db, id, farm)
}

// Delete deletes a farm by ID
func (s FarmAPI) Delete(ctx context.Context, db *mongo.Database, id int64) error {
	var filter directory.FarmFilter
	filter = filter.WithID(schema.ID(id))
	return filter.Delete(ctx, db)
}
