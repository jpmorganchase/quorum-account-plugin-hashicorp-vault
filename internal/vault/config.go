package vault

import (
	"github.com/ethereum/go-ethereum/accounts"
	"io"
)

type ValidatableAccountParsableConfig interface {
	ValidatableConfig
	AccountParser
}

type ValidatableConfig interface {
	Validate() error
	ValidateForAccountCreation() error
}

type AccountAndWalletUrl struct {
	Account   accounts.Account
	WalletUrl accounts.URL
}

type AccountParser interface {
	ParseAccount(vaultAddr, filepath string) (AccountAndWalletUrl, error)
}

type AccountConfigUnmarshaller interface {
	Unmarshal(r io.Reader) (ValidatableAccountParsableConfig, error)
}
