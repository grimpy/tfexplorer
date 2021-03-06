package types

import (
	"context"
	"fmt"

	"github.com/threefoldtech/tfexplorer/models/generated/workloads"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Setup sets up indexes for types, must be called at least
// Onetime during the life time of the object
func Setup(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(ReservationCollection)
	indexes := []mongo.IndexModel{
		{
			Keys: bson.M{"data_reservation.networks.network_resources.node_id": 1},
		},
	}

	// Gather all supported workloads types to index them all
	wt := make([]string, 0, len(workloads.WorkloadTypes))
	for t := range workloads.WorkloadTypes {
		wt = append(wt, t.String())
	}

	for _, typ := range wt {
		indexes = append(
			indexes,
			mongo.IndexModel{
				Keys: bson.M{fmt.Sprintf("data_reservation.%s.node_id", typ): 1},
			},
		)

	}
	indexes = append(indexes, mongo.IndexModel{Keys: bson.M{"next_action": 1}})
	indexes = append(indexes, mongo.IndexModel{Keys: bson.M{"customer_tid": 1}})

	if _, err := col.Indexes().CreateMany(ctx, indexes); err != nil {
		return err
	}

	col = db.Collection(queueCollection)
	indexes = []mongo.IndexModel{
		{
			Keys: bson.M{"node_id": 1},
		},
		{
			Keys: bson.M{"workload_id": 1},
		},
	}

	if _, err := col.Indexes().CreateMany(ctx, indexes); err != nil {
		return err
	}

	return nil
}
