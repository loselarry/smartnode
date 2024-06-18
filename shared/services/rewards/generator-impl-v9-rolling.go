package rewards

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ipfs/go-cid"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	tnsettings "github.com/rocket-pool/rocketpool-go/settings/trustednode"
	"github.com/rocket-pool/rocketpool-go/utils/eth"
	"github.com/rocket-pool/smartnode/shared/services/beacon"
	"github.com/rocket-pool/smartnode/shared/services/config"
	"github.com/rocket-pool/smartnode/shared/services/rewards/ssz_types"
	sszbig "github.com/rocket-pool/smartnode/shared/services/rewards/ssz_types/big"
	"github.com/rocket-pool/smartnode/shared/services/state"
	"github.com/rocket-pool/smartnode/shared/utils/log"
)

// Implementation for tree generator ruleset v9 with rolling record support
type treeGeneratorImpl_v9_rolling struct {
	networkState            *state.NetworkState
	rewardsFile             *ssz_types.SSZFile_v1
	elSnapshotHeader        *types.Header
	snapshotEnd             *SnapshotEnd
	log                     *log.ColorLogger
	logPrefix               string
	rp                      *rocketpool.RocketPool
	cfg                     *config.RocketPoolConfig
	bc                      beacon.Client
	opts                    *bind.CallOpts
	smoothingPoolBalance    *big.Int
	intervalDutiesInfo      *IntervalDutiesInfo
	slotsPerEpoch           uint64
	validatorIndexMap       map[string]*MinipoolInfo
	elStartTime             time.Time
	elEndTime               time.Time
	validNetworkCache       map[uint64]bool
	epsilon                 *big.Int
	intervalSeconds         *big.Int
	beaconConfig            beacon.Eth2Config
	rollingRecord           *RollingRecord
	nodeDetails             map[common.Address]*NodeSmoothingDetails
	invalidNetworkNodes     map[common.Address]uint64
	minipoolPerformanceFile *MinipoolPerformanceFile_v2
	nodeRewards             map[common.Address]*ssz_types.NodeReward
	networkRewards          map[ssz_types.Layer]*ssz_types.NetworkReward
}

// Create a new tree generator
func newTreeGeneratorImpl_v9_rolling(log *log.ColorLogger, logPrefix string, index uint64, snapshotEnd *SnapshotEnd, elSnapshotHeader *types.Header, intervalsPassed uint64, state *state.NetworkState, rollingRecord *RollingRecord) *treeGeneratorImpl_v9_rolling {
	return &treeGeneratorImpl_v9_rolling{
		rewardsFile: &ssz_types.SSZFile_v1{
			RewardsFileVersion: 3,
			RulesetVersion:     9,
			Index:              index,
			IntervalsPassed:    intervalsPassed,
			TotalRewards: &ssz_types.TotalRewards{
				ProtocolDaoRpl:               sszbig.NewUint256(0),
				TotalCollateralRpl:           sszbig.NewUint256(0),
				TotalOracleDaoRpl:            sszbig.NewUint256(0),
				TotalSmoothingPoolEth:        sszbig.NewUint256(0),
				PoolStakerSmoothingPoolEth:   sszbig.NewUint256(0),
				NodeOperatorSmoothingPoolEth: sszbig.NewUint256(0),
				TotalNodeWeight:              sszbig.NewUint256(0),
			},
			NetworkRewards: ssz_types.NetworkRewards{},
			NodeRewards:    ssz_types.NodeRewards{},
		},
		validatorIndexMap:   map[string]*MinipoolInfo{},
		elSnapshotHeader:    elSnapshotHeader,
		snapshotEnd:         snapshotEnd,
		log:                 log,
		logPrefix:           logPrefix,
		networkState:        state,
		rollingRecord:       rollingRecord,
		invalidNetworkNodes: map[common.Address]uint64{},
		minipoolPerformanceFile: &MinipoolPerformanceFile_v2{
			Index:               index,
			MinipoolPerformance: map[common.Address]*SmoothingPoolMinipoolPerformance_v2{},
		},
		nodeRewards:    map[common.Address]*ssz_types.NodeReward{},
		networkRewards: map[ssz_types.Layer]*ssz_types.NetworkReward{},
	}
}

// Get the version of the ruleset used by this generator
func (r *treeGeneratorImpl_v9_rolling) getRulesetVersion() uint64 {
	return r.rewardsFile.RulesetVersion
}

func (r *treeGeneratorImpl_v9_rolling) generateTree(rp *rocketpool.RocketPool, cfg *config.RocketPoolConfig, bc beacon.Client) (*GenerateTreeResult, error) {

	r.log.Printlnf("%s Generating tree using Ruleset v%d.", r.logPrefix, r.rewardsFile.RulesetVersion)

	// Provision some struct params
	r.rp = rp
	r.cfg = cfg
	r.bc = bc
	r.validNetworkCache = map[uint64]bool{
		0: true,
	}

	// Set the network name
	r.rewardsFile.Network, _ = ssz_types.NetworkFromString(fmt.Sprint(cfg.Smartnode.Network.Value))
	r.minipoolPerformanceFile.Network = fmt.Sprint(cfg.Smartnode.Network.Value)
	r.minipoolPerformanceFile.RewardsFileVersion = r.rewardsFile.RewardsFileVersion
	r.minipoolPerformanceFile.RulesetVersion = r.rewardsFile.RulesetVersion

	// Get the Beacon config
	r.beaconConfig = r.networkState.BeaconConfig
	r.slotsPerEpoch = r.beaconConfig.SlotsPerEpoch

	// Set the EL client call opts
	r.opts = &bind.CallOpts{
		BlockNumber: r.elSnapshotHeader.Number,
	}

	r.log.Printlnf("%s Creating tree for %d nodes", r.logPrefix, len(r.networkState.NodeDetails))

	// Get the max of node count and minipool count - this will be used for an error epsilon due to division truncation
	nodeCount := len(r.networkState.NodeDetails)
	minipoolCount := len(r.networkState.MinipoolDetails)
	if nodeCount > minipoolCount {
		r.epsilon = big.NewInt(int64(nodeCount))
	} else {
		r.epsilon = big.NewInt(int64(minipoolCount))
	}

	// Calculate the RPL rewards
	err := r.calculateRplRewards()
	if err != nil {
		return nil, fmt.Errorf("error calculating RPL rewards: %w", err)
	}

	// Calculate the ETH rewards
	err = r.calculateEthRewards(true)
	if err != nil {
		return nil, fmt.Errorf("error calculating ETH rewards: %w", err)
	}

	// Sort and assign the maps to the ssz file lists
	for nodeAddress, nodeReward := range r.nodeRewards {
		copy(nodeReward.Address[:], nodeAddress[:])
		r.rewardsFile.NodeRewards = append(r.rewardsFile.NodeRewards, nodeReward)
	}

	for layer, networkReward := range r.networkRewards {
		networkReward.Network = layer
		r.rewardsFile.NetworkRewards = append(r.rewardsFile.NetworkRewards, networkReward)
	}

	// Generate the Merkle Tree
	err = r.rewardsFile.GenerateMerkleTree()
	if err != nil {
		return nil, fmt.Errorf("error generating Merkle tree: %w", err)
	}

	// Sort all of the missed attestations so the files are always generated in the same state
	for _, minipoolInfo := range r.minipoolPerformanceFile.MinipoolPerformance {
		sort.Slice(minipoolInfo.MissingAttestationSlots, func(i, j int) bool {
			return minipoolInfo.MissingAttestationSlots[i] < minipoolInfo.MissingAttestationSlots[j]
		})
	}

	return &GenerateTreeResult{
		RewardsFile:             r.rewardsFile,
		InvalidNetworkNodes:     r.invalidNetworkNodes,
		MinipoolPerformanceFile: r.minipoolPerformanceFile,
	}, nil

}

// Quickly calculates an approximate of the staker's share of the smoothing pool balance without processing Beacon performance
// Used for approximate returns in the rETH ratio update
func (r *treeGeneratorImpl_v9_rolling) approximateStakerShareOfSmoothingPool(rp *rocketpool.RocketPool, cfg *config.RocketPoolConfig, bc beacon.Client) (*big.Int, error) {
	r.log.Printlnf("%s Approximating tree using Ruleset v%d.", r.logPrefix, r.rewardsFile.RulesetVersion)

	r.rp = rp
	r.cfg = cfg
	r.bc = bc
	r.validNetworkCache = map[uint64]bool{
		0: true,
	}

	// Set the network name
	r.rewardsFile.Network, _ = ssz_types.NetworkFromString(fmt.Sprint(cfg.Smartnode.Network.Value))
	r.minipoolPerformanceFile.Network = fmt.Sprint(cfg.Smartnode.Network.Value)
	r.minipoolPerformanceFile.RewardsFileVersion = r.rewardsFile.RewardsFileVersion
	r.minipoolPerformanceFile.RulesetVersion = r.rewardsFile.RulesetVersion

	// Get the Beacon config
	r.beaconConfig = r.networkState.BeaconConfig
	r.slotsPerEpoch = r.beaconConfig.SlotsPerEpoch

	// Set the EL client call opts
	r.opts = &bind.CallOpts{
		BlockNumber: r.elSnapshotHeader.Number,
	}

	r.log.Printlnf("%s Creating tree for %d nodes", r.logPrefix, len(r.networkState.NodeDetails))

	// Get the max of node count and minipool count - this will be used for an error epsilon due to division truncation
	nodeCount := len(r.networkState.NodeDetails)
	minipoolCount := len(r.networkState.MinipoolDetails)
	if nodeCount > minipoolCount {
		r.epsilon = big.NewInt(int64(nodeCount))
	} else {
		r.epsilon = big.NewInt(int64(minipoolCount))
	}

	// Calculate the ETH rewards
	err := r.calculateEthRewards(false)
	if err != nil {
		return nil, fmt.Errorf("error calculating ETH rewards: %w", err)
	}

	return r.rewardsFile.TotalRewards.PoolStakerSmoothingPoolEth.Int, nil
}

func (r *treeGeneratorImpl_v9_rolling) calculateNodeRplRewards(
	collateralRewards *big.Int,
	nodeEffectiveStake *big.Int,
	totalEffectiveRplStake *big.Int,
	nodeWeight *big.Int,
	totalNodeWeight *big.Int,
) *big.Int {

	if nodeEffectiveStake.Sign() <= 0 || nodeWeight.Sign() <= 0 {
		return big.NewInt(0)
	}

	// C is in the closed range [1, 6]
	// C := min(6, interval - 18 + 1)
	c := int64(6)
	interval := int64(r.networkState.NetworkDetails.RewardIndex)

	if c > (interval - 18 + 1) {
		c = interval - 18 + 1
	}

	if c <= 0 {
		c = 1
	}

	bigC := big.NewInt(c)

	// (collateralRewards * C * nodeWeight / (totalNodeWeight * 6)) + (collateralRewards * (6 - C) * nodeEffectiveStake / (totalEffectiveRplStake * 6))
	// First, (collateralRewards * C * nodeWeight / (totalNodeWeight * 6))
	rpip30Rewards := big.NewInt(0).Mul(collateralRewards, nodeWeight)
	rpip30Rewards.Mul(rpip30Rewards, bigC)
	rpip30Rewards.Quo(rpip30Rewards, big.NewInt(0).Mul(totalNodeWeight, six))

	// Once C hits 6 we can exit early as an optimization
	if c == 6 {
		return rpip30Rewards
	}

	// Second, (collateralRewards * (6 - C) * nodeEffectiveStake / (totalEffectiveRplStake * 6))
	oldRewards := big.NewInt(6)
	oldRewards.Sub(oldRewards, bigC)
	oldRewards.Mul(oldRewards, collateralRewards)
	oldRewards.Mul(oldRewards, nodeEffectiveStake)
	oldRewards.Quo(oldRewards, big.NewInt(0).Mul(totalEffectiveRplStake, six))

	// Add them together
	return rpip30Rewards.Add(rpip30Rewards, oldRewards)
}

// Calculates the RPL rewards for the given interval
func (r *treeGeneratorImpl_v9_rolling) calculateRplRewards() error {
	pendingRewards := r.networkState.NetworkDetails.PendingRPLRewards
	r.log.Printlnf("%s Pending RPL rewards: %s (%.3f)", r.logPrefix, pendingRewards.String(), eth.WeiToEth(pendingRewards))
	if pendingRewards.Cmp(common.Big0) == 0 {
		return fmt.Errorf("there are no pending RPL rewards, so this interval cannot be used for rewards submission")
	}

	// Get baseline Protocol DAO rewards
	pDaoPercent := r.networkState.NetworkDetails.ProtocolDaoRewardsPercent
	pDaoRewards := big.NewInt(0)
	pDaoRewards.Mul(pendingRewards, pDaoPercent)
	pDaoRewards.Div(pDaoRewards, eth.EthToWei(1))
	r.log.Printlnf("%s Expected Protocol DAO rewards: %s (%.3f)", r.logPrefix, pDaoRewards.String(), eth.WeiToEth(pDaoRewards))

	// Get node operator rewards
	nodeOpPercent := r.networkState.NetworkDetails.NodeOperatorRewardsPercent
	totalNodeRewards := big.NewInt(0)
	totalNodeRewards.Mul(pendingRewards, nodeOpPercent)
	totalNodeRewards.Div(totalNodeRewards, eth.EthToWei(1))
	r.log.Printlnf("%s Approx. total collateral RPL rewards: %s (%.3f)", r.logPrefix, totalNodeRewards.String(), eth.WeiToEth(totalNodeRewards))

	// Calculate the effective stake of each node, scaling by their participation in this interval
	// Before entering this function, make sure to hard-code MaxCollateralFraction to 1.5 eth (150% in wei), to comply with RPIP-30.
	// Do it here, as the network state value will still be used for vote power, so doing it upstream is likely to introduce more issues.
	// Doing it here also ensures that v1-7 continue to run correctly on networks other than mainnet where the max collateral fraction may not have always been 150%.
	r.networkState.NetworkDetails.MaxCollateralFraction = big.NewInt(1.5e18) // 1.5 eth is 150% in wei
	trueNodeEffectiveStakes, totalNodeEffectiveStake, err := r.networkState.CalculateTrueEffectiveStakes(true, true)
	if err != nil {
		return fmt.Errorf("error calculating effective RPL stakes: %w", err)
	}

	// Calculate the RPIP-30 weight of each node, scaling by their participation in this interval
	nodeWeights, totalNodeWeight, err := r.networkState.CalculateNodeWeights()
	if err != nil {
		return fmt.Errorf("error calculating node weights: %w", err)
	}

	// Operate normally if any node has rewards
	if totalNodeEffectiveStake.Sign() > 0 && totalNodeWeight.Sign() > 0 {
		// Make sure to record totalNodeWeight in the rewards file
		r.rewardsFile.TotalRewards.TotalNodeWeight.Set(totalNodeWeight)

		r.log.Printlnf("%s Calculating individual collateral rewards...", r.logPrefix)
		for i, nodeDetails := range r.networkState.NodeDetails {
			// Get how much RPL goes to this node
			nodeRplRewards := r.calculateNodeRplRewards(
				totalNodeRewards,
				trueNodeEffectiveStakes[nodeDetails.NodeAddress],
				totalNodeEffectiveStake,
				nodeWeights[nodeDetails.NodeAddress],
				totalNodeWeight,
			)

			// If there are pending rewards, add it to the map
			if nodeRplRewards.Sign() == 1 {
				rewardsForNode, exists := r.nodeRewards[nodeDetails.NodeAddress]
				if !exists {
					// Get the network the rewards should go to
					network := r.networkState.NodeDetails[i].RewardNetwork.Uint64()
					validNetwork, err := r.validateNetwork(network)
					if err != nil {
						return err
					}
					if !validNetwork {
						r.invalidNetworkNodes[nodeDetails.NodeAddress] = network
						network = 0
					}

					rewardsForNode = ssz_types.NewNodeReward(
						network,
						ssz_types.AddressFromBytes(nodeDetails.NodeAddress.Bytes()),
					)
					r.nodeRewards[nodeDetails.NodeAddress] = rewardsForNode
				}
				rewardsForNode.CollateralRpl.Add(rewardsForNode.CollateralRpl.Int, nodeRplRewards)

				// Add the rewards to the running total for the specified network
				rewardsForNetwork, exists := r.networkRewards[rewardsForNode.Network]
				if !exists {
					rewardsForNetwork = ssz_types.NewNetworkReward(rewardsForNode.Network)
					r.networkRewards[rewardsForNode.Network] = rewardsForNetwork
				}
				rewardsForNetwork.CollateralRpl.Add(rewardsForNetwork.CollateralRpl.Int, nodeRplRewards)
			}
		}

		// Sanity check to make sure we arrived at the correct total
		delta := big.NewInt(0)
		totalCalculatedNodeRewards := big.NewInt(0)
		for _, networkRewards := range r.networkRewards {
			totalCalculatedNodeRewards.Add(totalCalculatedNodeRewards, networkRewards.CollateralRpl.Int)
		}
		delta.Sub(totalNodeRewards, totalCalculatedNodeRewards).Abs(delta)
		if delta.Cmp(r.epsilon) == 1 {
			return fmt.Errorf("error calculating collateral RPL: total was %s, but expected %s; error was too large", totalCalculatedNodeRewards.String(), totalNodeRewards.String())
		}
		r.rewardsFile.TotalRewards.TotalCollateralRpl.Int.Set(totalCalculatedNodeRewards)
		r.log.Printlnf("%s Calculated rewards:           %s (error = %s wei)", r.logPrefix, totalCalculatedNodeRewards.String(), delta.String())
		pDaoRewards.Sub(pendingRewards, totalCalculatedNodeRewards)
	} else {
		// In this situation, none of the nodes in the network had eligible rewards so send it all to the pDAO
		pDaoRewards.Add(pDaoRewards, totalNodeRewards)
		r.log.Printlnf("%s None of the nodes were eligible for collateral rewards, sending everything to the pDAO; now at %s (%.3f)", r.logPrefix, pDaoRewards.String(), eth.WeiToEth(pDaoRewards))
	}

	// Handle Oracle DAO rewards
	oDaoPercent := r.networkState.NetworkDetails.TrustedNodeOperatorRewardsPercent
	totalODaoRewards := big.NewInt(0)
	totalODaoRewards.Mul(pendingRewards, oDaoPercent)
	totalODaoRewards.Div(totalODaoRewards, eth.EthToWei(1))
	r.log.Printlnf("%s Total Oracle DAO RPL rewards: %s (%.3f)", r.logPrefix, totalODaoRewards.String(), eth.WeiToEth(totalODaoRewards))

	oDaoDetails := r.networkState.OracleDaoMemberDetails

	// Calculate the true effective time of each oDAO node based on their participation in this interval
	totalODaoNodeTime := big.NewInt(0)
	trueODaoNodeTimes := map[common.Address]*big.Int{}
	for _, details := range oDaoDetails {
		// Get the timestamp of the node joining the oDAO
		joinTime := details.JoinedTime

		// Get the actual effective time, scaled based on participation
		intervalDuration := r.networkState.NetworkDetails.IntervalDuration
		intervalDurationBig := big.NewInt(int64(intervalDuration.Seconds()))
		participationTime := big.NewInt(0).Set(intervalDurationBig)
		snapshotBlockTime := time.Unix(int64(r.elSnapshotHeader.Time), 0)
		eligibleDuration := snapshotBlockTime.Sub(joinTime)
		if eligibleDuration < intervalDuration {
			participationTime = big.NewInt(int64(eligibleDuration.Seconds()))
		}
		trueODaoNodeTimes[details.Address] = participationTime

		// Add it to the total
		totalODaoNodeTime.Add(totalODaoNodeTime, participationTime)
	}

	for _, details := range oDaoDetails {
		address := details.Address

		// Calculate the oDAO rewards for the node: (participation time) * (total oDAO rewards) / (total participation time)
		individualOdaoRewards := big.NewInt(0)
		individualOdaoRewards.Mul(trueODaoNodeTimes[address], totalODaoRewards)
		individualOdaoRewards.Div(individualOdaoRewards, totalODaoNodeTime)

		rewardsForNode, exists := r.nodeRewards[address]
		if !exists {
			// Get the network the rewards should go to
			network := r.networkState.NodeDetailsByAddress[address].RewardNetwork.Uint64()
			validNetwork, err := r.validateNetwork(network)
			if err != nil {
				return err
			}
			if !validNetwork {
				r.invalidNetworkNodes[address] = network
				network = 0
			}

			rewardsForNode = ssz_types.NewNodeReward(
				network,
				ssz_types.AddressFromBytes(address.Bytes()),
			)
			r.nodeRewards[address] = rewardsForNode

		}
		rewardsForNode.OracleDaoRpl.Add(rewardsForNode.OracleDaoRpl.Int, individualOdaoRewards)

		// Add the rewards to the running total for the specified network
		rewardsForNetwork, exists := r.networkRewards[rewardsForNode.Network]
		if !exists {
			rewardsForNetwork = ssz_types.NewNetworkReward(rewardsForNode.Network)
			r.networkRewards[rewardsForNode.Network] = rewardsForNetwork
		}
		rewardsForNetwork.OracleDaoRpl.Add(rewardsForNetwork.OracleDaoRpl.Int, individualOdaoRewards)
	}

	// Sanity check to make sure we arrived at the correct total
	totalCalculatedOdaoRewards := big.NewInt(0)
	delta := big.NewInt(0)
	for _, networkRewards := range r.networkRewards {
		totalCalculatedOdaoRewards.Add(totalCalculatedOdaoRewards, networkRewards.OracleDaoRpl.Int)
	}
	delta.Sub(totalODaoRewards, totalCalculatedOdaoRewards).Abs(delta)
	if delta.Cmp(r.epsilon) == 1 {
		return fmt.Errorf("error calculating ODao RPL: total was %s, but expected %s; error was too large", totalCalculatedOdaoRewards.String(), totalODaoRewards.String())
	}
	r.rewardsFile.TotalRewards.TotalOracleDaoRpl.Int.Set(totalCalculatedOdaoRewards)
	r.log.Printlnf("%s Calculated rewards:           %s (error = %s wei)", r.logPrefix, totalCalculatedOdaoRewards.String(), delta.String())

	// Get actual protocol DAO rewards
	pDaoRewards.Sub(pDaoRewards, totalCalculatedOdaoRewards)
	r.rewardsFile.TotalRewards.ProtocolDaoRpl = sszbig.NewUint256(0)
	r.rewardsFile.TotalRewards.ProtocolDaoRpl.Set(pDaoRewards)
	r.log.Printlnf("%s Actual Protocol DAO rewards:   %s to account for truncation", r.logPrefix, pDaoRewards.String())

	return nil

}

// Calculates the ETH rewards for the given interval
func (r *treeGeneratorImpl_v9_rolling) calculateEthRewards(checkBeaconPerformance bool) error {

	// Get the Smoothing Pool contract's balance
	r.smoothingPoolBalance = r.networkState.NetworkDetails.SmoothingPoolBalance
	r.log.Printlnf("%s Smoothing Pool Balance: %s (%.3f)", r.logPrefix, r.smoothingPoolBalance.String(), eth.WeiToEth(r.smoothingPoolBalance))

	// Ignore the ETH calculation if there are no rewards
	if r.smoothingPoolBalance.Cmp(common.Big0) == 0 {
		return nil
	}

	if r.rewardsFile.Index == 0 {
		// This is the first interval, Smoothing Pool rewards are ignored on the first interval since it doesn't have a discrete start time
		return nil
	}

	startElBlockHeader, err := r.getBlocksAndTimesForInterval()
	if err != nil {
		return err
	}

	r.elStartTime = time.Unix(int64(startElBlockHeader.Time), 0)
	r.elEndTime = time.Unix(int64(r.elSnapshotHeader.Time), 0)
	r.intervalSeconds = big.NewInt(int64(r.elEndTime.Sub(r.elStartTime) / time.Second))

	// Process the attestation performance for each minipool during this interval
	r.intervalDutiesInfo = &IntervalDutiesInfo{
		Index: r.rewardsFile.Index,
		Slots: map[uint64]*SlotInfo{},
	}

	// Determine how much ETH each node gets and how much the pool stakers get
	poolStakerETH, nodeOpEth, err := r.calculateNodeRewards()
	if err != nil {
		return err
	}

	// Update the rewards maps
	for nodeAddress, nodeInfo := range r.nodeDetails {
		if nodeInfo.SmoothingPoolEth.Cmp(common.Big0) > 0 {
			rewardsForNode, exists := r.nodeRewards[nodeAddress]
			if !exists {
				network := nodeInfo.RewardsNetwork
				validNetwork, err := r.validateNetwork(network)
				if err != nil {
					return err
				}
				if !validNetwork {
					r.invalidNetworkNodes[nodeAddress] = network
					network = 0
				}

				rewardsForNode = ssz_types.NewNodeReward(
					network,
					ssz_types.AddressFromBytes(nodeAddress.Bytes()),
				)
				r.nodeRewards[nodeAddress] = rewardsForNode
			}
			rewardsForNode.SmoothingPoolEth.Add(rewardsForNode.SmoothingPoolEth.Int, nodeInfo.SmoothingPoolEth)

			// Add minipool rewards to the JSON
			for _, minipoolInfo := range nodeInfo.Minipools {
				successfulAttestations := uint64(minipoolInfo.AttestationCount)
				missingAttestations := uint64(len(minipoolInfo.MissingAttestationSlots))
				performance := &SmoothingPoolMinipoolPerformance_v2{
					Pubkey:                  minipoolInfo.ValidatorPubkey.Hex(),
					SuccessfulAttestations:  successfulAttestations,
					MissedAttestations:      missingAttestations,
					AttestationScore:        &QuotedBigInt{Int: minipoolInfo.AttestationScore.Int},
					EthEarned:               &QuotedBigInt{Int: *minipoolInfo.MinipoolShare},
					MissingAttestationSlots: []uint64{},
				}
				if successfulAttestations+missingAttestations == 0 {
					// Don't include minipools that have zero attestations
					continue
				}
				for slot := range minipoolInfo.MissingAttestationSlots {
					performance.MissingAttestationSlots = append(performance.MissingAttestationSlots, slot)
				}
				r.minipoolPerformanceFile.MinipoolPerformance[minipoolInfo.Address] = performance
			}

			// Add the rewards to the running total for the specified network
			rewardsForNetwork, exists := r.networkRewards[rewardsForNode.Network]
			if !exists {
				rewardsForNetwork = ssz_types.NewNetworkReward(rewardsForNode.Network)
				r.networkRewards[rewardsForNode.Network] = rewardsForNetwork
			}
			rewardsForNetwork.SmoothingPoolEth.Add(rewardsForNetwork.SmoothingPoolEth.Int, nodeInfo.SmoothingPoolEth)
		}
	}

	// Set the totals
	r.rewardsFile.TotalRewards.PoolStakerSmoothingPoolEth.Set(poolStakerETH)
	r.rewardsFile.TotalRewards.NodeOperatorSmoothingPoolEth.Set(nodeOpEth)
	r.rewardsFile.TotalRewards.TotalSmoothingPoolEth.Set(r.smoothingPoolBalance)
	return nil

}

// Calculate the distribution of Smoothing Pool ETH to each node
func (r *treeGeneratorImpl_v9_rolling) calculateNodeRewards() (*big.Int, *big.Int, error) {

	// Get the list of cheaters
	cheaters := r.getCheaters()

	// Get the latest scores from the rolling record
	minipools, totalScore, attestationCount := r.rollingRecord.GetScores(cheaters)

	// If there weren't any successful attestations, everything goes to the pool stakers
	if totalScore.Cmp(common.Big0) == 0 || attestationCount == 0 {
		r.log.Printlnf("WARNING: Total attestation score = %s, successful attestations = %d... sending the whole smoothing pool balance to the pool stakers.", totalScore.String(), attestationCount)
		return r.smoothingPoolBalance, big.NewInt(0), nil
	}

	totalEthForMinipools := big.NewInt(0)
	totalNodeOpShare := big.NewInt(0)
	totalNodeOpShare.Mul(r.smoothingPoolBalance, totalScore)
	totalNodeOpShare.Div(totalNodeOpShare, big.NewInt(int64(attestationCount)))
	totalNodeOpShare.Div(totalNodeOpShare, eth.EthToWei(1))

	r.nodeDetails = map[common.Address]*NodeSmoothingDetails{}
	for _, minipool := range minipools {
		// Get the node amount
		nodeInfo, exists := r.nodeDetails[minipool.NodeAddress]
		if !exists {
			nodeInfo = &NodeSmoothingDetails{
				Minipools:        []*MinipoolInfo{},
				SmoothingPoolEth: big.NewInt(0),
				RewardsNetwork:   r.networkState.NodeDetailsByAddress[minipool.NodeAddress].RewardNetwork.Uint64(),
			}
			r.nodeDetails[minipool.NodeAddress] = nodeInfo
		}
		nodeInfo.Minipools = append(nodeInfo.Minipools, minipool)

		// Add the minipool's score to the total node score
		minipoolEth := big.NewInt(0).Set(totalNodeOpShare)
		minipoolEth.Mul(minipoolEth, &minipool.AttestationScore.Int)
		minipoolEth.Div(minipoolEth, totalScore)
		minipool.MinipoolShare = minipoolEth
		nodeInfo.SmoothingPoolEth.Add(nodeInfo.SmoothingPoolEth, minipoolEth)
	}

	// Add the node amounts to the total
	for _, nodeInfo := range r.nodeDetails {
		totalEthForMinipools.Add(totalEthForMinipools, nodeInfo.SmoothingPoolEth)
	}

	// This is how much actually goes to the pool stakers - it should ideally be equal to poolStakerShare but this accounts for any cumulative floating point errors
	truePoolStakerAmount := big.NewInt(0).Sub(r.smoothingPoolBalance, totalEthForMinipools)

	// Sanity check to make sure we arrived at the correct total
	delta := big.NewInt(0).Sub(totalEthForMinipools, totalNodeOpShare)
	delta.Abs(delta)
	if delta.Cmp(r.epsilon) == 1 {
		return nil, nil, fmt.Errorf("error calculating smoothing pool ETH: total was %s, but expected %s; error was too large (%s wei)", totalEthForMinipools.String(), totalNodeOpShare.String(), delta.String())
	}

	// Calculate the staking pool share and the node op share
	poolStakerShare := big.NewInt(0).Sub(r.smoothingPoolBalance, totalNodeOpShare)

	r.log.Printlnf("%s Pool staker ETH:    %s (%.3f)", r.logPrefix, poolStakerShare.String(), eth.WeiToEth(poolStakerShare))
	r.log.Printlnf("%s Node Op ETH:        %s (%.3f)", r.logPrefix, totalNodeOpShare.String(), eth.WeiToEth(totalNodeOpShare))
	r.log.Printlnf("%s Calculated NO ETH:  %s (error = %s wei)", r.logPrefix, totalEthForMinipools.String(), delta.String())
	r.log.Printlnf("%s Adjusting pool staker ETH to %s to account for truncation", r.logPrefix, truePoolStakerAmount.String())

	return truePoolStakerAmount, totalEthForMinipools, nil

}

// Validates that the provided network is legal
func (r *treeGeneratorImpl_v9_rolling) validateNetwork(network uint64) (bool, error) {
	valid, exists := r.validNetworkCache[network]
	if !exists {
		var err error
		valid, err = tnsettings.GetNetworkEnabled(r.rp, big.NewInt(int64(network)), r.opts)
		if err != nil {
			return false, err
		}
		r.validNetworkCache[network] = valid
	}

	return valid, nil
}

// Gets the EL header for the given interval's start block
func (r *treeGeneratorImpl_v9_rolling) getBlocksAndTimesForInterval() (*types.Header, error) {

	// Get the Beacon block for the start slot of the record
	r.rewardsFile.ConsensusStartBlock = r.rollingRecord.StartSlot
	r.minipoolPerformanceFile.ConsensusStartBlock = r.rollingRecord.StartSlot
	beaconBlock, exists, err := r.bc.GetBeaconBlock(fmt.Sprint(r.rollingRecord.StartSlot))
	if err != nil {
		return nil, fmt.Errorf("error verifying block from interval start: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("couldn't retrieve CL block from interval start (slot %d); this likely means you checkpoint sync'd your Beacon Node and it has not backfilled to the previous interval yet so it cannot be used for tree generation", r.rollingRecord.StartSlot)
	}

	// Get the EL block for that Beacon block
	elBlockNumber := beaconBlock.ExecutionBlockNumber
	r.rewardsFile.ExecutionStartBlock = elBlockNumber
	r.minipoolPerformanceFile.ExecutionStartBlock = r.rewardsFile.ExecutionStartBlock
	startElHeader, err := r.rp.Client.HeaderByNumber(context.Background(), big.NewInt(int64(elBlockNumber)))
	if err != nil {
		return nil, fmt.Errorf("error getting EL header for block %d: %w", elBlockNumber, err)
	}

	r.rewardsFile.ConsensusEndBlock = r.snapshotEnd.ConsensusBlock
	r.minipoolPerformanceFile.ConsensusEndBlock = r.snapshotEnd.ConsensusBlock

	r.rewardsFile.ExecutionEndBlock = r.snapshotEnd.ExecutionBlock
	r.minipoolPerformanceFile.ExecutionEndBlock = r.snapshotEnd.ExecutionBlock

	// rollingRecord.StartSlot is the first non-missing slot, so it isn't suitable for startTime, but can be used for startBlock
	// it can safely be assumed to be in the same epoch, due to the implementation of GetStartSlotForInterval.
	// Calculate the time of the first slot in that epoch.
	startTime := r.beaconConfig.GetSlotTime((r.rollingRecord.StartSlot / r.beaconConfig.SlotsPerEpoch) * r.beaconConfig.SlotsPerEpoch)

	r.rewardsFile.StartTime = startTime
	r.minipoolPerformanceFile.StartTime = startTime

	endTime := r.beaconConfig.GetSlotTime(r.snapshotEnd.Slot)
	r.rewardsFile.EndTime = endTime
	r.minipoolPerformanceFile.EndTime = endTime

	return startElHeader, nil
}

// Detect and flag any cheaters
func (r *treeGeneratorImpl_v9_rolling) getCheaters() map[common.Address]bool {
	cheatingNodes := map[common.Address]bool{}
	three := big.NewInt(3)

	for _, nd := range r.networkState.NodeDetails {
		for _, mpd := range r.networkState.MinipoolDetailsByNode[nd.NodeAddress] {
			if mpd.PenaltyCount.Cmp(three) >= 0 {
				// If any minipool has 3+ penalties, ban the entire node
				cheatingNodes[nd.NodeAddress] = true
				break
			}
		}
	}

	return cheatingNodes
}

func (r *treeGeneratorImpl_v9_rolling) saveFiles(treeResult *GenerateTreeResult, nodeTrusted bool) (cid.Cid, map[string]cid.Cid, error) {
	return saveRewardsArtifacts(r.cfg.Smartnode, treeResult, nodeTrusted)
}
