package partnerTracker

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/yearn/ydaemon/common/bigNumber"
	"github.com/yearn/ydaemon/common/contracts"
	"github.com/yearn/ydaemon/common/ethereum"
	"github.com/yearn/ydaemon/common/logs"
)

type TRefereeVaultBlockData struct {
	Referee ethcommon.Address
	Block   uint64
}
type TRefereeTransferEvent struct {
	TxHash      common.Hash
	BlockNumber uint64
	TxIndex     uint
	LogIndex    uint
	Value       *bigNumber.Int
	Referee     ethcommon.Address
	From        ethcommon.Address
	To          ethcommon.Address
	Token       ethcommon.Address
}

/**********************************************************************************************
** RetrieveAllPartnerTrackerEvents will retrieve all the events matchn the
** ReferrerBalanceIncrease event for a given partnerContract on a given chain. Once all the
** events are retrieved, we will build a tree that will allow us to easily retrieve any item.
** Then, one important step is to retrieve the transfers related to the vault token for each
** referee. This will be used to calculate the current balance of a referee for a given vault
** at any given time to compute the referral bonus.
**********************************************************************************************/
func RetrieveAllPartnerTrackerEvents(chainID uint64) (
	map[ethcommon.Address]map[ethcommon.Address]map[ethcommon.Address][]TEventReferredBalanceIncreased,
	map[ethcommon.Address]map[ethcommon.Address][]TRefereeTransferEvent,
) {
	/**********************************************************************************************
	** First we need to catch all the events that happened in the past to be able to calculate the
	** current state of the partner tracker
	**********************************************************************************************/
	timeBefore := time.Now()
	allEvents := retrieveAllRefererBalanceIncrease(chainID)
	logs.Success(`It tooks`, time.Since(timeBefore), `to load`, len(allEvents), ` referral balance increase events`)

	/**********************************************************************************************
	** Once we got all the events, we can check how many unique referrer, referee and vaults we
	** have and start building our relation tree:
	** [chainID][vault][referrer][referee][amount]
	** We will also store a struct of [referee - vault - blockNumber] in order to retrieve all
	** transfers that happened for a given referee and vault after a given blockNumber. This will
	** allow us to calculate the current balance of a referee for a given vault at any given time
	** to compute the referral bonus.
	**********************************************************************************************/
	refereeVaultBlockData := map[ethcommon.Address][]TRefereeVaultBlockData{}
	partnerTrackerTree := map[ethcommon.Address]map[ethcommon.Address]map[ethcommon.Address][]TEventReferredBalanceIncreased{}
	for _, event := range allEvents {
		/******************************************************************************************
		** Ugly go code to avoid crash because of nil pointer
		******************************************************************************************/
		if partnerTrackerTree[event.Vault.ToAddress()] == nil {
			partnerTrackerTree[event.Vault.ToAddress()] = map[ethcommon.Address]map[ethcommon.Address][]TEventReferredBalanceIncreased{}
		}
		if partnerTrackerTree[event.Vault.ToAddress()][event.PartnerID.ToAddress()] == nil {
			partnerTrackerTree[event.Vault.ToAddress()][event.PartnerID.ToAddress()] = map[ethcommon.Address][]TEventReferredBalanceIncreased{}
		}
		if partnerTrackerTree[event.Vault.ToAddress()][event.PartnerID.ToAddress()][event.Depositer.ToAddress()] == nil {
			partnerTrackerTree[event.Vault.ToAddress()][event.PartnerID.ToAddress()][event.Depositer.ToAddress()] = []TEventReferredBalanceIncreased{}
		}
		if refereeVaultBlockData[event.Vault.ToAddress()] == nil {
			refereeVaultBlockData[event.Vault.ToAddress()] = []TRefereeVaultBlockData{}
		}

		/******************************************************************************************
		** Actual code to add the amount to the tree
		******************************************************************************************/
		partnerTrackerTree[event.Vault.ToAddress()][event.PartnerID.ToAddress()][event.Depositer.ToAddress()] = append(
			partnerTrackerTree[event.Vault.ToAddress()][event.PartnerID.ToAddress()][event.Depositer.ToAddress()],
			event,
		)
		refereeVaultBlockData[event.Vault.ToAddress()] = append(
			refereeVaultBlockData[event.Vault.ToAddress()],
			TRefereeVaultBlockData{
				Referee: event.Depositer.ToAddress(),
				Block:   event.BlockNumber,
			},
		)
	}
	allTransfersForReferees := retrieveAllTransfersForReferee(chainID, refereeVaultBlockData)
	return partnerTrackerTree, allTransfersForReferees
}

/**********************************************************************************************
** retrieveAllTransfersForReferee will, for a given map of vaults and referees, retrieve all
** the transfers that happened for a given referee and vault after a given blockNumber (init
** time). This will be used to make sure we are using the correct balance for a given referee
** and vault at any given time.
**********************************************************************************************/
func retrieveAllTransfersForReferee(
	chainID uint64,
	refereeVaultBlockData map[ethcommon.Address][]TRefereeVaultBlockData,
) map[ethcommon.Address]map[ethcommon.Address][]TRefereeTransferEvent {
	client := ethereum.GetRPC(chainID)
	now := time.Now()
	transfersEvents := []TRefereeTransferEvent{}
	wg := sync.WaitGroup{}

	for vaultAddress, refereeBlockData := range refereeVaultBlockData {
		allRefereesAddresses := []ethcommon.Address{}
		earliestBlock := uint64(math.MaxUint64)
		for _, refereeBlock := range refereeBlockData {
			allRefereesAddresses = append(allRefereesAddresses, refereeBlock.Referee)
			if refereeBlock.Block < earliestBlock {
				earliestBlock = refereeBlock.Block
			}
		}
		opts := &bind.FilterOpts{Start: earliestBlock, End: nil}

		vaultTokenContract, _ := contracts.NewERC20(vaultAddress, client)
		wg.Add(2)
		go func(_allRefereesAddresses []ethcommon.Address) {
			defer wg.Done()
			if log, err := vaultTokenContract.FilterTransfer(opts, _allRefereesAddresses, nil); err == nil {
				for log.Next() {
					if log.Error() != nil {
						continue
					}
					transfersEvents = append(transfersEvents, TRefereeTransferEvent{
						TxHash:      log.Event.Raw.TxHash,
						BlockNumber: log.Event.Raw.BlockNumber,
						TxIndex:     log.Event.Raw.TxIndex,
						LogIndex:    log.Event.Raw.Index,
						Value:       bigNumber.SetInt(log.Event.Value),
						Referee:     log.Event.From,
						From:        log.Event.From,
						To:          log.Event.To,
						Token:       log.Event.Raw.Address,
					})
				}
			}
		}(allRefereesAddresses)
		go func(_allRefereesAddresses []ethcommon.Address) {
			defer wg.Done()
			if log, err := vaultTokenContract.FilterTransfer(opts, nil, _allRefereesAddresses); err == nil {
				for log.Next() {
					if log.Error() != nil {
						continue
					}
					transfersEvents = append(transfersEvents, TRefereeTransferEvent{
						TxHash:      log.Event.Raw.TxHash,
						BlockNumber: log.Event.Raw.BlockNumber,
						TxIndex:     log.Event.Raw.TxIndex,
						LogIndex:    log.Event.Raw.Index,
						Value:       bigNumber.SetInt(log.Event.Value),
						Referee:     log.Event.To,
						From:        log.Event.From,
						To:          log.Event.To,
						Token:       log.Event.Raw.Address,
					})
				}
			}
		}(allRefereesAddresses)
	}
	wg.Wait()

	logs.Success(`It tooks`, time.Since(now), `to retrieve all transfers for all user with a delegate deposit:`, len(transfersEvents))
	allTransfers := map[ethcommon.Address]map[ethcommon.Address][]TRefereeTransferEvent{} //[vault][referee][transfer]
	sort.Slice(transfersEvents, func(i, j int) bool {
		return transfersEvents[i].BlockNumber < transfersEvents[j].BlockNumber
	})

	for _, transfer := range transfersEvents {
		if allTransfers[transfer.Token] == nil {
			allTransfers[transfer.Token] = map[ethcommon.Address][]TRefereeTransferEvent{}
		}
		if allTransfers[transfer.Token][transfer.Referee] == nil {
			allTransfers[transfer.Token][transfer.Referee] = []TRefereeTransferEvent{}
		}
		allTransfers[transfer.Token][transfer.Referee] = append(allTransfers[transfer.Token][transfer.Referee], transfer)
	}
	return allTransfers
}

/**********************************************************************************************
** FilterReferralBalanceIncreaseEventsForVault will, for a given chainID, return the list of
** all events for a given vault address and a given upper block limit.
**********************************************************************************************/
func FilterReferralBalanceIncreaseEventsForVault(
	allEvents map[ethcommon.Address]map[ethcommon.Address]map[ethcommon.Address][]TEventReferredBalanceIncreased,
	vaultAddress ethcommon.Address,
	upperBlockLimit uint64,
) map[ethcommon.Address]map[ethcommon.Address][]TEventReferredBalanceIncreased {
	events := map[ethcommon.Address]map[ethcommon.Address][]TEventReferredBalanceIncreased{}
	for _, eventsPartnerLevel := range allEvents[vaultAddress] {
		for _, eventDepositorLevel := range eventsPartnerLevel {
			for _, event := range eventDepositorLevel {
				if event.BlockNumber < upperBlockLimit {
					if events[event.PartnerID.ToAddress()] == nil {
						events[event.PartnerID.ToAddress()] = map[ethcommon.Address][]TEventReferredBalanceIncreased{}
					}
					if events[event.PartnerID.ToAddress()][event.Depositer.ToAddress()] == nil {
						events[event.PartnerID.ToAddress()][event.Depositer.ToAddress()] = []TEventReferredBalanceIncreased{}
					}
					events[event.PartnerID.ToAddress()][event.Depositer.ToAddress()] = append(
						events[event.PartnerID.ToAddress()][event.Depositer.ToAddress()],
						event,
					)
				}
			}
		}
	}

	//sort by block number
	for _, eventsPartnerLevel := range events {
		for _, eventDepositorLevel := range eventsPartnerLevel {
			sort.Slice(eventDepositorLevel, func(i, j int) bool {
				return eventDepositorLevel[i].BlockNumber < eventDepositorLevel[j].BlockNumber
			})
		}
	}
	return events
}