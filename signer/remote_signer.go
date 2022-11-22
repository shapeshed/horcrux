package signer

import (
	"fmt"
	"net"
	"time"

	tmCryptoEd2219 "github.com/tendermint/tendermint/crypto/ed25519"
	tmCryptoEncoding "github.com/tendermint/tendermint/crypto/encoding"
	tmLog "github.com/tendermint/tendermint/libs/log"
	tmNet "github.com/tendermint/tendermint/libs/net"
	tmService "github.com/tendermint/tendermint/libs/service"
	tmP2pConn "github.com/tendermint/tendermint/p2p/conn"
	"github.com/tendermint/tendermint/privval"
	tmProtoCrypto "github.com/tendermint/tendermint/proto/tendermint/crypto"
	tmProtoPrivval "github.com/tendermint/tendermint/proto/tendermint/privval"
	tmProto "github.com/tendermint/tendermint/proto/tendermint/types"
	tm "github.com/tendermint/tendermint/types"
)

// ReconnRemoteSigner dials using its dialer and responds to any
// signature requests using its privVal.
type ReconnRemoteSigner struct {
	tmService.BaseService

	config *RuntimeConfig

	address string
	privKey tmCryptoEd2219.PrivKey
	privVal tm.PrivValidator

	dialer net.Dialer
}

// NewReconnRemoteSigner return a ReconnRemoteSigner that will dial using the given
// dialer and respond to any signature requests over the connection
// using the given privVal.
//
// If the connection is broken, the ReconnRemoteSigner will attempt to reconnect.
func NewReconnRemoteSigner(
	config *RuntimeConfig,
	address string,
	logger tmLog.Logger,
	privVal tm.PrivValidator,
	dialer net.Dialer,
) *ReconnRemoteSigner {
	rs := &ReconnRemoteSigner{
		config:  config,
		address: address,
		privVal: privVal,
		dialer:  dialer,
		privKey: tmCryptoEd2219.GenPrivKey(),
	}

	rs.BaseService = *tmService.NewBaseService(logger, "RemoteSigner", rs)
	return rs
}

// OnStart implements cmn.Service.
func (rs *ReconnRemoteSigner) OnStart() error {
	go rs.loop()
	return nil
}

// main loop for ReconnRemoteSigner
func (rs *ReconnRemoteSigner) loop() {
	var conn net.Conn
	for {
		if !rs.IsRunning() {
			if conn != nil {
				if err := conn.Close(); err != nil {
					rs.Logger.Error("Close", "err", err.Error()+"closing listener failed")
				}
			}
			return
		}

		for conn == nil {
			proto, address := tmNet.ProtocolAndAddress(rs.address)
			netConn, err := rs.dialer.Dial(proto, address)
			if err != nil {
				sentryConnectTries.Add(float64(1))
				totalSentryConnectTries.Inc()
				rs.Logger.Error("Dialing", "err", err)
				rs.Logger.Info("Retrying", "sleep (s)", 3, "address", rs.address)
				time.Sleep(time.Second * 3)
				continue
			}
			sentryConnectTries.Set(0)

			rs.Logger.Info("Connected to Sentry", "address", rs.address)
			conn, err = tmP2pConn.MakeSecretConnection(netConn, rs.privKey)
			if err != nil {
				conn = nil
				rs.Logger.Error("Secret Conn", "err", err)
				rs.Logger.Info("Retrying", "sleep (s)", 3, "address", rs.address)
				time.Sleep(time.Second * 3)
				continue
			}
		}

		// since dialing can take time, we check running again
		if !rs.IsRunning() {
			if err := conn.Close(); err != nil {
				rs.Logger.Error("Close", "err", err.Error()+"closing listener failed")
			}
			return
		}

		req, err := ReadMsg(conn)
		if err != nil {
			rs.Logger.Error("readMsg", "err", err)
			conn.Close()
			conn = nil
			continue
		}

		// handleRequest handles request errors. We always send back a response
		res := rs.handleRequest(req)

		err = WriteMsg(conn, res)
		if err != nil {
			rs.Logger.Error("writeMsg", "err", err)
			conn.Close()
			conn = nil
		}
	}
}

func (rs *ReconnRemoteSigner) handleRequest(req tmProtoPrivval.Message) tmProtoPrivval.Message {
	switch typedReq := req.Sum.(type) {
	case *tmProtoPrivval.Message_SignVoteRequest:
		return rs.handleSignVoteRequest(typedReq.SignVoteRequest.ChainId, typedReq.SignVoteRequest.Vote)
	case *tmProtoPrivval.Message_SignProposalRequest:
		return rs.handleSignProposalRequest(typedReq.SignProposalRequest.ChainId, typedReq.SignProposalRequest.Proposal)
	case *tmProtoPrivval.Message_PubKeyRequest:
		return rs.handlePubKeyRequest(typedReq.PubKeyRequest.ChainId)
	case *tmProtoPrivval.Message_PingRequest:
		return rs.handlePingRequest()
	default:
		rs.Logger.Error("Unknown request", "err", fmt.Errorf("%v", typedReq))
		return tmProtoPrivval.Message{}
	}
}

func (rs *ReconnRemoteSigner) handleSignVoteRequest(chainID string, vote *tmProto.Vote) tmProtoPrivval.Message {
	msgSum := &tmProtoPrivval.Message_SignedVoteResponse{SignedVoteResponse: &tmProtoPrivval.SignedVoteResponse{
		Vote:  tmProto.Vote{},
		Error: nil,
	}}

	if err := rs.privVal.SignVote(chainID, vote); err != nil {
		switch typedErr := err.(type) {
		case *BeyondBlockError:
			rs.Logger.Debug(
				"Rejecting sign vote request",
				"chain_id", chainID,
				"height", vote.Height,
				"round", vote.Round,
				"type", vote.Type,
				"node", rs.address,
				"validator", fmt.Sprintf("%X", vote.ValidatorAddress),
				"reason", typedErr.msg,
			)
			beyondBlockErrors.Inc()
		default:
			rs.Logger.Error(
				"Failed to sign vote",
				"chain_id", chainID,
				"height", vote.Height,
				"round", vote.Round,
				"type", vote.Type,
				"node", rs.address,
				"validator", fmt.Sprintf("%X", vote.ValidatorAddress),
				"error", err,
			)
			failedSignVote.Inc()
		}
		msgSum.SignedVoteResponse.Error = getRemoteSignerError(err)
		return tmProtoPrivval.Message{Sum: msgSum}
	}
	// Show signatures provided to each node have the same signature and timestamps
	sigLen := 6
	if len(vote.Signature) < sigLen {
		sigLen = len(vote.Signature)
	}
	rs.Logger.Info(
		"Signed vote",
		"chain_id", chainID,
		"height", vote.Height,
		"round", vote.Round,
		"type", vote.Type,
		"sig", vote.Signature[:sigLen],
		"ts", vote.Timestamp.Unix(),
		"node", rs.address,
	)

	if vote.Type == tmProto.PrecommitType {
		stepSize := vote.Height - previousPrecommitHeight
		if previousPrecommitHeight != 0 && stepSize > 1 {
			missedPrecommits.Add(float64(stepSize))
			totalMissedPrecommits.Add(float64(stepSize))
		} else {
			missedPrecommits.Set(0)
		}
		previousPrecommitHeight = vote.Height // remember last PrecommitHeight

		metricsTimeKeeper.SetPreviousPrecommit(time.Now())

		lastPrecommitHeight.Set(float64(vote.Height))
		lastPrecommitRound.Set(float64(vote.Round))
		totalPrecommitsSigned.Inc()
	}
	if vote.Type == tmProto.PrevoteType {
		// Determine number of heights since the last Prevote
		stepSize := vote.Height - previousPrevoteHeight
		if previousPrevoteHeight != 0 && stepSize > 1 {
			missedPrevotes.Add(float64(stepSize))
			totalMissedPrevotes.Add(float64(stepSize))
		} else {
			missedPrevotes.Set(0)
		}

		previousPrevoteHeight = vote.Height // remember last PrevoteHeight

		metricsTimeKeeper.SetPreviousPrevote(time.Now())

		lastPrevoteHeight.Set(float64(vote.Height))
		lastPrevoteRound.Set(float64(vote.Round))
		totalPrevotesSigned.Inc()
	}

	msgSum.SignedVoteResponse.Vote = *vote
	return tmProtoPrivval.Message{Sum: msgSum}
}

func (rs *ReconnRemoteSigner) handleSignProposalRequest(
	chainID string,
	proposal *tmProto.Proposal,
) tmProtoPrivval.Message {
	msgSum := &tmProtoPrivval.Message_SignedProposalResponse{
		SignedProposalResponse: &tmProtoPrivval.SignedProposalResponse{
			Proposal: tmProto.Proposal{},
			Error:    nil,
		}}

	if err := rs.privVal.SignProposal(chainID, proposal); err != nil {
		switch typedErr := err.(type) {
		case *BeyondBlockError:
			rs.Logger.Debug(
				"Rejecting proposal sign request",
				"chain_id", chainID,
				"height", proposal.Height,
				"round", proposal.Round,
				"type", proposal.Type,
				"node", rs.address,
				"reason", typedErr.msg,
			)
			beyondBlockErrors.Inc()
		default:
			rs.Logger.Error(
				"Failed to sign proposal",
				"chain_id", chainID,
				"height", proposal.Height,
				"round", proposal.Round,
				"type", proposal.Type,
				"node", rs.address,
				"error", err,
			)
		}
		msgSum.SignedProposalResponse.Error = getRemoteSignerError(err)
		return tmProtoPrivval.Message{Sum: msgSum}
	}
	// Show signatures provided to each node have the same signature and timestamps
	sigLen := 6
	if len(proposal.Signature) < sigLen {
		sigLen = len(proposal.Signature)
	}
	rs.Logger.Info(
		"Signed proposal",
		"chain_id", chainID,
		"height", proposal.Height,
		"round", proposal.Round,
		"type", proposal.Type,
		"sig", proposal.Signature[:sigLen],
		"ts", proposal.Timestamp.Unix(),
		"node", rs.address,
	)
	lastProposalHeight.Set(float64(proposal.Height))
	lastProposalRound.Set(float64(proposal.Round))
	totalProposalsSigned.Inc()
	msgSum.SignedProposalResponse.Proposal = *proposal
	return tmProtoPrivval.Message{Sum: msgSum}
}

func (rs *ReconnRemoteSigner) handlePubKeyRequest(chainID string) tmProtoPrivval.Message {
	totalPubKeyRequests.Inc()
	msgSum := &tmProtoPrivval.Message_PubKeyResponse{PubKeyResponse: &tmProtoPrivval.PubKeyResponse{
		PubKey: tmProtoCrypto.PublicKey{},
		Error:  nil,
	}}

	if pvGuard, ok := rs.privVal.(*PvGuard); ok {
		if filePv, ok := pvGuard.PrivValidator.(*privval.FilePV); ok {
			if filePv.LastSignState.Height == 0 {
				pvGuard.PrivValidator = privval.LoadFilePVEmptyState(
					rs.config.KeyFilePath(false),
					rs.config.PrivValStateFile(chainID),
				)
			}
		}
	}

	pubKey, err := rs.privVal.GetPubKey()
	if err != nil {
		rs.Logger.Error(
			"Failed to get Pub Key",
			"chain_id", chainID,
			"node", rs.address,
			"error", err,
		)
		msgSum.PubKeyResponse.Error = getRemoteSignerError(err)
		return tmProtoPrivval.Message{Sum: msgSum}
	}
	pk, err := tmCryptoEncoding.PubKeyToProto(pubKey)
	if err != nil {
		rs.Logger.Error(
			"Failed to get Pub Key",
			"chain_id", chainID,
			"node", rs.address,
			"error", err,
		)
		msgSum.PubKeyResponse.Error = getRemoteSignerError(err)
		return tmProtoPrivval.Message{Sum: msgSum}
	}
	msgSum.PubKeyResponse.PubKey = pk
	return tmProtoPrivval.Message{Sum: msgSum}
}

func (rs *ReconnRemoteSigner) handlePingRequest() tmProtoPrivval.Message {
	return tmProtoPrivval.Message{Sum: &tmProtoPrivval.Message_PingResponse{PingResponse: &tmProtoPrivval.PingResponse{}}}
}

func getRemoteSignerError(err error) *tmProtoPrivval.RemoteSignerError {
	if err == nil {
		return nil
	}
	return &tmProtoPrivval.RemoteSignerError{
		Code:        0,
		Description: err.Error(),
	}
}

func StartRemoteSigners(config *RuntimeConfig, services []tmService.Service, logger tmLog.Logger,
	privVal tm.PrivValidator, nodes []string) ([]tmService.Service, error) {
	var err error
	go StartMetrics()
	for _, node := range nodes {
		// Tendermint requires a connection within 3 seconds of start or crashes
		// A long timeout such as 30 seconds would cause the sentry to fail in loops
		// Use a short timeout and dial often to connect within 3 second window
		dialer := net.Dialer{Timeout: 2 * time.Second}
		s := NewReconnRemoteSigner(config, node, logger, privVal, dialer)

		err = s.Start()
		if err != nil {
			return nil, err
		}

		services = append(services, s)
	}
	return services, err
}
