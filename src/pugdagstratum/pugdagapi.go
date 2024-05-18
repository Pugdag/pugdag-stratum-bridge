package pugdagstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/GRinvestPOOL/pugdag-stratum-bridge/src/gostratum"
	"github.com/Pugdag/pugdagd/app/appmessage"
	"github.com/Pugdag/pugdagd/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type PugdagApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	pugdagd      *rpcclient.RPCClient
	connected     bool
}

func NewPugdagApi(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*PugdagApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &PugdagApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "pugdagapi:"+address)),
		pugdagd:      client,
		connected:     true,
	}, nil
}

func (ks *PugdagApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *PugdagApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.pugdagd.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from pugdag, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.pugdagd.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from pugdag, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *PugdagApi) reconnect() error {
	if ks.pugdagd != nil {
		return ks.pugdagd.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.pugdagd = client
	return nil
}

func (s *PugdagApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking pugdagd sync state")
	}
	for {
		clientInfo, err := s.pugdagd.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from pugdagd @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("Karlsen is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("pugdagd synced, starting server")
	}
	return nil
}

func (s *PugdagApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.pugdagd.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from pugdag")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking pugdagd sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to pugdagd, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (ks *PugdagApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.pugdagd.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via pugdag-network/pugdag-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from pugdag")
	}
	return template, nil
}
