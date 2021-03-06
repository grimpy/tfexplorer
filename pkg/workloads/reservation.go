package workloads

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/threefoldtech/tfexplorer/config"
	"github.com/threefoldtech/tfexplorer/models"
	generated "github.com/threefoldtech/tfexplorer/models/generated/workloads"
	"github.com/threefoldtech/tfexplorer/mw"
	directory "github.com/threefoldtech/tfexplorer/pkg/directory/types"
	"github.com/threefoldtech/tfexplorer/pkg/escrow"
	escrowtypes "github.com/threefoldtech/tfexplorer/pkg/escrow/types"
	phonebook "github.com/threefoldtech/tfexplorer/pkg/phonebook/types"
	"github.com/threefoldtech/tfexplorer/pkg/stellar"
	"github.com/threefoldtech/tfexplorer/pkg/workloads/types"
	"github.com/threefoldtech/tfexplorer/schema"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type (
	// API struct
	API struct {
		escrow escrow.Escrow
	}

	// ReservationCreateResponse wraps reservation create response
	ReservationCreateResponse struct {
		ID                schema.ID                             `json:"reservation_id"`
		EscrowInformation escrowtypes.CustomerEscrowInformation `json:"escrow_information"`
	}
)

// freeTFT currency code
const freeTFT = "FreeTFT"

func (a *API) validAddresses(ctx context.Context, db *mongo.Database, res *types.Reservation) error {
	if config.Config.Network == "" {
		log.Info().Msg("escrow disabled, no validation of farmer wallet address needed")
		return nil
	}

	workloads := res.Workloads("")
	var nodes []string

	for _, wl := range workloads {
		nodes = append(nodes, wl.NodeID)
	}

	farms, err := directory.FarmsForNodes(ctx, db, nodes...)
	if err != nil {
		return err
	}

	for _, farm := range farms {
		for _, a := range farm.WalletAddresses {
			validator, err := stellar.NewAddressValidator(config.Config.Network, a.Asset)
			if err != nil {
				if errors.Is(err, stellar.ErrAssetCodeNotSupported) {
					continue
				}
				return errors.Wrap(err, "could not initialize address validator")
			}
			if err := validator.Valid(a.Address); err != nil {
				return fmt.Errorf("farm %s has an invalid address for currency %s: %w", farm.Name, a.Asset, err)
			}
		}

	}

	return nil
}

func (a *API) create(r *http.Request) (interface{}, mw.Response) {
	defer r.Body.Close()
	var reservation types.Reservation
	if err := json.NewDecoder(r.Body).Decode(&reservation); err != nil {
		return nil, mw.BadRequest(err)
	}

	if reservation.Expired() {
		return nil, mw.BadRequest(fmt.Errorf("creating for a reservation that expires in the past"))
	}

	duration := time.Until(reservation.DataReservation.ExpirationReservation.Time)
	if duration < time.Hour {
		return nil, mw.BadRequest(fmt.Errorf("the minimum duration for a reservation is 1 hour, you tried to reserve for %s", duration.String()))
	}

	// we make sure those arrays are initialized correctly
	// this will make updating the document in place much easier
	// in later stages
	reservation.SignaturesProvision = make([]generated.SigningSignature, 0)
	reservation.SignaturesDelete = make([]generated.SigningSignature, 0)
	reservation.SignaturesFarmer = make([]generated.SigningSignature, 0)
	reservation.Results = make([]generated.Result, 0)

	if err := reservation.Validate(); err != nil {
		return nil, mw.BadRequest(err)
	}

	reservation, err := a.pipeline(reservation, nil)
	if err != nil {
		// if failed to create pipeline, then
		// this reservation has failed initial validation
		return nil, mw.BadRequest(err)
	}

	if reservation.IsAny(types.Invalid, types.Delete) {
		return nil, mw.BadRequest(fmt.Errorf("invalid request wrong status '%s'", reservation.NextAction.String()))
	}

	db := mw.Database(r)

	if err := a.validAddresses(r.Context(), db, &reservation); err != nil {
		return nil, mw.Error(err, http.StatusFailedDependency) //FIXME: what is this strange status ?
	}

	// check if freeTFT is allowed to be used
	// if all nodes are marked as free to use then FreeTFT is allowed
	// otherwise it is not

	var freeNodes int

	usedNodes := reservation.NodeIDs()
	count, err := (directory.NodeFilter{}).
		WithNodeIDs(usedNodes).
		WithFreeToUse(true).
		Count(r.Context(), db)
	if err != nil {
		return nil, mw.Error(err, http.StatusInternalServerError)
	}
	freeNodes += int(count)

	usedGateways := reservation.GatewayIDs()
	count, err = (directory.GatewayFilter{}).
		WithGWIDs(usedGateways).
		WithFreeToUse(true).
		Count(r.Context(), db)
	if err != nil {
		return nil, mw.Error(err, http.StatusInternalServerError)
	}
	freeNodes += int(count)

	paidNodes := len(usedNodes) + len(usedGateways)

	log.Info().
		Int64("reservation_id", int64(reservation.ID)).
		Int("paid_nodes", paidNodes).
		Int("free_nodes", freeNodes).
		Msg("distribution of free nodes")

	currencies := make([]string, len(reservation.DataReservation.Currencies))
	copy(currencies, reservation.DataReservation.Currencies)

	// filter out FreeTFT if not all the nodes can be paid with freeTFT
	if freeNodes < paidNodes {
		for i, c := range currencies {
			if c == freeTFT {
				currencies = append(currencies[:i], currencies[i+1:]...)
			}
		}
	}

	var filter phonebook.UserFilter
	filter = filter.WithID(schema.ID(reservation.CustomerTid))
	user, err := filter.Get(r.Context(), db)
	if err != nil {
		return nil, mw.BadRequest(errors.Wrapf(err, "cannot find user with id '%d'", reservation.CustomerTid))
	}

	signature, err := hex.DecodeString(reservation.CustomerSignature)
	if err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "invalid signature format, expecting hex encoded string"))
	}

	if err := reservation.Verify(user.Pubkey, signature); err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "failed to verify customer signature"))
	}

	reservation.Epoch = schema.Date{Time: time.Now()}

	id, err := types.ReservationCreate(r.Context(), db, reservation)
	if err != nil {
		return nil, mw.Error(err)
	}

	reservation, err = types.ReservationFilter{}.WithID(id).Get(r.Context(), db)
	if err != nil {
		return nil, mw.Error(err)
	}

	escrowDetails, err := a.escrow.RegisterReservation(generated.Reservation(reservation), currencies)
	if err != nil {
		return nil, mw.Error(err)
	}

	return ReservationCreateResponse{
		ID:                reservation.ID,
		EscrowInformation: escrowDetails,
	}, mw.Created()
}

func (a *API) parseID(id string) (schema.ID, error) {
	v, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, errors.Wrap(err, "invalid id format")
	}

	return schema.ID(v), nil
}

func (a *API) pipeline(r types.Reservation, err error) (types.Reservation, error) {
	if err != nil {
		return r, err
	}
	pl, err := types.NewPipeline(r)
	if err != nil {
		return r, errors.Wrap(err, "failed to process reservation state pipeline")
	}

	r, _ = pl.Next()
	return r, nil
}

func (a *API) get(r *http.Request) (interface{}, mw.Response) {
	id, err := a.parseID(mux.Vars(r)["res_id"])
	if err != nil {
		return nil, mw.BadRequest(fmt.Errorf("invalid reservation id"))
	}

	var filter types.ReservationFilter
	filter = filter.WithID(id)

	db := mw.Database(r)
	reservation, err := a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.NotFound(err)
	}

	return reservation, nil
}

func (a *API) list(r *http.Request) (interface{}, mw.Response) {
	var filter types.ReservationFilter
	filter, err := types.ApplyQueryFilter(r, filter)
	if err != nil {
		return nil, mw.BadRequest(err)
	}

	db := mw.Database(r)
	pager := models.PageFromRequest(r)
	cur, err := filter.Find(r.Context(), db, pager)
	if err != nil {
		return nil, mw.Error(err)
	}

	defer cur.Close(r.Context())

	total, err := filter.Count(r.Context(), db)
	if err != nil {
		return nil, mw.Error(err)
	}

	reservations := []types.Reservation{}

	for cur.Next(r.Context()) {
		var reservation types.Reservation
		if err := cur.Decode(&reservation); err != nil {
			// skip reservations we can not load
			// this is probably an old reservation
			currentID := cur.Current.Lookup("_id").Int64()
			log.Error().Err(err).Int64("id", currentID).Msg("failed to decode reservation")
			continue
		}

		reservation, err := a.pipeline(reservation, nil)
		if err != nil {
			log.Error().Err(err).Int64("id", int64(reservation.ID)).Msg("failed to process reservation")
			continue
		}

		reservations = append(reservations, reservation)
	}

	pages := fmt.Sprintf("%d", models.NrPages(total, *pager.Limit))
	return reservations, mw.Ok().WithHeader("Pages", pages)
}

func (a *API) queued(ctx context.Context, db *mongo.Database, nodeID string, limit int64) ([]types.Workload, error) {

	type intermediate struct {
		WorkloadID string                     `bson:"workload_id" json:"workload_id"`
		User       string                     `bson:"user" json:"user"`
		Type       generated.WorkloadTypeEnum `bson:"type" json:"type"`
		Content    bson.Raw                   `bson:"content" json:"content"`
		Created    schema.Date                `bson:"created" json:"created"`
		Duration   int64                      `bson:"duration" json:"duration"`
		Signature  string                     `bson:"signature" json:"signature"`
		ToDelete   bool                       `bson:"to_delete" json:"to_delete"`
		NodeID     string                     `json:"node_id" bson:"node_id"`
	}

	workloads := make([]types.Workload, 0)

	var queue types.QueueFilter
	queue = queue.WithNodeID(nodeID)

	cur, err := queue.Find(ctx, db, options.Find().SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		// why we have intermediate struct you say? I will tell you
		// Content in the workload structure is definition as of type interface{}
		// bson if found a nil interface, it initialize it with bson.D (list of elements)
		// so data in Content will be something like [{key: k1, value: v1}, {key: k2, value: v2}]
		// which is not the same structure expected in the node
		// hence we use bson.M to force it to load data in a map like {k1: v1, k2: v2}
		var wl intermediate

		if err := cur.Decode(&wl); err != nil {
			return workloads, err
		}

		obj := generated.ReservationWorkload{
			WorkloadId: wl.WorkloadID,
			User:       wl.User,
			Type:       wl.Type,
			Created:    wl.Created,
			Duration:   wl.Duration,
			Signature:  wl.Signature,
			ToDelete:   wl.ToDelete,
		}

		switch wl.Type {
		case generated.WorkloadTypeContainer:
			var data generated.Container
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeVolume:
			var data generated.Volume
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeZDB:
			var data generated.ZDB
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeNetwork:
			var data generated.Network
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeKubernetes:
			var data generated.K8S
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeDomainDelegate:
			var data generated.GatewayDelegate
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeSubDomain:
			var data generated.GatewaySubdomain
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeProxy:
			var data generated.GatewayProxy
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data

		case generated.WorkloadTypeReverseProxy:
			var data generated.GatewayReserveProxy
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data
		case generated.WorkloadTypeGateway4To6:
			var data generated.Gateway4To6
			if err := bson.Unmarshal(wl.Content, &data); err != nil {
				return nil, err
			}
			obj.Content = data
		}

		workloads = append(workloads, types.Workload{
			NodeID:              wl.NodeID,
			ReservationWorkload: obj,
		})
	}

	return workloads, nil
}

func (a *API) workloads(r *http.Request) (interface{}, mw.Response) {
	const (
		maxPageSize = 200
	)

	var (
		nodeID = mux.Vars(r)["node_id"]
	)

	db := mw.Database(r)
	workloads, err := a.queued(r.Context(), db, nodeID, maxPageSize)
	if err != nil {
		return nil, mw.Error(err)
	}
	log.Debug().Msgf("%d queue", len(workloads))

	if len(workloads) > maxPageSize {
		return workloads, nil
	}

	from, err := a.parseID(r.FormValue("from"))
	if err != nil {
		return nil, mw.BadRequest(err)
	}

	// store last reservation ID
	lastID, err := types.ReservationLastID(r.Context(), db)
	if err != nil {
		return nil, mw.Error(err)
	}

	filter := types.ReservationFilter{}.WithIDGE(from)
	filter = filter.WithNodeID(nodeID)

	cur, err := filter.Find(r.Context(), db)
	if err != nil {
		return nil, mw.Error(err)
	}

	defer cur.Close(r.Context())

	for cur.Next(r.Context()) {
		var reservation types.Reservation
		if err := cur.Decode(&reservation); err != nil {
			return nil, mw.Error(err)
		}

		reservation, err = a.pipeline(reservation, nil)
		if err != nil {
			log.Error().Err(err).Int64("id", int64(reservation.ID)).Msg("failed to process reservation")
			continue
		}

		if reservation.NextAction == types.Delete {
			if err := a.setReservationDeleted(r.Context(), db, reservation.ID); err != nil {
				return nil, mw.Error(err)
			}
		}

		// only reservations that is in right status
		if !reservation.IsAny(types.Deploy, types.Delete) {
			continue
		}

		workloads = append(workloads, reservation.Workloads(nodeID)...)

		if len(workloads) >= maxPageSize {
			break
		}
	}

	return workloads, mw.Ok().WithHeader("x-last-id", fmt.Sprint(lastID))
}

func (a *API) workloadGet(r *http.Request) (interface{}, mw.Response) {
	gwid := mux.Vars(r)["gwid"]

	rid, err := a.parseID(strings.Split(gwid, "-")[0])
	if err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "invalid reservation id part"))
	}

	var filter types.ReservationFilter
	filter = filter.WithID(rid)

	db := mw.Database(r)
	reservation, err := a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.NotFound(err)
	}
	// we use an empty node-id in listing to return all workloads in this reservation
	workloads := reservation.Workloads("")

	var workload *types.Workload
	for _, wl := range workloads {
		if wl.WorkloadId == gwid {
			workload = &wl
			break
		}
	}

	if workload == nil {
		return nil, mw.NotFound(fmt.Errorf("workload not found"))
	}

	var result struct {
		types.Workload
		Result []types.Result `json:"result"`
	}
	result.Workload = *workload
	for _, rs := range reservation.Results {
		if rs.WorkloadId == workload.WorkloadId {
			t := types.Result(rs)
			result.Result = append(result.Result, t)
			break
		}
	}

	return result, nil
}

func (a *API) workloadPutResult(r *http.Request) (interface{}, mw.Response) {
	defer r.Body.Close()

	nodeID := mux.Vars(r)["node_id"]
	gwid := mux.Vars(r)["gwid"]

	rid, err := a.parseID(strings.Split(gwid, "-")[0])
	if err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "invalid reservation id part"))
	}

	var result types.Result
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		return nil, mw.BadRequest(err)
	}

	var filter types.ReservationFilter
	filter = filter.WithID(rid)

	db := mw.Database(r)
	reservation, err := a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.NotFound(err)
	}
	// we use an empty node-id in listing to return all workloads in this reservation
	workloads := reservation.Workloads(nodeID)
	var workload *types.Workload
	for _, wl := range workloads {
		if wl.WorkloadId == gwid {
			workload = &wl
			break
		}
	}

	if workload == nil {
		return nil, mw.NotFound(errors.New("workload not found"))
	}

	result.NodeId = nodeID
	result.WorkloadId = gwid
	result.Epoch = schema.Date{Time: time.Now()}

	if err := result.Verify(nodeID); err != nil {
		return nil, mw.UnAuthorized(errors.Wrap(err, "invalid result signature"))
	}

	if err := types.ResultPush(r.Context(), db, rid, result); err != nil {
		return nil, mw.Error(err)
	}

	if err := types.WorkloadPop(r.Context(), db, gwid, nodeID); err != nil {
		return nil, mw.Error(err)
	}

	if result.State == generated.ResultStateError {
		if err := a.setReservationDeleted(r.Context(), db, rid); err != nil {
			return nil, mw.Error(err)
		}
	} else if result.State == generated.ResultStateOK {
		// check if entire reservation is deployed successfully
		// fetch reservation from db again to have result appended in the model
		reservation, err = a.pipeline(filter.Get(r.Context(), db))
		if err != nil {
			return nil, mw.NotFound(err)
		}

		if reservation.IsSuccessfullyDeployed() {
			a.escrow.ReservationDeployed(rid)
		}
	}

	return nil, mw.Created()
}

func (a *API) workloadPutDeleted(r *http.Request) (interface{}, mw.Response) {
	// WARNING: #TODO
	// This method does not validate the signature of the caller
	// because there is no payload in a delete call.
	// may be a simple body that has "reservation id" and "signature"
	// can be used, we use the reservation id to avoid using the same
	// request body to delete other reservations

	// HTTP Delete should not have a body though, so may be this should be
	// changed to a PUT operation.

	nodeID := mux.Vars(r)["node_id"]
	gwid := mux.Vars(r)["gwid"]

	rid, err := a.parseID(strings.Split(gwid, "-")[0])
	if err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "invalid reservation id part"))
	}

	var filter types.ReservationFilter
	filter = filter.WithID(rid)

	db := mw.Database(r)
	reservation, err := a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.NotFound(err)
	}

	// we use an empty node-id in listing to return all workloads in this reservation
	workloads := reservation.Workloads(nodeID)
	var workload *types.Workload
	for _, wl := range workloads {
		if wl.WorkloadId == gwid {
			workload = &wl
			break
		}
	}

	if workload == nil {
		return nil, mw.NotFound(errors.New("workload not found"))
	}

	result := reservation.ResultOf(gwid)
	if result == nil {
		// no result for this work load
		// QUESTION: should we still mark the result as deleted?
		result = &types.Result{
			WorkloadId: gwid,
			Epoch:      schema.Date{Time: time.Now()},
		}
	}

	result.State = generated.ResultStateDeleted

	if err := types.ResultPush(r.Context(), db, rid, *result); err != nil {
		return nil, mw.Error(err)
	}

	if err := types.WorkloadPop(r.Context(), db, gwid, nodeID); err != nil {
		return nil, mw.Error(err)
	}

	// get it from store again (make sure we are up to date)
	reservation, err = a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.Error(err)
	}

	if !reservation.AllDeleted() {
		return nil, nil
	}

	if err := types.ReservationSetNextAction(r.Context(), db, reservation.ID, generated.NextActionDeleted); err != nil {
		return nil, mw.Error(err)
	}

	return nil, nil
}

func (a *API) signProvision(r *http.Request) (interface{}, mw.Response) {
	defer r.Body.Close()
	var signature generated.SigningSignature

	if err := json.NewDecoder(r.Body).Decode(&signature); err != nil {
		return nil, mw.BadRequest(err)
	}

	sig, err := hex.DecodeString(signature.Signature)
	if err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "invalid signature expecting hex encoded string"))
	}

	id, err := a.parseID(mux.Vars(r)["res_id"])
	if err != nil {
		return nil, mw.BadRequest(fmt.Errorf("invalid reservation id"))
	}

	var filter types.ReservationFilter
	filter = filter.WithID(id)

	db := mw.Database(r)
	reservation, err := a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.NotFound(err)
	}

	if reservation.NextAction != generated.NextActionSign {
		return nil, mw.UnAuthorized(fmt.Errorf("reservation not expecting signatures"))
	}

	in := func(i int64, l []int64) bool {
		for _, x := range l {
			if x == i {
				return true
			}
		}
		return false
	}

	if !in(signature.Tid, reservation.DataReservation.SigningRequestProvision.Signers) {
		return nil, mw.UnAuthorized(fmt.Errorf("signature not required for '%d'", signature.Tid))
	}

	user, err := phonebook.UserFilter{}.WithID(schema.ID(signature.Tid)).Get(r.Context(), db)
	if err != nil {
		return nil, mw.NotFound(errors.Wrap(err, "customer id not found"))
	}

	if err := reservation.SignatureVerify(user.Pubkey, sig); err != nil {
		return nil, mw.UnAuthorized(errors.Wrap(err, "failed to verify signature"))
	}

	signature.Epoch = schema.Date{Time: time.Now()}
	if err := types.ReservationPushSignature(r.Context(), db, id, types.SignatureProvision, signature); err != nil {
		return nil, mw.Error(err)
	}

	reservation, err = a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.Error(err)
	}

	if reservation.NextAction == generated.NextActionDeploy {
		types.WorkloadPush(r.Context(), db, reservation.Workloads("")...)
	}

	return nil, mw.Created()
}

func (a *API) signDelete(r *http.Request) (interface{}, mw.Response) {
	defer r.Body.Close()
	var signature generated.SigningSignature

	if err := json.NewDecoder(r.Body).Decode(&signature); err != nil {
		return nil, mw.BadRequest(err)
	}

	sig, err := hex.DecodeString(signature.Signature)
	if err != nil {
		return nil, mw.BadRequest(errors.Wrap(err, "invalid signature expecting hex encoded string"))
	}

	id, err := a.parseID(mux.Vars(r)["res_id"])
	if err != nil {
		return nil, mw.BadRequest(fmt.Errorf("invalid reservation id"))
	}

	var filter types.ReservationFilter
	filter = filter.WithID(id)

	db := mw.Database(r)
	reservation, err := a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.NotFound(err)
	}

	in := func(i int64, l []int64) bool {
		for _, x := range l {
			if x == i {
				return true
			}
		}
		return false
	}

	if !in(signature.Tid, reservation.DataReservation.SigningRequestDelete.Signers) {
		return nil, mw.UnAuthorized(fmt.Errorf("signature not required for '%d'", signature.Tid))
	}

	user, err := phonebook.UserFilter{}.WithID(schema.ID(signature.Tid)).Get(r.Context(), db)
	if err != nil {
		return nil, mw.NotFound(errors.Wrap(err, "customer id not found"))
	}

	if err := reservation.SignatureVerify(user.Pubkey, sig); err != nil {
		return nil, mw.UnAuthorized(errors.Wrap(err, "failed to verify signature"))
	}

	signature.Epoch = schema.Date{Time: time.Now()}
	if err := types.ReservationPushSignature(r.Context(), db, id, types.SignatureDelete, signature); err != nil {
		return nil, mw.Error(err)
	}

	reservation, err = a.pipeline(filter.Get(r.Context(), db))
	if err != nil {
		return nil, mw.Error(err)
	}

	if reservation.NextAction != generated.NextActionDelete {
		return nil, mw.Created()
	}

	if err := a.setReservationDeleted(r.Context(), db, reservation.ID); err != nil {
		return nil, mw.Error(err)
	}

	if err := types.WorkloadPush(r.Context(), db, reservation.Workloads("")...); err != nil {
		return nil, mw.Error(err)
	}

	return nil, mw.Created()
}

func (a *API) setReservationDeleted(ctx context.Context, db *mongo.Database, id schema.ID) error {
	// cancel reservation escrow in case the reservation has not yet been deployed
	a.escrow.ReservationCanceled(id)
	return types.ReservationSetNextAction(ctx, db, id, generated.NextActionDelete)
}
