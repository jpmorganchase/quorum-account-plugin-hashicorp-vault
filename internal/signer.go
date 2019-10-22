package internal

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/goquorum/quorum-plugin-definitions/signer/go/proto"
	"github.com/goquorum/quorum-plugin-hashicorp-account-store/internal/config"
	"github.com/goquorum/quorum-plugin-hashicorp-account-store/internal/manager"
	"github.com/goquorum/quorum-plugin-hashicorp-account-store/internal/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"log"
	"math/big"
	"strings"
	"time"
)

type HashicorpVaultAccountManager interface {
	accounts.Backend
	GetAccountCreator(vaultAddr string) (manager.AccountCreator, error)
	Wallet(url string) (accounts.Wallet, error)
	TimedUnlock(acct accounts.Account, passphrase string, timeout time.Duration) error
	Lock(acct accounts.Account) error
}

type HashicorpVaultAccountManagerDelegate struct {
	HashicorpVaultAccountManager
	events            chan accounts.WalletEvent
	eventSubscription event.Subscription
}

func (am *HashicorpVaultAccountManagerDelegate) init(config config.PluginAccountManagerConfig) error {
	log.Println("[PLUGIN SIGNER] init")
	manager, err := manager.NewManager(config.Vaults)
	if err != nil {
		return err
	}
	am.HashicorpVaultAccountManager = manager
	am.events = make(chan accounts.WalletEvent, 4*len(config.Vaults))
	return nil
}

func (am *HashicorpVaultAccountManagerDelegate) Status(_ context.Context, req *proto.StatusRequest) (*proto.StatusResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	wltStatus, err := w.Status()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.StatusResponse{Status: wltStatus}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) Open(_ context.Context, req *proto.OpenRequest) (*proto.OpenResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := w.Open(req.Passphrase); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.OpenResponse{}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) Close(_ context.Context, req *proto.CloseRequest) (*proto.CloseResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := w.Close(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.CloseResponse{}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) Accounts(_ context.Context, req *proto.AccountsRequest) (*proto.AccountsResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	accts := w.Accounts()

	protoAccts := make([]*proto.Account, len(accts))
	for i, a := range accts {
		protoAccts[i] = asProtoAccount(a)
	}

	return &proto.AccountsResponse{Accounts: protoAccts}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) Contains(_ context.Context, req *proto.ContainsRequest) (*proto.ContainsResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.ContainsResponse{IsContained: w.Contains(a)}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) SignHash(_ context.Context, req *proto.SignHashRequest) (*proto.SignHashResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	result, err := w.SignHash(a, req.Hash)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.SignHashResponse{Result: result}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) SignTx(_ context.Context, req *proto.SignTxRequest) (*proto.SignTxResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	tx := new(types.Transaction)
	if err := rlp.DecodeBytes(req.RlpTx, tx); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	chainID := &big.Int{}
	chainID.SetBytes(req.ChainID)

	result, err := w.SignTx(a, tx, chainID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	rlpTx, err := rlp.EncodeToBytes(result)
	if err != nil {
		return nil, err
	}

	return &proto.SignTxResponse{RlpTx: rlpTx}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) SignHashWithPassphrase(_ context.Context, req *proto.SignHashWithPassphraseRequest) (*proto.SignHashResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	result, err := w.SignHashWithPassphrase(a, req.Passphrase, req.Hash)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.SignHashResponse{Result: result}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) SignTxWithPassphrase(_ context.Context, req *proto.SignTxWithPassphraseRequest) (*proto.SignTxResponse, error) {
	w, err := am.Wallet(req.WalletUrl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	tx := new(types.Transaction)
	if err := rlp.DecodeBytes(req.RlpTx, tx); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	chainID := &big.Int{}
	chainID.SetBytes(req.ChainID)

	result, err := w.SignTxWithPassphrase(a, req.Passphrase, tx, chainID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	rlpTx, err := rlp.EncodeToBytes(result)
	if err != nil {
		return nil, err
	}

	return &proto.SignTxResponse{RlpTx: rlpTx}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) GetEventStream(req *proto.GetEventStreamRequest, stream proto.Signer_GetEventStreamServer) error {
	defer func() {
		am.eventSubscription.Unsubscribe()
		am.eventSubscription = nil
	}()

	wallets := am.Wallets()

	// now that we have the initial set of wallets, subscribe to the acct manager backend to be notified when changes occur
	am.eventSubscription = am.HashicorpVaultAccountManager.Subscribe(am.events)

	// stream the currently held wallets to the caller
	for _, w := range wallets {
		pluginEvent := &proto.GetEventStreamResponse{
			WalletEvent: proto.GetEventStreamResponse_WALLET_ARRIVED,
			WalletUrl:   w.URL().String(),
		}

		if err := stream.Send(pluginEvent); err != nil {
			log.Println("[ERROR] error sending event: ", pluginEvent, "err: ", err)
			return err
		}
		log.Println("[DEBUG] sent event: ", pluginEvent)
	}

	// listen for wallet events and stream to the caller until termination
	for {
		e := <-am.events
		pluginEvent := asProtoWalletEvent(e)
		log.Println("[DEBUG] read event: ", pluginEvent)
		if err := stream.Send(pluginEvent); err != nil {
			log.Println("[ERROR] error sending event: ", pluginEvent, "err: ", err)
			return err
		}
		log.Println("[DEBUG] sent event: ", pluginEvent)
	}
}

func (am *HashicorpVaultAccountManagerDelegate) TimedUnlock(_ context.Context, req *proto.TimedUnlockRequest) (*proto.TimedUnlockResponse, error) {
	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = am.HashicorpVaultAccountManager.TimedUnlock(a, req.Password, time.Duration(req.Duration))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.TimedUnlockResponse{}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) Lock(_ context.Context, req *proto.LockRequest) (*proto.LockResponse, error) {
	a, err := asAccount(req.Account)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = am.HashicorpVaultAccountManager.Lock(a)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.LockResponse{}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) NewAccount(_ context.Context, req *proto.NewAccountRequest) (*proto.NewAccountResponse, error) {
	b, err := am.GetAccountCreator(req.NewVaultAccount.VaultAddress)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	acct, secretUri, err := b.NewAccount(asVaultAccountConfig(req.NewVaultAccount))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.NewAccountResponse{Account: asProtoAccount(acct), SecretUri: secretUri}, nil
}

func (am *HashicorpVaultAccountManagerDelegate) ImportRawKey(_ context.Context, req *proto.ImportRawKeyRequest) (*proto.ImportRawKeyResponse, error) {
	b, err := am.GetAccountCreator(req.NewVaultAccount.VaultAddress)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	key, err := crypto.HexToECDSA(req.RawKey)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	acct, secretUri, err := b.ImportECDSA(key, asVaultAccountConfig(req.NewVaultAccount))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proto.ImportRawKeyResponse{Account: asProtoAccount(acct), SecretUri: secretUri}, nil
}

// TODO duplicated from quorum plugin/accounts/gateway.go
func asAccount(pAcct *proto.Account) (accounts.Account, error) {
	addr := strings.TrimSpace(common.Bytes2Hex(pAcct.Address))

	if !common.IsHexAddress(addr) {
		return accounts.Account{}, fmt.Errorf("invalid hex address: %v", addr)
	}

	url, err := utils.ToUrl(pAcct.Url)
	if err != nil {
		return accounts.Account{}, err
	}

	acct := accounts.Account{
		Address: common.HexToAddress(addr),
		URL:     url,
	}

	return acct, nil
}

func asProtoAccount(acct accounts.Account) *proto.Account {
	return &proto.Account{
		Address: acct.Address.Bytes(),
		Url:     acct.URL.String(),
	}
}

// TODO end duplication

func asProtoWalletEvent(event accounts.WalletEvent) *proto.GetEventStreamResponse {
	var t proto.GetEventStreamResponse_WalletEvent

	switch event.Kind {
	case accounts.WalletArrived:
		t = proto.GetEventStreamResponse_WALLET_ARRIVED
	case accounts.WalletOpened:
		t = proto.GetEventStreamResponse_WALLET_OPENED
	case accounts.WalletDropped:
		t = proto.GetEventStreamResponse_WALLET_DROPPED
	}

	return &proto.GetEventStreamResponse{
		WalletEvent: t,
		WalletUrl:   event.Wallet.URL().String(),
	}
}

func asVaultAccountConfig(req *proto.NewVaultAccount) config.VaultSecretConfig {
	return config.VaultSecretConfig{
		PathParams: config.PathParams{
			SecretEnginePath: req.SecretEnginePath,
			SecretPath:       req.SecretPath,
		},
		AuthID:          req.AuthID,
		InsecureSkipCas: req.InsecureSkipCas,
		CasValue:        req.CasValue,
	}
}
