package internal

import (
	"context"
	"encoding/json"
	"github.com/hashicorp/go-plugin"
	"github.com/jpmorganchase/quorum-account-manager-plugin-sdk-go/proto"
	"github.com/jpmorganchase/quorum-account-manager-plugin-sdk-go/proto_common"
	"github.com/jpmorganchase/quorum-plugin-account-store-hashicorp/internal/config"
	"github.com/jpmorganchase/quorum-plugin-account-store-hashicorp/internal/hashicorp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"log"
	"time"
)

type HashicorpPlugin struct {
	plugin.Plugin
	acctManager proto.AccountManagerServer
}

func (p HashicorpPlugin) Init(_ context.Context, req *proto_common.PluginInitialization_Request) (*proto_common.PluginInitialization_Response, error) {
	startTime := time.Now()
	defer func() {
		log.Println("[INFO] plugin initialization took", time.Now().Sub(startTime).Round(time.Microsecond))
	}()

	var conf config.VaultClients

	if err := json.Unmarshal(req.GetRawConfiguration(), conf); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	if err := conf.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	am, err := hashicorp.NewAccountManager(conf)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	p.acctManager = am

	return &proto_common.PluginInitialization_Response{}, nil
}