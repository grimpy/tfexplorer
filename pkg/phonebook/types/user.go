package types

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"

	"github.com/threefoldtech/tfexplorer/models"
	generated "github.com/threefoldtech/tfexplorer/models/generated/phonebook"
	"github.com/threefoldtech/tfexplorer/schema"
	"github.com/threefoldtech/zos/pkg/crypto"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	// UserCollection db collection name
	UserCollection = "user"
)

var (
	// ErrUserExists returned if user with same name exists
	ErrUserExists = errors.New("user with same name or email exists")
	// ErrUserNotFound is returned if user is not found
	ErrUserNotFound = errors.New("user not found")
	// ErrBadUserUpdate is returned when invalid data is passed to update
	ErrBadUserUpdate = errors.New("bad data during user update")
	// ErrAuthorization returned if user is not allowed to do an operation
	ErrAuthorization = errors.New("operation not allowed")
)

// User type
type User generated.User

// Validate makes the sanity check requires for the user type
func (u User) Validate() error {
	if strings.ToLower(u.Name) != u.Name {
		return fmt.Errorf("name should be all lower case")
	}
	if strings.ToLower(u.Email) != u.Email {
		return fmt.Errorf("email should be all lower case")
	}

	if len(u.Name) < 3 {
		return fmt.Errorf("name should be at least 3 character")
	}

	return nil
}

// Encode user data for signing
func (u *User) Encode() []byte {
	var buf bytes.Buffer
	// order is encoding is very important
	// from jumpscale, we see that the fields
	// are encoding like
	// id, name, email, ip-addr, description, pubkey
	buf.WriteString(fmt.Sprint(int64(u.ID)))
	buf.WriteString(u.Name)
	buf.WriteString(u.Email)
	buf.WriteString(u.Host)
	buf.WriteString(u.Description)
	buf.WriteString(u.Pubkey)

	return buf.Bytes()
}

// UserFilter type
type UserFilter bson.D

// WithID filters user with ID
func (f UserFilter) WithID(id schema.ID) UserFilter {
	if id == 0 {
		return f
	}
	return append(f, bson.E{Key: "_id", Value: id})
}

// WithName filters user with name
func (f UserFilter) WithName(name string) UserFilter {
	if name == "" {
		return f
	}
	return append(f, bson.E{Key: "name", Value: name})
}

// WithEmail filters user with email
func (f UserFilter) WithEmail(email string) UserFilter {
	if email == "" {
		return f
	}
	return append(f, bson.E{Key: "email", Value: email})
}

// Find all users that matches filter
func (f UserFilter) Find(ctx context.Context, db *mongo.Database, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	if f == nil {
		f = UserFilter{}
	}
	return db.Collection(UserCollection).Find(ctx, f, opts...)
}

// Count number of documents matching
func (f UserFilter) Count(ctx context.Context, db *mongo.Database) (int64, error) {
	col := db.Collection(UserCollection)
	if f == nil {
		f = UserFilter{}
	}

	return col.CountDocuments(ctx, f)
}

// Get single user
func (f UserFilter) Get(ctx context.Context, db *mongo.Database) (user User, err error) {
	if f == nil {
		f = UserFilter{}
	}

	result := db.Collection(UserCollection).FindOne(ctx, f, options.FindOne())
	if err = result.Err(); err != nil {
		return
	}

	err = result.Decode(&user)
	return
}

// UserCreate creates the user
func UserCreate(ctx context.Context, db *mongo.Database, name, email, pubkey string) (user User, err error) {
	if len(name) == 0 {
		return user, fmt.Errorf("invalid name, can't be empty")
	}

	if _, err := crypto.KeyFromHex(pubkey); err != nil {
		return user, errors.Wrapf(err, "invalid public key %s", pubkey)
	}

	var filter UserFilter
	filter = filter.WithName(name)
	_, err = filter.Get(ctx, db)

	if err == nil {
		return user, ErrUserExists
	} else if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return user, err
	}
	// else ErrNoDocuments

	id := models.MustID(ctx, db, UserCollection)
	user = User{
		ID:     id,
		Name:   name,
		Email:  email,
		Pubkey: pubkey,
	}

	col := db.Collection(UserCollection)
	_, err = col.InsertOne(ctx, user)
	if err != nil {
		if merr, ok := err.(mongo.WriteException); ok {
			errCode := merr.WriteErrors[0].Code
			if errCode == 11000 {
				return user, ErrUserExists
			}
		}
		return user, err
	}
	return
}

// UserUpdate update user info
func UserUpdate(ctx context.Context, db *mongo.Database, id schema.ID, signature []byte, update User) error {
	update.ID = id

	// then we find the user that matches this given ID
	var filter UserFilter
	filter = filter.WithID(id)

	current, err := filter.Get(ctx, db)
	if err != nil && errors.Is(err, mongo.ErrNoDocuments) {
		return ErrUserNotFound
	}

	// user need to always sign with current stored public key
	// even to update new key
	key, err := crypto.KeyFromHex(current.Pubkey)
	if err != nil {
		return err
	}

	// NOTE: verification here is done over the update request
	// data. We make sure that the signature is indeed done
	// with the priv key part of the user
	encoded := update.Encode()
	log.Debug().Str("encoded", string(encoded)).Msg("encoded message")
	if err := crypto.Verify(key, encoded, signature); err != nil {
		return errors.Wrap(ErrBadUserUpdate, "payload verification failed")
	}

	// if public key update is required, we make sure
	// that is valid key.
	if len(update.Pubkey) != 0 {
		_, err := crypto.KeyFromHex(update.Pubkey)
		if err != nil {
			return errors.Wrap(ErrBadUserUpdate, "invalid public key")
		}

		current.Pubkey = update.Pubkey
	}

	// sanity check make sure user is not trying to update his name
	if len(update.Name) != 0 && current.Name != update.Name {
		return errors.Wrap(ErrBadUserUpdate, "can not update name")
	}

	// copy all modified fields.
	if len(update.Email) != 0 {
		current.Email = update.Email
	}

	if len(update.Description) != 0 {
		current.Description = update.Description
	}

	if len(update.Host) != 0 {
		current.Host = update.Host
	}

	// actually update the user with final data
	if _, err := db.Collection(UserCollection).UpdateOne(ctx, filter, bson.M{"$set": current}); err != nil {
		return err
	}

	return nil
}
