package escrow

import (
	"math"
	"math/big"
	"time"

	"github.com/pkg/errors"
	"github.com/stellar/go/amount"
	"github.com/stellar/go/xdr"
	"github.com/threefoldtech/tfexplorer/models/generated/workloads"
)

type (
	rsuPerFarmer map[int64]rsu

	rsuPerNode map[string]rsu

	rsu struct {
		cru int64
		sru int64
		hru int64
		mru float64
	}

	cloudUnits struct {
		cu float64
		su float64
	}
)

// cost price of cloud units per hour:
// - 0.04 for a compute unit
// - 0.03 for a storage unit
// TFT price is fixed at $0.15 / TFT
// since this means neither compute unit nor cloud unit returns a nice value when
// expressed in TFT, we fix this to 3 digit precision.
const (
	computeUnitTFTCost = 0.266 //  0.04 / 0.15
	storageUnitTFTCost = 0.200 // 0.03 / 0.15
)

const (
	// durations
	day   = 24 * time.Hour
	week  = 7 * day
	month = 30 * day
)

func getDiscount(d time.Duration) float64 {

	switch {
	case d >= 12*month:
		return 1 - 0.70
	case d >= 6*month:
		return 1 - 0.60
	case d >= month:
		return 1 - 0.50
	case d >= week:
		return 1 - 0.25
	default:
		return 1
	}

}

// calculateReservationCost calculates the cost of reservation based on a resource per farmer map
func (e Stellar) calculateReservationCost(rsuPerFarmerMap rsuPerFarmer, duration time.Duration) (map[int64]xdr.Int64, error) {
	cloudUnitsPerFarmer := make(map[int64]cloudUnits)
	for id, rsu := range rsuPerFarmerMap {
		cloudUnitsPerFarmer[id] = rsuToCu(rsu)
	}

	costPerFarmerMap := make(map[int64]xdr.Int64)
	for id, cu := range cloudUnitsPerFarmer {
		// stellar does not have a nice type for currency, so use big.Float's during
		// calculation to avoid floating point errors. Since both the price and
		// cloud units are 3 digit precision floats, the result will be at most a 6
		// digit precision float.  Stellar has 7 digits precision, so we can use this
		// result as is.
		// TODO: do we round this to 3 digits precision as well?
		// NOTE: yes we need 3 big.Floats for the final calculation, or it screws up
		total := big.NewFloat(0)
		a := big.NewFloat(0)
		b := big.NewFloat(0)
		total = total.Add(
			a.Mul(big.NewFloat(computeUnitTFTCost), big.NewFloat(cu.cu)),
			b.Mul(big.NewFloat(storageUnitTFTCost), big.NewFloat(cu.su)),
		)

		// lock the duration to the hour above
		ceiledDuration := math.Ceil(duration.Hours())
		// compute the total amount of token to pay
		total = a.Mul(total, big.NewFloat(ceiledDuration))

		// apply the discount
		discount := getDiscount(duration)
		total = a.Mul(total, big.NewFloat(discount))

		cost, err := amount.Parse(total.String())
		if err != nil {
			return nil, errors.Wrap(err, "could not parse calculated cost")
		}
		costPerFarmerMap[id] = cost
	}
	return costPerFarmerMap, nil
}

func (e Stellar) processReservationResources(resData workloads.ReservationData) (rsuPerFarmer, error) {
	rsuPerNodeMap := make(rsuPerNode)
	for _, cont := range resData.Containers {
		rsuPerNodeMap[cont.NodeId] = rsuPerNodeMap[cont.NodeId].add(processContainer(cont))
	}
	for _, vol := range resData.Volumes {
		rsuPerNodeMap[vol.NodeId] = rsuPerNodeMap[vol.NodeId].add(processVolume(vol))
	}
	for _, zdb := range resData.Zdbs {
		rsuPerNodeMap[zdb.NodeId] = rsuPerNodeMap[zdb.NodeId].add(processZdb(zdb))
	}
	for _, k8s := range resData.Kubernetes {
		rsuPerNodeMap[k8s.NodeId] = rsuPerNodeMap[k8s.NodeId].add(processKubernetes(k8s))
	}
	rsuPerFarmerMap := make(rsuPerFarmer)
	for nodeID, rsu := range rsuPerNodeMap {
		node, err := e.nodeAPI.Get(e.ctx, e.db, nodeID, false)
		if err != nil {
			return nil, errors.Wrap(err, "could not get node")
		}
		rsuPerFarmerMap[node.FarmId] = rsuPerFarmerMap[node.FarmId].add(rsu)
	}
	return rsuPerFarmerMap, nil
}

func processContainer(cont workloads.Container) rsu {
	rsu := rsu{
		cru: cont.Capacity.Cpu,
		// round mru to 4 digits precision
		mru: math.Round(float64(cont.Capacity.Memory)/1024*10000) / 10000,
	}
	switch cont.Capacity.DiskType {
	case workloads.DiskTypeHDD:
		hru := math.Round(float64(cont.Capacity.DiskSize)/1024*10000) / 10000
		rsu.hru = int64(hru)
	case workloads.DiskTypeSSD:
		sru := math.Round(float64(cont.Capacity.DiskSize)/1024*10000) / 10000
		rsu.sru = int64(sru)
	}

	return rsu
}

func processVolume(vol workloads.Volume) rsu {
	switch vol.Type {
	case workloads.VolumeTypeHDD:
		return rsu{
			hru: vol.Size,
		}
	case workloads.VolumeTypeSSD:
		return rsu{
			sru: vol.Size,
		}
	}
	return rsu{}
}

func processZdb(zdb workloads.ZDB) rsu {
	switch zdb.DiskType {
	case workloads.DiskTypeHDD:
		return rsu{
			hru: zdb.Size,
		}
	case workloads.DiskTypeSSD:
		return rsu{
			sru: zdb.Size,
		}
	}
	return rsu{}

}

func processKubernetes(k8s workloads.K8S) rsu {
	switch k8s.Size {
	case 1:
		return rsu{
			cru: 1,
			mru: 2,
			sru: 50,
		}
	case 2:
		return rsu{
			cru: 2,
			mru: 4,
			sru: 100,
		}
	}
	return rsu{}

}

func (r rsu) add(other rsu) rsu {
	return rsu{
		cru: r.cru + other.cru,
		sru: r.sru + other.sru,
		hru: r.hru + other.hru,
		mru: r.mru + other.mru,
	}
}

// rsuToCu converts resource units to cloud units. Cloud units are rounded to 3
// decimal places
func rsuToCu(r rsu) cloudUnits {
	cloudUnits := cloudUnits{
		cu: math.Round(math.Min(r.mru/4*0.95, float64(r.cru)*2)*1000) / 1000,
		su: math.Round((float64(r.hru)/1093+float64(r.sru)/91)*1000) / 1000,
	}
	return cloudUnits
}
