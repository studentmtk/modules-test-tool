package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/b-harvest/modules-test-tool/client"
	"github.com/b-harvest/modules-test-tool/config"
	"github.com/b-harvest/modules-test-tool/tx"
	"github.com/b-harvest/modules-test-tool/wallet"
	rpcclient "github.com/tendermint/tendermint/rpc/client"

	"github.com/cosmos/cosmos-sdk/client/flags"
	sdktypes "github.com/cosmos/cosmos-sdk/types"
	ibctypes "github.com/cosmos/cosmos-sdk/x/ibc/applications/transfer/types"

	"github.com/rs/zerolog/log"

	"github.com/spf13/cobra"
)

const (
	flagPacketTimeoutHeight    = "packet-timeout-height"
	flagPacketTimeoutTimestamp = "packet-timeout-timestamp"
	flagAbsoluteTimeouts       = "absolute-timeouts"
)

func IBCtransferCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "transfer [src-chainid] [src-port] [src-channel] [receiver] [amount] [blocks] [tx-num] [msg-num]",
		Short:   "Transfer a fungible token through IBC",
		Aliases: []string{"t"},
		Args:    cobra.ExactArgs(8),
		Long: `Transfer a fungible token through IBC.

Example: $tester t gaia transfer channel-0 cosmos1pacc0fr45hggcn8jrfhgnqf8vgyqna7r5sftql 10uatom 10 1 1

blocks: how many blocks to keep the test going?
tx-num: how many transactions to be included in a block
msg-num: how many transaction messages to be included in a transaction
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := SetLogger(logLevel)
			if err != nil {
				return err
			}

			cfg, err := config.Read(config.DefaultConfigPath)
			if err != nil {
				return fmt.Errorf("failed to read config file: %s", err)
			}
			var mainchain config.IBCchain
			for _, i := range cfg.IBCconfig.Chains {
				if i.ChainId == args[0] {
					mainchain = i
					break
				}
			}
			client, err := client.NewClient(mainchain.Rpc, mainchain.Grpc)
			if err != nil {
				return fmt.Errorf("failed to connect clients: %s", err)
			}

			defer client.Stop() // nolint: errcheck
			ibcclientCtx := client.GetCLIContext()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			chainID, err := client.RPC.GetNetworkChainID(ctx)
			if err != nil {
				return fmt.Errorf("failed to get chain id: %s", err)
			}

			srcPort := args[1]
			srcChannel := args[2]
			receiver := args[3]

			coin, err := sdktypes.ParseCoinNormalized(args[4])
			if err != nil {
				return err
			}

			if !strings.HasPrefix(coin.Denom, "ibc/") {
				denomTrace := ibctypes.ParseDenomTrace(coin.Denom)
				coin.Denom = denomTrace.IBCDenom()
			}

			blocks, err := strconv.Atoi(args[5])
			if err != nil {
				return fmt.Errorf("blocks must be integer: %s", args[5])
			}

			txNum, err := strconv.Atoi(args[6])
			if err != nil {
				return fmt.Errorf("txNum must be integer: %s", args[6])
			}

			msgNum, err := strconv.Atoi(args[7])
			if err != nil {
				return fmt.Errorf("msgNum must be integer: %s", args[7])
			}

			accAddr, privKey, err := wallet.IBCRecoverAccountFromMnemonic(cfg.Custom.Mnemonics[0], "", mainchain.AccountHD, mainchain.AccountaddrPrefix)
			if err != nil {
				return fmt.Errorf("failed to retrieve account from mnemonic: %s", err)
			}

			gasLimit := uint64(cfg.Custom.GasLimit)

			fees := sdktypes.NewCoins(sdktypes.NewCoin(mainchain.TokenDenom, sdktypes.NewInt(cfg.Custom.FeeAmount)))
			memo := cfg.Custom.Memo

			tx := tx.IbcNewtransaction(client, chainID, gasLimit, fees, memo)

			account, err := client.GRPC.GetBaseAccountInfo(ctx, accAddr)
			if err != nil {
				return fmt.Errorf("failed to get account information: %s", err)
			}
			accSeq := account.GetSequence()
			accNum := account.GetAccountNumber()
			blockTimes := make(map[int64]time.Time)
			st, err := client.RPC.Status(ctx)
			if err != nil {
				return fmt.Errorf("get status: %w", err)
			}
			startingHeight := st.SyncInfo.LatestBlockHeight + 2
			log.Info().Msgf("current block height is %d, waiting for the next block to be committed", st.SyncInfo.LatestBlockHeight)

			if err := rpcclient.WaitForHeight(client.RPC, startingHeight-1, nil); err != nil {
				return fmt.Errorf("wait for height: %w", err)
			}
			log.Info().Msgf("starting simulation #%d, blocks = %d, num txs per block = %d", blocks+1, blocks, txNum)
			targetHeight := startingHeight

			for i := 0; i < blocks; i++ {
				st, err := client.RPC.Status(ctx)
				if err != nil {
					return fmt.Errorf("get status: %w", err)
				}
				if st.SyncInfo.LatestBlockHeight != targetHeight-1 {
					log.Warn().Int64("expected", targetHeight-1).Int64("got", st.SyncInfo.LatestBlockHeight).Msg("mismatching block height")
					targetHeight = st.SyncInfo.LatestBlockHeight + 1
				}

				started := time.Now()
				sent := 0
			loop:
				for sent < txNum {
					msgs, err := tx.CreateTransferBot(cmd, ibcclientCtx, srcPort, srcChannel, coin, accAddr, receiver, msgNum)
					if err != nil {
						return fmt.Errorf("failed to create msg: %s", err)
					}
					for sent < txNum {
						txByte, err := tx.IbcSign(ctx, accSeq, accNum, privKey, msgs...)
						if err != nil {
							return fmt.Errorf("failed to sign and broadcast: %s", err)
						}
						resp, err := client.GRPC.BroadcastTx(ctx, txByte)
						//log.Info().Msgf("took %s broadcasting txs", resp)
						if err != nil {
							return fmt.Errorf("broadcast tx: %w", err)
						}
						accSeq = accSeq + 1
						if resp.TxResponse.Code != 0 {
							if resp.TxResponse.Code == 0x14 {
								log.Warn().Msg("mempool is full, stopping")
								accSeq = accSeq - 1
								break loop
							}
						}
						sent++
					}
				}
				log.Debug().Msgf("took %s broadcasting txs", time.Since(started))

				if err := rpcclient.WaitForHeight(client.RPC, targetHeight, nil); err != nil {
					return fmt.Errorf("wait for height: %w", err)
				}
				r, err := client.RPC.Block(ctx, &targetHeight)
				if err != nil {
					return err
				}
				var blockDuration time.Duration
				bt, ok := blockTimes[targetHeight-1]
				if !ok {
					log.Warn().Msg("past block time not found")
				} else {
					blockDuration = r.Block.Time.Sub(bt)
					delete(blockTimes, targetHeight-1)
				}
				blockTimes[targetHeight] = r.Block.Time
				log.Info().
					Int64("height", targetHeight).
					Str("block-time", r.Block.Time.Format(time.RFC3339Nano)).
					Str("block-duration", blockDuration.String()).
					Int("broadcast-txs", sent).
					Int("committed-txs", len(r.Block.Txs)).
					Msg("block committed")
				targetHeight++
			}

			return nil
		},
	}
	cmd.Flags().String(flagPacketTimeoutHeight, ibctypes.DefaultRelativePacketTimeoutHeight, "Packet timeout block height. The timeout is disabled when set to 0-0.")
	cmd.Flags().Uint64(flagPacketTimeoutTimestamp, ibctypes.DefaultRelativePacketTimeoutTimestamp, "Packet timeout timestamp in nanoseconds. Default is 10 minutes. The timeout is disabled when set to 0.")
	cmd.Flags().Bool(flagAbsoluteTimeouts, false, "Timeout flags are used as absolute timeouts.")
	flags.AddTxFlagsToCmd(cmd)
	return cmd
}
