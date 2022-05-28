package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/noot/atomic-swap/common"
	mcrypto "github.com/noot/atomic-swap/crypto/monero"
	"github.com/noot/atomic-swap/swapfactory"
)

type InfoFileContents struct {
	ContractAddress      string
	ContractSwapID       [32]byte
	ContractSwap         swapfactory.SwapFactorySwap
	PrivateKeyInfo       *mcrypto.PrivateKeyInfo
	SharedSwapPrivateKey *mcrypto.PrivateKeyInfo
}

// WriteContractAddressToFile writes the contract address to the given file
func WriteContractAddressToFile(infofile, addr string) error {
	file, contents, err := setupFile(infofile)
	if err != nil {
		return err
	}

	contents.ContractAddress = addr

	bz, err := json.MarshalIndent(contents, "", "\t")
	if err != nil {
		return err
	}

	_, err = file.Write(bz)
	return err
}

// // WriteSwapIDToFile writes the swap ID to the given file
// func WriteSwapIDToFile(infofile string, id uint64) error {
// 	file, contents, err := setupFile(infofile)
// 	if err != nil {
// 		return err
// 	}

// 	contents.SwapID = id

// 	bz, err := json.MarshalIndent(contents, "", "\t")
// 	if err != nil {
// 		return err
// 	}

// 	_, err = file.Write(bz)
// 	return err
// }

// WriteContractSwapToFile writes the given Swap contract struct to the given file
func WriteContractSwapToFile(infofile string, swapID [32]byte, swap swapfactory.SwapFactorySwap) error {
	file, contents, err := setupFile(infofile)
	if err != nil {
		return err
	}

	contents.ContractSwapID = swapID
	contents.ContractSwap = swap

	bz, err := json.MarshalIndent(contents, "", "\t")
	if err != nil {
		return err
	}

	_, err = file.Write(bz)
	return err
}

// WriteKeysToFile writes the given private key pair to the given file
func WriteKeysToFile(infofile string, keys *mcrypto.PrivateKeyPair, env common.Environment) error {
	file, contents, err := setupFile(infofile)
	if err != nil {
		return err
	}

	contents.PrivateKeyInfo = keys.Info(env)

	bz, err := json.MarshalIndent(contents, "", "\t")
	if err != nil {
		return err
	}

	_, err = file.Write(bz)
	return err
}

// WriteSharedSwapKeyPairToFile writes the given private key pair to the given file
func WriteSharedSwapKeyPairToFile(infofile string, keys *mcrypto.PrivateKeyPair, env common.Environment) error {
	file, contents, err := setupFile(infofile)
	if err != nil {
		return err
	}

	contents.SharedSwapPrivateKey = keys.Info(env)

	bz, err := json.MarshalIndent(contents, "", "\t")
	if err != nil {
		return err
	}

	_, err = file.Write(bz)
	return err
}

func setupFile(infofile string) (*os.File, *InfoFileContents, error) {
	exists, err := exists(infofile)
	if err != nil {
		return nil, nil, err
	}

	var (
		file     *os.File
		contents *InfoFileContents
	)
	if !exists {
		err = makeDir(filepath.Dir(infofile))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to make directory %s: %w", filepath.Dir(infofile), err)
		}

		file, err = os.Create(filepath.Clean(infofile))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create file %s: %w", filepath.Clean(infofile), err)
		}
	} else {
		file, err = os.OpenFile(filepath.Clean(infofile), os.O_RDWR, 0600)
		if err != nil {
			return nil, nil, err
		}

		bz, err := os.ReadFile(filepath.Clean(infofile))
		if err != nil {
			return nil, nil, err
		}

		if err = json.Unmarshal(bz, &contents); err != nil {
			return nil, nil, err
		}

		if err = file.Truncate(0); err != nil {
			return nil, nil, err
		}

		_, err = file.Seek(0, 0)
		if err != nil {
			return nil, nil, err
		}
	}

	if contents == nil {
		contents = &InfoFileContents{}
	}

	return file, contents, nil
}

func makeDir(dir string) error {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return err
	}

	return nil
}

// exists returns whether the given file or directory exists
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}

	if os.IsNotExist(err) {
		return false, nil
	}

	return false, err
}
