package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/getAlby/hub/logger"
	decodepay "github.com/nbd-wtf/ln-decodepay"
	"github.com/sirupsen/logrus"
)

func (api *api) RebalanceChannel(ctx context.Context, rebalanceChannelRequest *RebalanceChannelRequest) (*RebalanceChannelResponse, error) {
	if api.svc.GetLNClient() == nil {
		return nil, errors.New("LNClient not started")
	}

	// Validate that the receive_through node is actually a channel peer
	channels, err := api.svc.GetLNClient().ListChannels(ctx)
	if err != nil {
		logger.Logger.WithError(err).Error("Failed to list channels for rebalance validation")
		return nil, fmt.Errorf("failed to validate rebalance request: %w", err)
	}

	var hasChannelWithNode bool
	var channelInfo string
	for _, channel := range channels {
		if channel.RemotePubkey == rebalanceChannelRequest.ReceiveThroughNodePubkey {
			hasChannelWithNode = true
			capacity := channel.LocalBalance + channel.RemoteBalance
			channelInfo = fmt.Sprintf("capacity=%d local_balance=%d remote_balance=%d active=%t",
				capacity, channel.LocalBalance, channel.RemoteBalance, channel.Active)
			break
		}
	}

	if !hasChannelWithNode {
		logger.Logger.WithFields(logrus.Fields{
			"receive_through_pubkey": rebalanceChannelRequest.ReceiveThroughNodePubkey,
			"available_peers": func() []string {
				var peers []string
				for _, ch := range channels {
					peers = append(peers, ch.RemotePubkey)
				}
				return peers
			}(),
		}).Error("No active channel found with specified receive_through node")
		return nil, fmt.Errorf("no channel found with node %s", rebalanceChannelRequest.ReceiveThroughNodePubkey)
	}

	logger.Logger.WithFields(logrus.Fields{
		"receive_through_pubkey": rebalanceChannelRequest.ReceiveThroughNodePubkey,
		"channel_info":           channelInfo,
		"amount_sat":             rebalanceChannelRequest.AmountSat,
	}).Info("Starting rebalance through validated channel peer")

	// Log all channels for routing diagnostics
	var channelSummary []map[string]interface{}
	for _, ch := range channels {
		capacity := ch.LocalBalance + ch.RemoteBalance
		channelSummary = append(channelSummary, map[string]interface{}{
			"remote_pubkey":  ch.RemotePubkey,
			"capacity_msat":  capacity,
			"local_balance":  ch.LocalBalance,
			"remote_balance": ch.RemoteBalance,
			"spendable":      ch.LocalSpendableBalance,
			"active":         ch.Active,
			"public":         ch.Public,
			"is_outbound":    ch.IsOutbound,
		})
	}
	logger.Logger.WithFields(logrus.Fields{
		"total_channels": len(channels),
		"channels":       channelSummary,
	}).Info("Available channels for routing diagnostics")

	receiveMetadata := map[string]interface{}{
		"receive_through": rebalanceChannelRequest.ReceiveThroughNodePubkey,
		"amount_sat":      rebalanceChannelRequest.AmountSat,
	}

	receiveInvoice, err := api.svc.GetTransactionsService().MakeInvoice(ctx, rebalanceChannelRequest.AmountSat*1000, "Alby Hub Rebalance through "+rebalanceChannelRequest.ReceiveThroughNodePubkey, "", 0, receiveMetadata, api.svc.GetLNClient(), nil, nil)
	if err != nil {
		logger.Logger.WithError(err).Error("failed to generate rebalance receive invoice")
		return nil, err
	}

	type rspCreateOrderRequest struct {
		Token                   string `json:"token"`
		PayRequest              string `json:"pay_request"`
		PayThroughThisPublicKey string `json:"pay_through_this_public_key"`
	}

	newRspCreateOrderRequest := rspCreateOrderRequest{
		Token:                   "alby-hub",
		PayRequest:              receiveInvoice.PaymentRequest,
		PayThroughThisPublicKey: rebalanceChannelRequest.ReceiveThroughNodePubkey,
	}

	payloadBytes, err := json.Marshal(newRspCreateOrderRequest)
	if err != nil {
		return nil, err
	}
	bodyReader := bytes.NewReader(payloadBytes)

	req, err := http.NewRequest(http.MethodPost, api.cfg.GetEnv().RebalanceServiceUrl+"/api/rebalance/v1/create_order", bodyReader)
	if err != nil {
		logger.Logger.WithError(err).WithFields(logrus.Fields{
			"request": newRspCreateOrderRequest,
		}).Error("Failed to create new rebalance request")
		return nil, err
	}

	setDefaultRequestHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: time.Second * 60,
	}

	res, err := client.Do(req)
	if err != nil {
		logger.Logger.WithError(err).WithFields(logrus.Fields{
			"request": newRspCreateOrderRequest,
		}).Error("Failed to request new rebalance order")
		return nil, err
	}

	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		logger.Logger.WithError(err).WithFields(logrus.Fields{
			"request": newRspCreateOrderRequest,
		}).Error("Failed to read response body")
		return nil, errors.New("failed to read response body")
	}

	if res.StatusCode >= 300 {
		logger.Logger.WithFields(logrus.Fields{
			"request":    newRspCreateOrderRequest,
			"body":       string(body),
			"statusCode": res.StatusCode,
		}).Error("rebalance create_order endpoint returned non-success code")
		return nil, fmt.Errorf("rebalance create_order endpoint returned non-success code: %s", string(body))
	}

	type rspRebalanceCreateOrderResponse struct {
		OrderId    string `json:"order_id"`
		PayRequest string `json:"pay_request"`
	}

	var rebalanceCreateOrderResponse rspRebalanceCreateOrderResponse

	err = json.Unmarshal(body, &rebalanceCreateOrderResponse)
	if err != nil {
		logger.Logger.WithError(err).WithFields(logrus.Fields{
			"request": newRspCreateOrderRequest,
		}).Error("Failed to deserialize json")
		return nil, fmt.Errorf("failed to deserialize json from rebalance create order response: %s", string(body))
	}

	logger.Logger.WithField("response", rebalanceCreateOrderResponse).Info("New rebalance order created")

	// Log additional context for debugging routing issues
	nodeInfo, err := api.svc.GetLNClient().GetNodeConnectionInfo(ctx)
	if err == nil {
		logger.Logger.WithFields(logrus.Fields{
			"our_pubkey":             nodeInfo.Pubkey,
			"receive_through_pubkey": rebalanceChannelRequest.ReceiveThroughNodePubkey,
		}).Info("Node information for rebalance routing")
	}

	paymentRequest, err := decodepay.Decodepay(rebalanceCreateOrderResponse.PayRequest)
	if err != nil {
		logger.Logger.WithError(err).Error("Failed to decode bolt11 invoice")
		return nil, err
	}

	payMetadata := map[string]interface{}{
		"receive_through": rebalanceChannelRequest.ReceiveThroughNodePubkey,
		"amount_sat":      rebalanceChannelRequest.AmountSat,
		"order_id":        rebalanceCreateOrderResponse.OrderId,
	}

	logger.Logger.WithFields(logrus.Fields{
		"receive_through_pubkey": rebalanceChannelRequest.ReceiveThroughNodePubkey,
		"amount_sat":             rebalanceChannelRequest.AmountSat,
		"order_id":               rebalanceCreateOrderResponse.OrderId,
		"payment_hash":           paymentRequest.PaymentHash,
		"destination":            paymentRequest.Payee,
		"amount_msat":            paymentRequest.MSatoshi,
		"description":            paymentRequest.Description,
		"expiry":                 paymentRequest.Expiry,
	}).Info("Attempting to pay rebalance invoice")

	payRebalanceInvoiceResponse, err := api.svc.GetTransactionsService().SendPaymentSync(ctx, rebalanceCreateOrderResponse.PayRequest, nil, payMetadata, api.svc.GetLNClient(), nil, nil, nil)

	if err != nil {
		logger.Logger.WithFields(logrus.Fields{
			"receive_through_pubkey": rebalanceChannelRequest.ReceiveThroughNodePubkey,
			"amount_sat":             rebalanceChannelRequest.AmountSat,
			"order_id":               rebalanceCreateOrderResponse.OrderId,
			"payment_hash":           paymentRequest.PaymentHash,
			"destination":            paymentRequest.Payee,
			"amount_msat":            paymentRequest.MSatoshi,
			"bolt11":                 rebalanceCreateOrderResponse.PayRequest,
		}).WithError(err).Error("Failed to pay rebalance invoice - check if routing path exists through specified node")

		// Provide more specific error guidance
		var enhancedError string
		if strings.Contains(err.Error(), "RouteNotFound") || strings.Contains(err.Error(), "route") {
			enhancedError = fmt.Sprintf("Failed to pay rebalance invoice: %v. "+
				"This typically indicates insufficient liquidity in the specified routing path. "+
				"Please ensure: 1) The receive_through node (%s) has sufficient outbound liquidity to the destination, "+
				"2) Your node has sufficient outbound liquidity to the receive_through node, "+
				"3) The amount (%d sats) is within routing limits",
				err, rebalanceChannelRequest.ReceiveThroughNodePubkey, rebalanceChannelRequest.AmountSat)
		} else {
			enhancedError = fmt.Sprintf("failed to pay rebalance invoice: %v", err)
		}

		return nil, errors.New(enhancedError)
	}

	return &RebalanceChannelResponse{
		TotalFeeSat: uint64(paymentRequest.MSatoshi)/1000 + payRebalanceInvoiceResponse.FeeMsat/1000 - rebalanceChannelRequest.AmountSat,
	}, nil
}
