package tfexplorer

import (
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/ed25519"

	"github.com/threefoldtech/zos/pkg/identity"
	"github.com/threefoldtech/zos/pkg/versioned"
)

// Version History:
//   1.0.0: seed binary directly encoded
//   1.1.0: json with key mnemonic and threebot id

// TODO: remove once zos have exposed those variable
// https://github.com/threefoldtech/zos/blob/0ddc48e01b787893017095f71d5fd97efc42ef1a/pkg/identity/keys.go#L18
var (
	// SeedVersion1 (binary seed)
	seedVersion1 = versioned.MustParse("1.0.0")
	// SeedVersion11 (json mnemonic)
	seedVersion11 = versioned.MustParse("1.1.0")
	// SeedVersionLatest link to latest seed version
	seedVersionLatest = seedVersion11
)

// UserIdentity defines serializable struct to identify a user
type UserIdentity struct {
	// Mnemonic words of Private Key
	Mnemonic string `json:"mnemonic"`
	// ThreebotID generated by explorer
	ThreebotID uint64 `json:"threebotid"`
	// Internal keypair not exported
	key identity.KeyPair
}

// NewUserIdentity create a new UserIdentity from existing key
func NewUserIdentity(key identity.KeyPair, threebotid uint64) *UserIdentity {
	return &UserIdentity{
		key:        key,
		ThreebotID: threebotid,
	}
}

// Key returns the internal KeyPair
func (u *UserIdentity) Key() identity.KeyPair {
	return u.key
}

// Load fetch a seed file and initialize key based on mnemonic
func (u *UserIdentity) Load(path string) error {
	version, buf, err := versioned.ReadFile(path)
	if err != nil {
		return err
	}

	if version.Compare(seedVersion1) == 0 {
		return fmt.Errorf("seed file too old, please update it using 'tfuser id convert' command")
	}

	if version.NE(seedVersionLatest) {
		return fmt.Errorf("unsupported seed version")
	}

	err = json.Unmarshal(buf, &u)
	if err != nil {
		return err
	}

	return u.FromMnemonic(u.Mnemonic)
}

// FromMnemonic initialize the Key (KeyPair) from mnemonic argument
func (u *UserIdentity) FromMnemonic(mnemonic string) error {
	seed, err := bip39.EntropyFromMnemonic(mnemonic)
	if err != nil {
		return err
	}

	// Loading mnemonic
	u.key, err = identity.FromSeed(seed)
	if err != nil {
		return err
	}

	return nil
}

// Save dumps UserIdentity into a versioned file
func (u *UserIdentity) Save(path string) error {
	var err error

	log.Info().Msg("generating seed mnemonic")

	// Generate mnemonic of private key
	u.Mnemonic, err = bip39.NewMnemonic(u.key.PrivateKey.Seed())
	if err != nil {
		return err
	}

	// Versioning json output
	buf, err := json.Marshal(u)
	if err != nil {
		return err
	}

	// Saving json to file
	log.Info().Str("filename", path).Msg("writing user identity")
	versioned.WriteFile(path, seedVersion11, buf, 0400)

	return nil
}

// PrivateKey implements the client.Identity interface
func (u *UserIdentity) PrivateKey() ed25519.PrivateKey {
	return u.Key().PrivateKey
}

// Identity implements the Identifier interface
func (u *UserIdentity) Identity() string {
	return fmt.Sprintf("%d", u.ThreebotID)
}
