package providerstates

import (
	"context"
	"errors"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"

	datatransfer "github.com/filecoin-project/go-data-transfer/v2"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-statemachine"
	"github.com/filecoin-project/go-statemachine/fsm"

	rm "github.com/filecoin-project/go-fil-markets/retrievalmarket"
)

var log = logging.Logger("retrieval-fsm")

// ProviderDealEnvironment is a bridge to the environment a provider deal is executing in
// It provides access to relevant functionality on the retrieval provider
type ProviderDealEnvironment interface {
	// Node returns the node interface for this deal
	Node() rm.RetrievalProviderNode
	PrepareBlockstore(ctx context.Context, dealID rm.DealID, pieceCid cid.Cid) error
	DeleteStore(dealID rm.DealID) error
	ResumeDataTransfer(context.Context, datatransfer.ChannelID) error
	CloseDataTransfer(context.Context, datatransfer.ChannelID) error
	ChannelState(ctx context.Context, chid datatransfer.ChannelID) (datatransfer.ChannelState, error)
	UpdateValidationStatus(ctx context.Context, chid datatransfer.ChannelID, result datatransfer.ValidationResult) error
}

// UnsealData fetches the piece containing data needed for the retrieval,
// unsealing it if necessary
func UnsealData(ctx fsm.Context, environment ProviderDealEnvironment, deal rm.ProviderDealState) error {
	if err := environment.PrepareBlockstore(ctx.Context(), deal.ID, deal.PieceInfo.PieceCID); err != nil {
		return ctx.Trigger(rm.ProviderEventUnsealError, err)
	}
	log.Debugf("blockstore prepared successfully, firing unseal complete for deal %d", deal.ID)
	return ctx.Trigger(rm.ProviderEventUnsealComplete)
}

// UnpauseDeal resumes a deal so we can start sending data after its unsealed
func UnpauseDeal(ctx fsm.Context, environment ProviderDealEnvironment, deal rm.ProviderDealState) error {
	log.Debugf("unpausing data transfer for deal %d", deal.ID)

	if deal.ChannelID != nil {
		log.Debugf("resuming data transfer for deal %d", deal.ID)
		err := environment.ResumeDataTransfer(ctx.Context(), *deal.ChannelID)
		if err != nil {
			return ctx.Trigger(rm.ProviderEventDataTransferError, err)
		}
	}
	return nil
}

// UpdateFunding saves payments as needed until a transfer can resume
func UpdateFunding(ctx fsm.Context, environment ProviderDealEnvironment, deal rm.ProviderDealState) error {
	log.Debugf("handling new event while in ongoing state of transfer %d", deal.ID)
	if deal.ChannelID == nil {
		return nil
	}
	channelState, err := environment.ChannelState(ctx.Context(), *deal.ChannelID)
	if err != nil {
		return ctx.Trigger(rm.ProviderEventDataTransferError, err)
	}
	result := updateFunding(ctx, environment, deal, channelState)
	err = environment.UpdateValidationStatus(ctx.Context(), *deal.ChannelID, result)
	if err != nil {
		return ctx.Trigger(rm.ProviderEventDataTransferError, err)
	}
	return nil
}

func updateFunding(ctx fsm.Context,
	environment ProviderDealEnvironment,
	deal rm.ProviderDealState,
	channelState datatransfer.ChannelState) datatransfer.ValidationResult {
	received, err := processLastVoucher(ctx, environment, channelState)
	if err != nil {
		return errorDealResponse(deal.Identifier(), err)
	}

	if received.Nil() {
		received = big.Zero()
	}

	totalPaid := big.Add(deal.FundsReceived, received)

	// check if all payments are received to continue the deal, or send updated required payment
	owed := deal.Params.OutstandingBalance(totalPaid, channelState.Queued(), channelState.Status().InFinalization())
	log.Debugf("provider: owed %d, total received %d = received so far %d + newly received %d, unseal price %d, price per byte %d",
		owed, totalPaid, deal.FundsReceived, received, deal.UnsealPrice, deal.PricePerByte)

	var voucherResult datatransfer.VoucherResult
	if owed.GreaterThan(big.Zero()) {
		if received.GreaterThan(big.Zero()) {
			log.Debugf("provider: owed %d: sending partial payment request", owed)
			_ = ctx.Trigger(rm.ProviderEventPartialPaymentReceived, received)
		}
		if received.GreaterThan(big.Zero()) || deal.Status != rm.DealStatusFundsNeededUnseal {
			voucherResult = &rm.DealResponse{
				ID:          deal.ID,
				Status:      deal.Status,
				PaymentOwed: owed,
			}
		}
	} else {
		_ = ctx.Trigger(rm.ProviderEventPaymentReceived, received)
		if deal.Status == rm.DealStatusFundsNeededLastPayment {
			log.Debugf("provider: funds needed: last payment")
			voucherResult = &rm.DealResponse{
				ID:     deal.ID,
				Status: rm.DealStatusCompleted,
			}
		}
	}
	return datatransfer.ValidationResult{
		Accepted:             true,
		VoucherResult:        voucherResult,
		ForcePause:           deal.Status == rm.DealStatusUnsealing || deal.Status == rm.DealStatusFundsNeededUnseal,
		RequiresFinalization: owed.GreaterThan(big.Zero()) || deal.Status != rm.DealStatusFundsNeededLastPayment,
		DataLimit:            deal.Params.NextInterval(totalPaid),
	}
}

func savePayment(ctx fsm.Context, env ProviderDealEnvironment, payment *rm.DealPayment) (abi.TokenAmount, error) {
	tok, _, err := env.Node().GetChainHead(context.TODO())
	if err != nil {
		_ = ctx.Trigger(rm.ProviderEventSaveVoucherFailed, err)
		return big.Zero(), err
	}
	// Save voucher
	received, err := env.Node().SavePaymentVoucher(context.TODO(), payment.PaymentChannel, payment.PaymentVoucher, nil, big.Zero(), tok)
	if err != nil {
		_ = ctx.Trigger(rm.ProviderEventSaveVoucherFailed, err)
		return big.Zero(), err
	}
	return received, nil
}

func processLastVoucher(ctx fsm.Context, env ProviderDealEnvironment, channelState datatransfer.ChannelState) (abi.TokenAmount, error) {
	voucher := channelState.LastVoucher()
	// read payment and return response if present
	if payment, isPayment := voucher.(*rm.DealPayment); isPayment {
		return savePayment(ctx, env, payment)
	}

	if _, isProposal := voucher.(*rm.DealProposal); isProposal {
		return big.Zero(), nil
	}
	return big.Zero(), errors.New("wrong voucher type")
}

func errorDealResponse(dealID rm.ProviderDealIdentifier, err error) datatransfer.ValidationResult {
	return datatransfer.ValidationResult{
		Accepted: false,
		VoucherResult: &rm.DealResponse{
			ID:      dealID.DealID,
			Message: err.Error(),
			Status:  rm.DealStatusErrored,
		},
	}
}

// CancelDeal clears a deal that went wrong for an unknown reason
func CancelDeal(ctx fsm.Context, environment ProviderDealEnvironment, deal rm.ProviderDealState) error {
	// Read next response (or fail)
	err := environment.DeleteStore(deal.ID)
	if err != nil {
		return ctx.Trigger(rm.ProviderEventMultiStoreError, err)
	}
	if deal.ChannelID != nil {
		err = environment.CloseDataTransfer(ctx.Context(), *deal.ChannelID)
		if err != nil && !errors.Is(err, statemachine.ErrTerminated) {
			return ctx.Trigger(rm.ProviderEventDataTransferError, err)
		}
	}
	return ctx.Trigger(rm.ProviderEventCancelComplete)
}

// CleanupDeal runs to do memory cleanup for an in progress deal
func CleanupDeal(ctx fsm.Context, environment ProviderDealEnvironment, deal rm.ProviderDealState) error {
	err := environment.DeleteStore(deal.ID)
	if err != nil {
		return ctx.Trigger(rm.ProviderEventMultiStoreError, err)
	}
	return ctx.Trigger(rm.ProviderEventCleanupComplete)
}
