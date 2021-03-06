package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/threefoldtech/tfexplorer/mw"
	"github.com/threefoldtech/zos/pkg/app"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	directory "github.com/threefoldtech/tfexplorer/pkg/directory/types"
	phonebook "github.com/threefoldtech/tfexplorer/pkg/phonebook/types"
	workloads "github.com/threefoldtech/tfexplorer/pkg/workloads/types"
)

func foreach(root string, f func(p string, r io.Reader) error) error {
	files, err := ioutil.ReadDir(root)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		if filepath.Ext(file.Name()) != ".json" {
			continue
		}

		p := filepath.Join(root, file.Name())
		fd, err := os.Open(p)
		if err != nil {
			return err
		}

		if err := f(p, fd); err != nil {
			fd.Close()
			return err
		}

		fd.Close()
	}

	return nil
}

// Migrator callback
type Migrator func(root string, db *mongo.Database) error

func migrateFarms(root string, db *mongo.Database) error {
	col := db.Collection(directory.FarmCollection)
	return foreach(root, func(p string, r io.Reader) error {
		var farm directory.Farm
		if err := json.NewDecoder(r).Decode(&farm); err != nil {
			return errors.Wrapf(err, "failed to load file '%s'", p)
		}

		_, err := col.InsertOne(context.TODO(), farm)
		if err != nil {
			log.Error().Err(err).Msgf("failed to insert farm '%s'", p)
		}

		return nil
	})
}

func migrateNodes(root string, db *mongo.Database) error {
	col := db.Collection(directory.NodeCollection)
	return foreach(root, func(p string, r io.Reader) error {
		var node directory.Node
		if err := json.NewDecoder(r).Decode(&node); err != nil {
			return errors.Wrapf(err, "failed to load file '%s'", p)
		}

		if err := node.Validate(); err != nil {
			return errors.Wrapf(err, "file '%s'", p)
		}

		_, err := col.InsertOne(context.TODO(), node)
		if err != nil {
			log.Error().Err(err).Msgf("failed to insert node '%s'", p)
		}

		return nil
	})
}

func migrateUsers(root string, db *mongo.Database) error {
	col := db.Collection(phonebook.UserCollection)
	return foreach(root, func(p string, r io.Reader) error {
		var user phonebook.User
		if err := json.NewDecoder(r).Decode(&user); err != nil {
			return errors.Wrapf(err, "failed to load file '%s'", p)
		}

		_, err := col.InsertOne(context.TODO(), user)
		if err != nil {
			log.Error().Err(err).Msgf("failed to insert user '%s'", p)
		}

		return nil
	})
}

func migrateReservations(root string, db *mongo.Database) error {
	col := db.Collection(workloads.ReservationCollection)
	return foreach(root, func(p string, r io.Reader) error {
		var reservation workloads.Reservation
		if err := json.NewDecoder(r).Decode(&reservation); err != nil {
			return errors.Wrapf(err, "failed to load file '%s'", p)
		}

		_, err := col.InsertOne(context.TODO(), reservation)
		if err != nil {
			log.Error().Err(err).Msgf("failed to insert reservation '%s'", p)
		}

		return nil
	})
}

func connectDB(ctx context.Context, connectionURI string) (*mongo.Client, error) {
	client, err := mongo.NewClient(options.Client().ApplyURI(connectionURI))
	if err != nil {
		return nil, err
	}

	if err := client.Connect(ctx); err != nil {
		return nil, err
	}

	return client, nil
}

func main() {
	app.Initialize()

	var (
		root   string
		dbConf string
		name   string
	)

	flag.StringVar(&dbConf, "mongo", "mongodb://localhost:27017", "connection string to mongo database")
	flag.StringVar(&name, "name", "explorer", "database name")
	flag.StringVar(&root, "root", "", "root directory of the bcdb exported data")

	flag.Parse()
	if strings.EqualFold(root, "") {
		log.Fatal().Msg("root option is required")
	}

	ctx := context.TODO()

	db, err := connectDB(ctx, dbConf)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}

	dbMw, err := mw.NewDatabaseMiddleware(name, db)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}

	if err := directory.Setup(ctx, dbMw.Database()); err != nil {
		log.Fatal().Err(err).Msg("failed to setup directory indexes")
	}

	types := map[string]Migrator{
		"tfgrid_directory/tfgrid.directory.farm.1/yaml":        migrateFarms,
		"tfgrid_directory/tfgrid.directory.node.2/yaml":        migrateNodes,
		"phonebook/tfgrid.phonebook.user.1/yaml":               migrateUsers,
		"tfgrid_workloads/tfgrid.workloads.reservation.1/yaml": migrateReservations,
	}

	for typ, migrator := range types {
		if err := migrator(filepath.Join(root, typ), dbMw.Database()); err != nil {
			log.Error().Err(err).Str("root", typ).Msg("migration failed")
		}
	}
}
