package bob

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	eth "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/fatih/color" //nolint:misspell

	"github.com/noot/atomic-swap/common"
	"github.com/noot/atomic-swap/common/types"
	mcrypto "github.com/noot/atomic-swap/crypto/monero"
	"github.com/noot/atomic-swap/crypto/secp256k1"
	"github.com/noot/atomic-swap/dleq"
	"github.com/noot/atomic-swap/monero"
	"github.com/noot/atomic-swap/net"
	"github.com/noot/atomic-swap/net/message"
	pcommon "github.com/noot/atomic-swap/protocol"
	pswap "github.com/noot/atomic-swap/protocol/swap"
	"github.com/noot/atomic-swap/swapfactory"
)

const revertSwapCompleted = "swap is already completed"

var (
	// this is from the autogenerated swap.go
	// TODO: generate this ourselves instead of hard-coding
	refundedTopic = ethcommon.HexToHash("0x4fd30f3ee0d64f7eaa62d0e005ca64c6a560652156d6c33f23ea8ca4936106e0")
)

type swapState struct {
	bob    *Instance
	ctx    context.Context
	cancel context.CancelFunc
	sync.Mutex
	infofile string

	info     *pswap.Info
	offer    *types.Offer
	statusCh chan types.Status

	// our keys for this session
	dleqProof    *dleq.Proof
	secp256k1Pub *secp256k1.PublicKey
	privkeys     *mcrypto.PrivateKeyPair
	pubkeys      *mcrypto.PublicKeyPair

	// swap contract and timeouts in it; set once contract is deployed
	contract       *swapfactory.SwapFactory
	contractSwapID *big.Int
	contractAddr   ethcommon.Address
	t0, t1         time.Time
	txOpts         *bind.TransactOpts

	// Alice's keys for this session
	alicePublicKeys         *mcrypto.PublicKeyPair
	aliceSecp256K1PublicKey *secp256k1.PublicKey

	// next expected network message
	nextExpectedMessage net.Message

	// channels
	readyCh chan struct{}

	// address of reclaimed monero wallet, if the swap is refunded77
	moneroReclaimAddress mcrypto.Address
}

func newSwapState(b *Instance, offer *types.Offer, statusCh chan types.Status, infofile string,
	providesAmount common.MoneroAmount, desiredAmount common.EtherAmount) (*swapState, error) {
	txOpts, err := bind.NewKeyedTransactorWithChainID(b.ethPrivKey, b.chainID)
	if err != nil {
		return nil, err
	}

	txOpts.GasPrice = b.gasPrice
	txOpts.GasLimit = b.gasLimit

	exchangeRate := types.ExchangeRate(providesAmount.AsMonero() / desiredAmount.AsEther())
	stage := types.ExpectingKeys
	if statusCh == nil {
		statusCh = make(chan types.Status, 7)
	}
	statusCh <- stage
	info := pswap.NewInfo(types.ProvidesXMR, providesAmount.AsMonero(), desiredAmount.AsEther(),
		exchangeRate, stage, statusCh)
	if err := b.swapManager.AddSwap(info); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(b.ctx)
	s := &swapState{
		ctx:                 ctx,
		cancel:              cancel,
		bob:                 b,
		offer:               offer,
		infofile:            infofile,
		nextExpectedMessage: &net.SendKeysMessage{},
		readyCh:             make(chan struct{}),
		txOpts:              txOpts,
		info:                info,
		statusCh:            statusCh,
	}

	if err := pcommon.WriteSwapIDToFile(infofile, info.ID()); err != nil {
		return nil, err
	}

	return s, nil
}

// SendKeysMessage ...
func (s *swapState) SendKeysMessage() (*net.SendKeysMessage, error) {
	if err := s.generateAndSetKeys(); err != nil {
		return nil, err
	}

	return &net.SendKeysMessage{
		ProvidedAmount:     s.info.ProvidedAmount(),
		PublicSpendKey:     s.pubkeys.SpendKey().Hex(),
		PrivateViewKey:     s.privkeys.ViewKey().Hex(),
		DLEqProof:          hex.EncodeToString(s.dleqProof.Proof()),
		Secp256k1PublicKey: s.secp256k1Pub.String(),
		EthAddress:         s.bob.ethAddress.String(),
	}, nil
}

// InfoFile returns the swap's infofile path
func (s *swapState) InfoFile() string {
	return s.infofile
}

// ReceivedAmount returns the amount received, or expected to be received, at the end of the swap
func (s *swapState) ReceivedAmount() float64 {
	return s.info.ReceivedAmount()
}

// ID returns the ID of the swap
func (s *swapState) ID() uint64 {
	return s.info.ID()
}

// Exit is called by the network when the protocol stream closes, or if the swap_refund RPC endpoint is called.
// It exists the swap by refunding if necessary. If no locking has been done, it simply aborts the swap.
// If the swap already completed successfully, this function does not doing anything in regards to the protoco.
func (s *swapState) Exit() error {
	if s == nil {
		return errNilSwapState
	}

	s.Lock()
	defer s.Unlock()
	return s.exit()
}

func (s *swapState) exit() error {
	if s == nil {
		return errNilSwapState
	}

	log.Debugf("attempting to exit swap: nextExpectedMessage=%v", s.nextExpectedMessage)

	defer func() {
		// stop all running goroutines
		s.cancel()
		s.bob.swapState = nil
		s.bob.swapManager.CompleteOngoingSwap()

		if s.info.Status() != types.CompletedSuccess {
			// re-add offer, as it wasn't taken successfully
			s.bob.offerManager.putOffer(s.offer)
		}
	}()

	if s.info.Status() == types.CompletedSuccess {
		str := color.New(color.Bold).Sprintf("**swap completed successfully: id=%d**", s.ID())
		log.Info(str)
		return nil
	}

	if s.info.Status() == types.CompletedRefund {
		str := color.New(color.Bold).Sprintf("**swap refunded successfully: id=%d**", s.ID())
		log.Info(str)
		return nil
	}

	switch s.nextExpectedMessage.(type) {
	case *net.SendKeysMessage:
		// we are fine, as we only just initiated the protocol.
		s.clearNextExpectedMessage(types.CompletedAbort)
		return nil
	case *message.NotifyETHLocked:
		// we were waiting for the contract to be deployed, but haven't
		// locked out funds yet, so we're fine.
		s.clearNextExpectedMessage(types.CompletedAbort)
		return nil
	case *message.NotifyReady:
		// we should check if Alice refunded, if so then check contract for secret
		address, err := s.tryReclaimMonero()
		if err != nil {
			log.Errorf("failed to check for refund: err=%s", err)

			// TODO: depending on the error, we should either retry to refund or try to claim.
			// we should wait for both events in the contract and proceed accordingly.
			//
			// we already locked our funds - need to wait until we can claim
			// the funds (ie. wait until after t0)
			txHash, err := s.tryClaim()
			if err != nil {
				// TODO: this shouldn't happen, as it means we had a race condition somewhere
				if strings.Contains(err.Error(), revertSwapCompleted) && !s.info.Status().IsOngoing() {
					return nil
				}

				log.Errorf("failed to claim funds: err=%s", err)
			} else {
				log.Infof("claimed ether! transaction hash=%s", txHash)
				s.clearNextExpectedMessage(types.CompletedSuccess)
				return nil
			}

			// TODO: keep retrying until success
			return err
		}

		s.clearNextExpectedMessage(types.CompletedRefund)
		s.moneroReclaimAddress = address
		log.Infof("regained private key to monero wallet, address=%s", address)
		return nil
	default:
		s.clearNextExpectedMessage(types.CompletedAbort)
		log.Errorf("unexpected nextExpectedMessage in Exit: type=%T", s.nextExpectedMessage)
		return errUnexpectedMessageType
	}
}

func (s *swapState) tryReclaimMonero() (mcrypto.Address, error) {
	skA, err := s.filterForRefund()
	if err != nil {
		return "", err
	}

	return s.reclaimMonero(skA)
}

func (s *swapState) reclaimMonero(skA *mcrypto.PrivateSpendKey) (mcrypto.Address, error) {
	vkA, err := skA.View()
	if err != nil {
		return "", err
	}

	skAB := mcrypto.SumPrivateSpendKeys(skA, s.privkeys.SpendKey())
	vkAB := mcrypto.SumPrivateViewKeys(vkA, s.privkeys.ViewKey())
	kpAB := mcrypto.NewPrivateKeyPair(skAB, vkAB)

	// write keys to file in case something goes wrong
	if err = pcommon.WriteSharedSwapKeyPairToFile(s.infofile, kpAB, s.bob.env); err != nil {
		return "", err
	}

	// TODO: check balance
	return monero.CreateMoneroWallet("bob-swap-wallet", s.bob.env, s.bob.client, kpAB)
}

func (s *swapState) filterForRefund() (*mcrypto.PrivateSpendKey, error) {
	const refundedEvent = "Refunded"

	logs, err := s.bob.ethClient.FilterLogs(s.ctx, eth.FilterQuery{
		Addresses: []ethcommon.Address{s.contractAddr},
		Topics:    [][]ethcommon.Hash{{refundedTopic}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to filter logs: %w", err)
	}

	if len(logs) == 0 {
		return nil, errNoRefundLogsFound
	}

	var (
		foundLog ethtypes.Log
		found    bool
	)

	for _, log := range logs {
		matches, err := swapfactory.CheckIfLogIDMatches(log, refundedEvent, s.contractSwapID) //nolint:govet
		if err != nil {
			continue
		}

		if matches {
			foundLog = log
			found = true
			break
		}
	}

	if !found {
		return nil, errNoRefundLogsFound
	}

	sa, err := swapfactory.GetSecretFromLog(&foundLog, refundedEvent)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret from log: %w", err)
	}

	return sa, nil
}

func (s *swapState) tryClaim() (ethcommon.Hash, error) {
	untilT0 := time.Until(s.t0)
	untilT1 := time.Until(s.t1)
	info, err := s.contract.Swaps(s.bob.callOpts, s.contractSwapID)
	if err != nil {
		return ethcommon.Hash{}, err
	}

	if untilT0 > 0 && !info.IsReady {
		// we need to wait until t0 to claim
		log.Infof("waiting until time %s to claim, time now=%s", s.t0, time.Now())
		<-time.After(untilT0 + time.Second)
	}

	if untilT1 < 0 {
		// we've passed t1, our only option now is for Alice to refund
		// and we can regain control of the locked XMR.
		return ethcommon.Hash{}, errPastClaimTime
	}

	return s.claimFunds()
}

// generateKeys generates Bob's spend and view keys (s_b, v_b)
// It returns Bob's public spend key and his private view key, so that Alice can see
// if the funds are locked.
func (s *swapState) generateAndSetKeys() error {
	if s == nil {
		return errNilSwapState
	}

	if s.privkeys != nil {
		return nil
	}

	keysAndProof, err := generateKeys()
	if err != nil {
		return err
	}

	s.dleqProof = keysAndProof.DLEqProof
	s.secp256k1Pub = keysAndProof.Secp256k1PublicKey
	s.privkeys = keysAndProof.PrivateKeyPair
	s.pubkeys = keysAndProof.PublicKeyPair

	return pcommon.WriteKeysToFile(s.infofile, s.privkeys, s.bob.env)
}

func generateKeys() (*pcommon.KeysAndProof, error) {
	return pcommon.GenerateKeysAndProof()
}

// getSecret secrets returns the current secret scalar used to unlock funds from the contract.
func (s *swapState) getSecret() [32]byte {
	secret := s.dleqProof.Secret()
	var sc [32]byte
	copy(sc[:], common.Reverse(secret[:]))
	return sc
}

// setAlicePublicKeys sets Alice's public spend and view keys
func (s *swapState) setAlicePublicKeys(sk *mcrypto.PublicKeyPair, secp256k1Pub *secp256k1.PublicKey) {
	s.alicePublicKeys = sk
	s.aliceSecp256K1PublicKey = secp256k1Pub
}

// setContract sets the contract in which Alice has locked her ETH.
func (s *swapState) setContract(address ethcommon.Address) error {
	var err error
	s.contractAddr = address
	s.contract, err = swapfactory.NewSwapFactory(address, s.bob.ethClient)
	return err
}

func (s *swapState) setTimeouts() error {
	info, err := s.contract.Swaps(s.bob.callOpts, s.contractSwapID)
	if err != nil {
		return fmt.Errorf("failed to get swap info from contract: err=%w", err)
	}

	s.t0 = time.Unix(info.Timeout0.Int64(), 0)
	s.t1 = time.Unix(info.Timeout1.Int64(), 0)
	return nil
}

// checkContract checks the contract's balance and Claim/Refund keys.
// if the balance doesn't match what we're expecting to receive, or the public keys in the contract
// aren't what we expect, we error and abort the swap.
func (s *swapState) checkContract(txHash ethcommon.Hash) error {
	receipt, err := common.WaitForReceipt(s.ctx, s.bob.ethClient, txHash)
	if err != nil {
		return fmt.Errorf("failed to get receipt for New transaction: %w", err)
	}

	// check that New log was emitted
	if len(receipt.Logs) == 0 {
		return errCannotFindNewLog
	}

	event, err := s.contract.ParseNew(*receipt.Logs[0])
	if err != nil {
		return err
	}

	if event.SwapID.Cmp(s.contractSwapID) != 0 {
		return errUnexpectedSwapID
	}

	// check that contract was constructed with correct secp256k1 keys
	skOurs := s.secp256k1Pub.Keccak256()
	if !bytes.Equal(event.ClaimKey[:], skOurs[:]) {
		return fmt.Errorf("contract claim key is not expected: got 0x%x, expected 0x%x", event.ClaimKey, skOurs)
	}

	skTheirs := s.aliceSecp256K1PublicKey.Keccak256()
	if !bytes.Equal(event.RefundKey[:], skTheirs[:]) {
		return fmt.Errorf("contract refund key is not expected: got 0x%x, expected 0x%x", event.RefundKey, skTheirs)
	}

	// check value of created swap
	info, err := s.contract.Swaps(s.bob.callOpts, s.contractSwapID)
	if err != nil {
		return err
	}

	expected := common.EtherToWei(s.info.ReceivedAmount()).BigInt()
	if info.Value.Cmp(expected) < 0 {
		return fmt.Errorf("contract does not have expected balance: got %s, expected %s", info.Value, expected)
	}

	return nil
}

// lockFunds locks Bob's funds in the monero account specified by public key
// (S_a + S_b), viewable with (V_a + V_b)
// It accepts the amount to lock as the input
// TODO: units
func (s *swapState) lockFunds(amount common.MoneroAmount) (mcrypto.Address, error) {
	kp := mcrypto.SumSpendAndViewKeys(s.alicePublicKeys, s.pubkeys)
	log.Infof("going to lock XMR funds, amount(piconero)=%d", amount)

	balance, err := s.bob.client.GetBalance(0)
	if err != nil {
		return "", err
	}

	log.Debug("total XMR balance: ", balance.Balance)
	log.Info("unlocked XMR balance: ", balance.UnlockedBalance)

	address := kp.Address(s.bob.env)
	txResp, err := s.bob.client.Transfer(address, 0, uint(amount))
	if err != nil {
		return "", err
	}

	log.Infof("locked XMR, txHash=%s fee=%d", txResp.TxHash, txResp.Fee)

	bobAddr, err := s.bob.client.GetAddress(0)
	if err != nil {
		return "", err
	}

	// if we're on a development --regtest node, generate some blocks
	if s.bob.env == common.Development {
		_ = s.bob.daemonClient.GenerateBlocks(bobAddr.Address, 2)
	} else {
		// otherwise, wait for new blocks
		height, err := monero.WaitForBlocks(s.bob.client, 1)
		if err != nil {
			return "", err
		}

		log.Infof("monero block height: %d", height)
	}

	if err := s.bob.client.Refresh(); err != nil {
		return "", err
	}

	log.Infof("successfully locked XMR funds: address=%s", address)
	return address, nil
}

// claimFunds redeems Bob's ETH funds by calling Claim() on the contract
func (s *swapState) claimFunds() (ethcommon.Hash, error) {
	pub := s.bob.ethPrivKey.Public().(*ecdsa.PublicKey)
	addr := ethcrypto.PubkeyToAddress(*pub)

	balance, err := s.bob.ethClient.BalanceAt(s.ctx, addr, nil)
	if err != nil {
		return ethcommon.Hash{}, err
	}

	log.Infof("balance before claim: %v ETH", common.EtherAmount(*balance).AsEther())

	// call swap.Swap.Claim() w/ b.privkeys.sk, revealing Bob's secret spend key
	sc := s.getSecret()
	tx, err := s.contract.Claim(s.txOpts, s.contractSwapID, sc)
	if err != nil {
		return ethcommon.Hash{}, err
	}

	log.Infof("sent claim tx, tx hash=%s", tx.Hash())

	if _, err = common.WaitForReceipt(s.ctx, s.bob.ethClient, tx.Hash()); err != nil {
		return ethcommon.Hash{}, fmt.Errorf("failed to check claim transaction receipt: %w", err)
	}

	balance, err = s.bob.ethClient.BalanceAt(s.ctx, addr, nil)
	if err != nil {
		return ethcommon.Hash{}, err
	}

	log.Infof("balance after claim: %v ETH", common.EtherAmount(*balance).AsEther())
	return tx.Hash(), nil
}
