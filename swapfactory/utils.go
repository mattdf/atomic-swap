package swapfactory

import (
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/noot/atomic-swap/common"
	mcrypto "github.com/noot/atomic-swap/crypto/monero"
)

// GetSecretFromLog returns the secret from a Claimed or Refunded log
func GetSecretFromLog(log *ethtypes.Log, event string) (*mcrypto.PrivateSpendKey, error) {
	if event != "Refunded" && event != "Claimed" {
		return nil, errors.New("invalid event name, must be one of Claimed or Refunded")
	}

	abi, err := abi.JSON(strings.NewReader(SwapFactoryABI))
	if err != nil {
		return nil, err
	}

	data := log.Data
	res, err := abi.Unpack(event, data)
	if err != nil {
		return nil, err
	}

	if len(res) < 2 {
		return nil, errors.New("log had not enough parameters")
	}

	s := res[1].([32]byte)
	if s == [32]byte{} {
		return nil, errors.New("got zero secret key from contract")
	}

	sk, err := mcrypto.NewPrivateSpendKey(common.Reverse(s[:]))
	if err != nil {
		return nil, err
	}

	return sk, nil
}

// CheckIfLogIDMatches returns true if the sawp ID in the log matches the given ID, false otherwise.
func CheckIfLogIDMatches(log ethtypes.Log, event string, id *big.Int) (bool, error) {
	if event != "Refunded" && event != "Claimed" {
		return false, errors.New("invalid event name, must be one of Claimed or Refunded")
	}

	abi, err := abi.JSON(strings.NewReader(SwapFactoryABI))
	if err != nil {
		return false, err
	}

	data := log.Data
	res, err := abi.Unpack(event, data)
	if err != nil {
		return false, err
	}

	if len(res) < 2 {
		return false, errors.New("log had not enough parameters")
	}

	eventID := res[0].(*big.Int)
	if eventID.Cmp(id) != 0 {
		return false, nil
	}

	return true, nil
}

// GetIDFromLog returns the swap ID from a New log.
func GetIDFromLog(log *ethtypes.Log) (*big.Int, error) {
	abi, err := abi.JSON(strings.NewReader(SwapFactoryABI))
	if err != nil {
		return nil, err
	}

	const event = "New"

	data := log.Data
	res, err := abi.Unpack(event, data)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, errors.New("log had not enough parameters")
	}

	id := res[0].(*big.Int)
	return id, nil
}
