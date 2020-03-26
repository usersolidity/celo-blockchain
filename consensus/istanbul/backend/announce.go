// Copyright 2017 The Celo Authors
// This file is part of the celo library.
//
// The celo library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The celo library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the celo library. If not, see <http://www.gnu.org/licenses/>.

package backend

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/istanbul"
	vet "github.com/ethereum/go-ethereum/consensus/istanbul/backend/internal/enodes"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

// ==============================================
//
// define the constants and function for the sendAnnounce thread

const (
	queryEnodeGossipCooldownDuration = 5 * time.Minute
	// Schedule retries to be strictly later than the cooldown duration
	// that other nodes will impose for regossiping announces from this node.
	queryEnodeRetryDuration = queryEnodeGossipCooldownDuration + (30 * time.Second)

	signedAnnounceVersionGossipCooldownDuration = 5 * time.Minute
)

// The announceThread will:
// 1) Periodically poll to see if this node should be announcing
// 2) Periodically share the entire signed announce version table with all peers
// 3) Periodically prune announce-related data structures
// 4) Gossip announce messages when requested
// 5) Retry sending announce messages if they go unanswered
// 6) Update announce version when requested
func (sb *Backend) announceThread() {
	logger := sb.logger.New("func", "announceThread")

	sb.announceThreadWg.Add(1)
	defer sb.announceThreadWg.Done()

	// Create a ticker to poll if istanbul core is running and if this node is in
	// the validator conn set. If both conditions are true, then this node should announce.
	checkIfShouldAnnounceTicker := time.NewTicker(5 * time.Second)
	// TODO: this can be removed once we have more faith in this protocol
	updateAnnounceVersionTicker := time.NewTicker(5 * time.Minute)
	// Occasionally share the entire signed announce version table with all peers
	shareSignedAnnounceVersionTicker := time.NewTicker(5 * time.Minute)
	pruneAnnounceDataStructuresTicker := time.NewTicker(10 * time.Minute)

	// Periodically check to see if queryEnode Messages need to be sent
	queryEnodeTicker := time.NewTicker(6 * time.Minute)

	var queryEnodeRetryTimer *time.Timer
	var queryEnodeRetryTimerCh <-chan time.Time
	var announceVersion uint
	var announcing bool
	var shouldAnnounce bool
	var err error

	updateAnnounceVersionFunc := func() {
		version := getTimestamp()
		if version <= announceVersion {
			logger.Debug("Announce version is not newer than the existing version", "existing version", announceVersion, "attempted new version", version)
			return
		}
		if err := sb.setAndShareUpdatedAnnounceVersion(version); err != nil {
			logger.Warn("Error updating announce version", "err", err)
			return
		}
		announceVersion = version
	}

	for {
		select {
		case <-checkIfShouldAnnounceTicker.C:
			logger.Trace("Checking if this node should announce it's enode")

			shouldAnnounce, err = sb.shouldSaveAndPublishValEnodeURLs()
			if err != nil {
				logger.Warn("Error in checking if should announce", err)
				break
			}

			if shouldAnnounce && !announcing {
				updateAnnounceVersionFunc()
				// Gossip the announce after a minute.
				// The delay allows for all receivers of the announce message to
				// have a more up-to-date cached registered/elected valset, and
				// hence more likely that they will be aware that this node is
				// within that set.
				time.AfterFunc(1*time.Minute, func() {
					sb.startGossipQueryEnodeTask()
				})

				announcing = true
				logger.Trace("Enabled periodic gossiping of announce message")
			} else if !shouldAnnounce && announcing {
				if queryEnodeRetryTimer != nil {
					queryEnodeRetryTimer.Stop()
					queryEnodeRetryTimer = nil
					queryEnodeRetryTimerCh = nil
				}
				announcing = false
				logger.Trace("Disabled periodic gossiping of announce message")
			}

		case <-shareSignedAnnounceVersionTicker.C:
			// Send all signed announce versions to every peer. Only the entries
			// that are new to a node will end up being regossiped throughout the
			// network.
			allSignedAnnounceVersions, err := sb.getAllSignedAnnounceVersions()
			if err != nil {
				logger.Warn("Error getting all signed announce versions", "err", err)
				break
			}
			if err := sb.gossipSignedAnnounceVersionsMsg(allSignedAnnounceVersions); err != nil {
				logger.Warn("Error gossiping all signed announce versions")
			}

		case <-updateAnnounceVersionTicker.C:
			updateAnnounceVersionFunc()

		case <-queryEnodeRetryTimerCh: // If this is nil, this channel will never receive an event
			queryEnodeRetryTimer = nil
			queryEnodeRetryTimerCh = nil
			sb.startGossipQueryEnodeTask()

		case <-sb.generateAndGossipQueryEnodeCh:
			if shouldAnnounce {
				// This node may have recently sent out an announce message within
				// the gossip cooldown period imposed by other nodes.
				// Regardless, send the queryEnode so that it will at least be
				// processed by this node's peers. This is especially helpful when a network
				// is first starting up.
				hasContent, err := sb.generateAndGossipQueryEnode(announceVersion)
				if err != nil {
					logger.Warn("Error in generating and gossiping queryEnode", "err", err)
				}
				// If a retry hasn't been scheduled already by a previous announce,
				// schedule one.
				if hasContent && queryEnodeRetryTimer == nil {
					queryEnodeRetryTimer = time.NewTimer(queryEnodeRetryDuration)
					queryEnodeRetryTimerCh = queryEnodeRetryTimer.C
				}
			}

		case <-sb.updateAnnounceVersionCh:
			updateAnnounceVersionFunc()
			// Show that the announce update has been completed so we can rely on
			// it synchronously
			sb.updateAnnounceVersionCompleteCh <- struct{}{}

		case <-pruneAnnounceDataStructuresTicker.C:
			if err := sb.pruneAnnounceDataStructures(); err != nil {
				logger.Warn("Error in pruning announce data structures", "err", err)
			}

		case <-queryEnodeTicker.C:
			if shouldAnnounce {
				sb.startGossipQueryEnodeTask()
			}

		case <-sb.announceThreadQuit:
			checkIfShouldAnnounceTicker.Stop()
			pruneAnnounceDataStructuresTicker.Stop()
			if queryEnodeRetryTimer != nil {
				queryEnodeRetryTimer.Stop()
			}
			return
		}
	}
}

func (sb *Backend) shouldSaveAndPublishValEnodeURLs() (bool, error) {

	// Check if this node is in the validator connection set
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return false, err
	}

	return sb.coreStarted && validatorConnSet[sb.Address()], nil
}

// pruneAnnounceDataStructures will remove entries that are not in the validator connection set from all announce related data structures.
// The data structures that it prunes are:
// 1)  lastQueryEnodeGossiped
// 2)  valEnodeTable
// 3)  lastSignedAnnounceVersionsGossiped
// 4)  signedAnnounceVersionTable
func (sb *Backend) pruneAnnounceDataStructures() error {
	logger := sb.logger.New("func", "pruneAnnounceDataStructures")

	// retrieve the validator connection set
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return err
	}

	sb.lastQueryEnodeGossipedMu.Lock()
	for remoteAddress := range sb.lastQueryEnodeGossiped {
		if !validatorConnSet[remoteAddress] && time.Since(sb.lastQueryEnodeGossiped[remoteAddress]) >= queryEnodeGossipCooldownDuration {
			logger.Trace("Deleting entry from lastQueryEnodeGossiped", "address", remoteAddress, "gossip timestamp", sb.lastQueryEnodeGossiped[remoteAddress])
			delete(sb.lastQueryEnodeGossiped, remoteAddress)
		}
	}
	sb.lastQueryEnodeGossipedMu.Unlock()

	if err := sb.valEnodeTable.PruneEntries(validatorConnSet); err != nil {
		logger.Trace("Error in pruning valEnodeTable", "err", err)
		return err
	}

	sb.lastSignedAnnounceVersionsGossipedMu.Lock()
	for remoteAddress := range sb.lastSignedAnnounceVersionsGossiped {
		if !validatorConnSet[remoteAddress] && time.Since(sb.lastSignedAnnounceVersionsGossiped[remoteAddress]) >= signedAnnounceVersionGossipCooldownDuration {
			logger.Trace("Deleting entry from lastSignedAnnounceVersionsGossiped", "address", remoteAddress, "gossip timestamp", sb.lastSignedAnnounceVersionsGossiped[remoteAddress])
			delete(sb.lastSignedAnnounceVersionsGossiped, remoteAddress)
		}
	}
	sb.lastSignedAnnounceVersionsGossipedMu.Unlock()

	if err := sb.signedAnnounceVersionTable.Prune(validatorConnSet); err != nil {
		logger.Trace("Error in pruning signedAnnounceVersionTable", "err", err)
		return err
	}

	return nil
}

// ===============================================================
//
// define the IstanbulQueryEnode message format, the QueryEnodeMsgCache entries, the queryEnode send function (both the gossip version and the "retrieve from cache" version), and the announce get function

type encryptedEnodeURL struct {
	DestAddress       common.Address
	EncryptedEnodeURL []byte
}

func (ee *encryptedEnodeURL) String() string {
	return fmt.Sprintf("{DestAddress: %s, EncryptedEnodeURL length: %d}", ee.DestAddress.String(), len(ee.EncryptedEnodeURL))
}

type queryEnodeData struct {
	EncryptedEnodeURLs []*encryptedEnodeURL
	Version            uint
	// The timestamp of the node when the message is generated.
	// This results in a new hash for a newly generated message so it gets regossiped by other nodes
	Timestamp uint
}

func (qed *queryEnodeData) String() string {
	return fmt.Sprintf("{Version: %v, Timestamp: %v, EncryptedEnodeURLs: %v}", qed.Version, qed.Timestamp, qed.EncryptedEnodeURLs)
}

// ==============================================
//
// define the functions that needs to be provided for rlp Encoder/Decoder.

// EncodeRLP serializes ar into the Ethereum RLP format.
func (ee *encryptedEnodeURL) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{ee.DestAddress, ee.EncryptedEnodeURL})
}

// DecodeRLP implements rlp.Decoder, and load the ar fields from a RLP stream.
func (ee *encryptedEnodeURL) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		DestAddress       common.Address
		EncryptedEnodeURL []byte
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	ee.DestAddress, ee.EncryptedEnodeURL = msg.DestAddress, msg.EncryptedEnodeURL
	return nil
}

// EncodeRLP serializes ad into the Ethereum RLP format.
func (qed *queryEnodeData) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{qed.EncryptedEnodeURLs, qed.Version, qed.Timestamp})
}

// DecodeRLP implements rlp.Decoder, and load the ad fields from a RLP stream.
func (qed *queryEnodeData) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		EncryptedEnodeURLs []*encryptedEnodeURL
		Version            uint
		Timestamp          uint
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	qed.EncryptedEnodeURLs, qed.Version, qed.Timestamp = msg.EncryptedEnodeURLs, msg.Version, msg.Timestamp
	return nil
}

func (sb *Backend) startGossipQueryEnodeTask() {
	// sb.generateAndGossipQueryEnodeCh has a buffer of 1. If there is a value
	// already sent to the channel that has not been read from, don't block.
	select {
	case sb.generateAndGossipQueryEnodeCh <- struct{}{}:
	default:
	}
}

// generateAndGossipAnnounce will generate the lastest announce msg from this node
// and then broadcast it to it's peers, which should then gossip the announce msg
// message throughout the p2p network if there has not been a message sent from
// this node within the last announceGossipCooldownDuration.
// Returns if an announce message had content (ie not empty) and if there was an error.
func (sb *Backend) generateAndGossipQueryEnode(version uint) (bool, error) {
	logger := sb.logger.New("func", "generateAndGossipQueryEnode")
	logger.Trace("generateAndGossipQueryEnode called")
	istMsg, updatedValEnodeEntries, err := sb.generateQueryEnodeMsg(version)
	if err != nil {
		return false, err
	}

	if istMsg == nil {
		return false, nil
	}

	// Convert to payload
	payload, err := istMsg.Payload()
	if err != nil {
		logger.Error("Error in converting Istanbul QueryEnode Message to payload", "QueryEnodeMsg", istMsg.String(), "err", err)
		return true, err
	}

	if err := sb.Multicast(nil, payload, istanbulQueryEnodeMsg); err != nil {
		return true, err
	}

	sb.valEnodeTable.Upsert(updatedValEnodeEntries)

	return true, nil
}

// generateQueryEnodeMsg returns a queryEnode message from this node with a given version.
func (sb *Backend) generateQueryEnodeMsg(version uint) (*istanbul.Message, []*vet.AddressEntry, error) {
	logger := sb.logger.New("func", "generateQueryEnodeMsg")

	enodeURL, err := sb.getEnodeURL()
	if err != nil {
		logger.Error("Error getting enode URL", "err", err)
		return nil, nil, err
	}
	encryptedEnodeURLs, updatedValEnodeEntries, err := sb.generateEncryptedEnodeURLs(enodeURL)
	if err != nil {
		logger.Warn("Error generating encrypted enodeURLs", "err", err)
		return nil, nil, err
	}
	if len(encryptedEnodeURLs) == 0 {
		logger.Trace("No encrypted enodeURLs were generated, will not generate encryptedEnodeMsg")
		return nil, nil, nil
	}
	queryEnodeData := &queryEnodeData{
		EncryptedEnodeURLs: encryptedEnodeURLs,
		Version:            version,
		Timestamp:          getTimestamp(),
	}

	queryEnodeBytes, err := rlp.EncodeToBytes(queryEnodeData)
	if err != nil {
		logger.Error("Error encoding queryEnode content", "QueryEnodeData", queryEnodeData.String(), "err", err)
		return nil, nil, err
	}

	msg := &istanbul.Message{
		Code:      istanbulQueryEnodeMsg,
		Msg:       queryEnodeBytes,
		Address:   sb.Address(),
		Signature: []byte{},
	}

	// Sign the announce message
	if err := msg.Sign(sb.Sign); err != nil {
		logger.Error("Error in signing a QueryEnode Message", "QueryEnodeMsg", msg.String(), "err", err)
		return nil, nil, err
	}

	logger.Debug("Generated a queryEnode message", "IstanbulMsg", msg.String(), "QueryEnodeData", queryEnodeData.String())

	return msg, updatedValEnodeEntries, nil
}

// generateEncryptedEnodeURLs returns the encryptedEnodeURLs intended for validators
// whose entries in the val enode table do not exist or are outdated when compared
// to the signed announce version table.
func (sb *Backend) generateEncryptedEnodeURLs(enodeURL string) ([]*encryptedEnodeURL, []*vet.AddressEntry, error) {
	logger := sb.logger.New("func", "generateEncryptedEnodeURLs")
	valEnodeEntries, err := sb.valEnodeTable.GetAllValEnodes()
	if err != nil {
		return nil, nil, err
	}

	var encryptedEnodeURLs []*encryptedEnodeURL
	var updatedValEnodeEntries []*vet.AddressEntry
	for address, valEnodeEntry := range valEnodeEntries {
		// Don't generate an announce record for ourselves
		if address == sb.Address() {
			continue
		}

		if valEnodeEntry.Version == valEnodeEntry.HighestKnownVersion {
			continue
		}

		if valEnodeEntry.PublicKey == nil {
			logger.Warn("Cannot generate encrypted enode URL for a val enode entry without a PublicKey", "address", address)
			continue
		}

		if valEnodeEntry.NumQueryAttemptsForVersion > 1 {
			timeoutFactorPow := math.Min(float64(valEnodeEntry.NumQueryAttemptsForVersion), 5)
			timeoutMinutes := int64(math.Pow(2, timeoutFactorPow) * 5)
			timeoutForQuery := time.Duration(timeoutMinutes) * time.Minute

			if time.Since(*valEnodeEntry.LastQueryTimestamp) < timeoutForQuery {
				continue
			}
		}

		publicKey := ecies.ImportECDSAPublic(valEnodeEntry.PublicKey)
		encEnodeURL, err := ecies.Encrypt(rand.Reader, publicKey, []byte(enodeURL), nil, nil)
		if err != nil {
			return nil, nil, err
		}

		encryptedEnodeURLs = append(encryptedEnodeURLs, &encryptedEnodeURL{
			DestAddress:       address,
			EncryptedEnodeURL: encEnodeURL,
		})

		currentTime := time.Now()

		updatedValEnodeEntries = append(updatedValEnodeEntries, &vet.AddressEntry{
			Address:                    address,
			Version:                    valEnodeEntry.Version, // provide version to avoid a race condition by ensuring the query fields are meant for this version
			NumQueryAttemptsForVersion: valEnodeEntry.NumQueryAttemptsForVersion + 1,
			LastQueryTimestamp:         &currentTime,
		})
	}
	return encryptedEnodeURLs, updatedValEnodeEntries, nil
}

// This function will handle a queryEnode message.
func (sb *Backend) handleQueryEnodeMsg(peer consensus.Peer, payload []byte) error {
	logger := sb.logger.New("func", "handleQueryEnodeMsg")

	msg := new(istanbul.Message)

	// Decode message
	err := msg.FromPayload(payload, istanbul.GetSignatureAddress)
	if err != nil {
		logger.Error("Error in decoding received Istanbul Announce message", "err", err, "payload", hex.EncodeToString(payload))
		return err
	}
	logger.Trace("Handling an IstanbulAnnounce message", "from", msg.Address)

	// Check if the sender is within the validator connection set
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		logger.Trace("Error in retrieving validator connection set", "err", err)
		return err
	}

	if !validatorConnSet[msg.Address] {
		logger.Debug("Received a message from a validator not within the validator connection set. Ignoring it.", "sender", msg.Address)
		return errUnauthorizedAnnounceMessage
	}

	var qeData queryEnodeData
	err = rlp.DecodeBytes(msg.Msg, &qeData)
	if err != nil {
		logger.Warn("Error in decoding received Istanbul QueryEnode message content", "err", err, "IstanbulMsg", msg.String())
		return err
	}

	logger = logger.New("msgAddress", msg.Address, "msgVersion", qeData.Version)

	// Do some validation checks on the queryEnodeData
	if isValid, err := sb.validateQueryEnode(msg.Address, &qeData); !isValid || err != nil {
		logger.Warn("Validation of queryEnode message failed", "isValid", isValid, "err", err)
		return err
	}

	// If this is an elected or nearly elected validator and core is started, then process the queryEnode message
	shouldProcess, err := sb.shouldSaveAndPublishValEnodeURLs()
	if err != nil {
		logger.Warn("Error in checking if should process queryEnode", err)
	}

	if shouldProcess {
		logger.Trace("Processing an queryEnode message", "queryEnode records", qeData.EncryptedEnodeURLs)
		for _, encEnodeURL := range qeData.EncryptedEnodeURLs {
			// Only process an encEnodURL intended for this node
			if encEnodeURL.DestAddress != sb.Address() {
				continue
			}
			enodeBytes, err := sb.decryptFn(accounts.Account{Address: sb.Address()}, encEnodeURL.EncryptedEnodeURL, nil, nil)
			if err != nil {
				sb.logger.Warn("Error decrypting endpoint", "err", err, "encEnodeURL.EncryptedEnodeURL", encEnodeURL.EncryptedEnodeURL)
				return err
			}
			enodeURL := string(enodeBytes)
			node, err := enode.ParseV4(enodeURL)
			if err != nil {
				logger.Warn("Error parsing enodeURL", "enodeUrl", enodeURL)
				return err
			}

			// queryEnode messages should only be processed once because selfRecentMessages
			// will cache seen queryEnode messages, so it's safe to answer without any throttling
			if err := sb.answerQueryEnodeMsg(msg.Address, node, qeData.Version); err != nil {
				logger.Warn("Error answering an announce msg", "target node", node.URLv4(), "error", err)
				return err
			}

			break
		}
	}

	// Regossip this queryEnode message
	return sb.regossipQueryEnode(msg, qeData.Version, payload)
}

// answerQueryEnodeMsg will answer a received queryEnode message from an origin
// node. If the origin node is already a peer of any kind, an enodeCertificate will be sent.
// Regardless, the origin node will be upserted into the val enode table
// to ensure this node designates the origin node as a ValidatorPurpose peer.
func (sb *Backend) answerQueryEnodeMsg(address common.Address, node *enode.Node, version uint) error {
	targetIDs := map[enode.ID]bool{
		node.ID(): true,
	}
	// The target could be an existing peer of any purpose.
	matches := sb.broadcaster.FindPeers(targetIDs, p2p.AnyPurpose)
	if matches[node.ID()] != nil {
		enodeCertificateMsg, err := sb.retrieveEnodeCertificateMsg()
		if err != nil {
			return err
		}
		if err := sb.sendEnodeCertificateMsg(matches[node.ID()], enodeCertificateMsg); err != nil {
			return err
		}
	}
	// Upsert regardless to account for the case that the target is a non-ValidatorPurpose
	// peer but should be.
	// If the target is not a peer and should be a ValidatorPurpose peer, this
	// will designate the target as a ValidatorPurpose peer and send an enodeCertificate
	// during the istanbul handshake.
	if err := sb.valEnodeTable.Upsert([]*vet.AddressEntry{{Address: address, Node: node, Version: version}}); err != nil {
		return err
	}
	return nil
}

// validateQueryEnode will do some validation to check the contents of the queryEnode
// message. This is to force all validators that send a queryEnode message to
// create as succint message as possible, and prevent any possible network DOS attacks
// via extremely large queryEnode message.
func (sb *Backend) validateQueryEnode(msgAddress common.Address, qeData *queryEnodeData) (bool, error) {
	logger := sb.logger.New("func", "validateQueryEnode", "msg address", msgAddress)

	// Check if there are any duplicates in the queryEnode message
	var encounteredAddresses = make(map[common.Address]bool)
	for _, encEnodeURL := range qeData.EncryptedEnodeURLs {
		if encounteredAddresses[encEnodeURL.DestAddress] {
			logger.Info("QueryEnode message has duplicate entries", "address", encEnodeURL.DestAddress)
			return false, nil
		}

		encounteredAddresses[encEnodeURL.DestAddress] = true
	}

	// Check if the number of rows in the queryEnodePayload is at most 2 times the size of the current validator connection set.
	// Note that this is a heuristic of the actual size of validator connection set at the time the validator constructed the announce message.
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return false, err
	}

	if len(qeData.EncryptedEnodeURLs) > 2*len(validatorConnSet) {
		logger.Info("Number of queryEnode message encrypted enodes is more than two times the size of the current validator connection set", "num queryEnode enodes", len(qeData.EncryptedEnodeURLs), "reg/elected val set size", len(validatorConnSet))
		return false, err
	}

	return true, nil
}

// regossipQueryEnode will regossip a received querEnode message.
// If this node regossiped a queryEnode from the same source address within the last
// 5 minutes, then it won't regossip. This is to prevent a malicious validator from
// DOS'ing the network with very frequent announce messages.
// This opens an attack vector where any malicious node could continue to gossip
// a previously gossiped announce message from any validator, causing other nodes to regossip and
// enforce the cooldown period for future messages originating from the origin validator.
// This is circumvented by caching the hashes of messages that are regossiped
// with sb.selfRecentMessages to prevent future regossips.
func (sb *Backend) regossipQueryEnode(msg *istanbul.Message, msgTimestamp uint, payload []byte) error {
	logger := sb.logger.New("func", "regossipQueryEnode", "queryEnodeSourceAddress", msg.Address, "msgTimestamp", msgTimestamp)

	sb.lastQueryEnodeGossipedMu.RLock()
	if lastGossiped, ok := sb.lastQueryEnodeGossiped[msg.Address]; ok {
		if time.Since(lastGossiped) < queryEnodeGossipCooldownDuration {
			sb.lastQueryEnodeGossipedMu.RUnlock()
			logger.Trace("Already regossiped msg from this source address within the cooldown period, not regossiping.")
			return nil
		}
	}
	sb.lastQueryEnodeGossipedMu.RUnlock()

	logger.Trace("Regossiping the istanbul queryEnode message", "IstanbulMsg", msg.String())
	if err := sb.Multicast(nil, payload, istanbulQueryEnodeMsg); err != nil {
		return err
	}

	sb.lastQueryEnodeGossiped[msg.Address] = time.Now()

	return nil
}

// Used as a salt when signing signedAnnounceVersion. This is to account for
// the unlikely case where a different signed struct with the same field types
// is used elsewhere and shared with other nodes. If that were to happen, a
// malicious node could try sending the other struct where this struct is used,
// or vice versa. This ensures that the signature is only valid for this struct.
var signedAnnounceVersionSalt = []byte("signedAnnounceVersion")

// signedAnnounceVersion is a signed message from a validator indicating the most
// recent version of its enode.
type signedAnnounceVersion vet.SignedAnnounceVersionEntry

func newSignedAnnounceVersionFromEntry(entry *vet.SignedAnnounceVersionEntry) *signedAnnounceVersion {
	return &signedAnnounceVersion{
		Address:   entry.Address,
		PublicKey: entry.PublicKey,
		Version:   entry.Version,
		Signature: entry.Signature,
	}
}

func (sav *signedAnnounceVersion) Sign(signingFn func(data []byte) ([]byte, error)) error {
	payloadToSign, err := sav.payloadToSign()
	if err != nil {
		return err
	}
	sav.Signature, err = signingFn(payloadToSign)
	if err != nil {
		return err
	}
	return nil
}

// RecoverPublicKeyAndAddress recovers the ECDSA public key and corresponding
// address from the Signature
func (sav *signedAnnounceVersion) RecoverPublicKeyAndAddress() error {
	payloadToSign, err := sav.payloadToSign()
	if err != nil {
		return err
	}
	payloadHash := crypto.Keccak256(payloadToSign)
	publicKey, err := crypto.SigToPub(payloadHash, sav.Signature)
	if err != nil {
		return err
	}
	address, err := crypto.PubkeyToAddress(*publicKey), nil
	if err != nil {
		return err
	}
	sav.PublicKey = publicKey
	sav.Address = address
	return nil
}

// EncodeRLP serializes signedAnnounceVersion into the Ethereum RLP format.
// Only the Version and Signature are encoded, as the public key and address
// can be recovered from the Signature using RecoverPublicKeyAndAddress
func (sav *signedAnnounceVersion) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{sav.Version, sav.Signature})
}

// DecodeRLP implements rlp.Decoder, and load the signedAnnounceVersion fields from a RLP stream.
// Only the Version and Signature are encoded/decoded, as the public key and address
// can be recovered from the Signature using RecoverPublicKeyAndAddress
func (sav *signedAnnounceVersion) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		Version   uint
		Signature []byte
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	sav.Version, sav.Signature = msg.Version, msg.Signature
	return nil
}

func (sav *signedAnnounceVersion) Entry() *vet.SignedAnnounceVersionEntry {
	return &vet.SignedAnnounceVersionEntry{
		Address:   sav.Address,
		PublicKey: sav.PublicKey,
		Version:   sav.Version,
		Signature: sav.Signature,
	}
}

func (sav *signedAnnounceVersion) payloadToSign() ([]byte, error) {
	signedContent := []interface{}{signedAnnounceVersionSalt, sav.Version}
	payload, err := rlp.EncodeToBytes(signedContent)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func (sb *Backend) generateSignedAnnounceVersion(version uint) (*signedAnnounceVersion, error) {
	sav := &signedAnnounceVersion{
		Address:   sb.Address(),
		PublicKey: sb.publicKey,
		Version:   version,
	}
	err := sav.Sign(sb.Sign)
	if err != nil {
		return nil, err
	}
	return sav, nil
}

func (sb *Backend) gossipSignedAnnounceVersionsMsg(signedAnnVersions []*signedAnnounceVersion) error {
	logger := sb.logger.New("func", "gossipSignedAnnounceVersionsMsg")

	payload, err := rlp.EncodeToBytes(signedAnnVersions)
	if err != nil {
		logger.Warn("Error encoding entries", "err", err)
		return err
	}
	return sb.Multicast(nil, payload, istanbulSignedAnnounceVersionsMsg)
}

func (sb *Backend) getAllSignedAnnounceVersions() ([]*signedAnnounceVersion, error) {
	allEntries, err := sb.signedAnnounceVersionTable.GetAll()
	if err != nil {
		return nil, err
	}
	allSignedAnnounceVersions := make([]*signedAnnounceVersion, len(allEntries))
	for i, entry := range allEntries {
		allSignedAnnounceVersions[i] = newSignedAnnounceVersionFromEntry(entry)
	}
	return allSignedAnnounceVersions, nil
}

// sendAnnounceVersionTable sends all SignedAnnounceVersions this node
// has to a peer
func (sb *Backend) sendAnnounceVersionTable(peer consensus.Peer) error {
	logger := sb.logger.New("func", "sendAnnounceVersionTable")
	allSignedAnnounceVersions, err := sb.getAllSignedAnnounceVersions()
	if err != nil {
		logger.Warn("Error getting all signed announce versions", "err", err)
		return err
	}
	payload, err := rlp.EncodeToBytes(allSignedAnnounceVersions)
	if err != nil {
		logger.Warn("Error encoding entries", "err", err)
		return err
	}
	return peer.Send(istanbulSignedAnnounceVersionsMsg, payload)
}

func (sb *Backend) handleSignedAnnounceVersionsMsg(peer consensus.Peer, payload []byte) error {
	logger := sb.logger.New("func", "handleSignedAnnounceVersionsMsg")
	logger.Trace("Handling signed announce version msg")
	var signedAnnVersions []*signedAnnounceVersion

	err := rlp.DecodeBytes(payload, &signedAnnVersions)
	if err != nil {
		logger.Warn("Error in decoding received Signed Announce Versions msg", "err", err)
		return err
	}

	// If the announce's valAddress is not within the validator connection set, then ignore it
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		logger.Trace("Error in retrieving validator conn set", "err", err)
		return err
	}

	var validEntries []*vet.SignedAnnounceVersionEntry
	validAddresses := make(map[common.Address]bool)
	// Verify all entries are valid and remove duplicates
	for _, signedAnnVersion := range signedAnnVersions {
		// The public key and address are not RLP encoded/decoded and must be
		// explicitly recovered.
		if err := signedAnnVersion.RecoverPublicKeyAndAddress(); err != nil {
			logger.Warn("Error recovering signed announce version public key and address from signature", "err", err)
			continue
		}
		if !validatorConnSet[signedAnnVersion.Address] {
			logger.Debug("Found signed announce version from an address not in the validator conn set", "address", signedAnnVersion.Address)
			continue
		}
		if _, ok := validAddresses[signedAnnVersion.Address]; ok {
			logger.Debug("Found duplicate signed announce version in message", "address", signedAnnVersion.Address)
			continue
		}
		validAddresses[signedAnnVersion.Address] = true
		validEntries = append(validEntries, signedAnnVersion.Entry())
	}
	if err := sb.upsertAndGossipSignedAnnounceVersionEntries(validEntries); err != nil {
		logger.Warn("Error upserting and gossiping entries", "err", err)
		return err
	}
	// If this node is a validator (checked later as a result of this call) and it receives a signed annouunce
	// version from a remote validator that is newer than the remote validator's
	// version in the val enode table, this node did not receive a direct announce
	// and needs to announce its own enode to the remote validator.
	sb.startGossipQueryEnodeTask()
	return nil
}

func (sb *Backend) upsertAndGossipSignedAnnounceVersionEntries(entries []*vet.SignedAnnounceVersionEntry) error {
	logger := sb.logger.New("func", "upsertAndGossipSignedAnnounceVersionEntries")

	shouldProcess, err := sb.shouldSaveAndPublishValEnodeURLs()
	if err != nil {
		logger.Warn("Error in checking if should process queryEnode", err)
	}
	if shouldProcess {
		// Update entries in val enode db
		var valEnodeEntries []*vet.AddressEntry
		for _, entry := range entries {
			// Don't add ourselves into the val enode table
			if entry.Address == sb.Address() {
				continue
			}
			// Update the HighestKnownVersion for this address. Upsert will
			// only update this entry if the HighestKnownVersion is greater
			// than the existing one.
			// Also store the PublicKey for future encryption in queryEnode msgs
			valEnodeEntries = append(valEnodeEntries, &vet.AddressEntry{
				Address: entry.Address,
				PublicKey: entry.PublicKey,
				HighestKnownVersion: entry.Version,
			})
		}
		if err := sb.valEnodeTable.Upsert(valEnodeEntries); err != nil {
			logger.Warn("Error upserting val enode table entries", "err", err)
		}
	}

	newEntries, err := sb.signedAnnounceVersionTable.Upsert(entries)
	if err != nil {
		logger.Warn("Error upserting signed announce version table entries", "err", err)
	}

	// Only regossip entries that do not originate from an address that we have
	// gossiped a signed announce version for within the last 5 minutes, excluding
	// our own address.
	var signedAnnVersionsToRegossip []*signedAnnounceVersion
	sb.lastSignedAnnounceVersionsGossipedMu.Lock()
	for _, entry := range newEntries {
		lastGossipTime, ok := sb.lastSignedAnnounceVersionsGossiped[entry.Address]
		if ok && time.Since(lastGossipTime) >= signedAnnounceVersionGossipCooldownDuration && entry.Address != sb.ValidatorAddress() {
			continue
		}
		signedAnnVersionsToRegossip = append(signedAnnVersionsToRegossip, newSignedAnnounceVersionFromEntry(entry))
		sb.lastSignedAnnounceVersionsGossiped[entry.Address] = time.Now()
	}
	sb.lastSignedAnnounceVersionsGossipedMu.Unlock()
	if len(signedAnnVersionsToRegossip) > 0 {
		return sb.gossipSignedAnnounceVersionsMsg(signedAnnVersionsToRegossip)
	}
	return nil
}

// updateAnnounceVersion will synchronously update the announce version.
// Must be called in a separate goroutine from the announceThread to avoid
// a deadlock.
func (sb *Backend) updateAnnounceVersion() {
	sb.updateAnnounceVersionCh <- struct{}{}
	<-sb.updateAnnounceVersionCompleteCh
}

// setAndShareUpdatedAnnounceVersion generates announce data structures and
// and shares them with relevant nodes.
// It will:
//  1) Generate a new enode certificate
//  2) Send the new enode certificate to this node's proxy if one exists
//  3) Send the new enode certificate to all peers in the validator conn set
//  4) Generate a new signed announce version
//  5) Gossip the new signed announce version to all peers
func (sb *Backend) setAndShareUpdatedAnnounceVersion(version uint) error {
	logger := sb.logger.New("func", "setAndShareUpdatedAnnounceVersion")
	// Send new versioned enode msg to all other registered or elected validators
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return err
	}
	enodeCertificateMsg, err := sb.generateEnodeCertificateMsg(version)
	if err != nil {
		return err
	}
	sb.setEnodeCertificateMsg(enodeCertificateMsg)
	// Send the new versioned enode msg to the proxy peer
	if sb.config.Proxied && sb.proxyNode != nil && sb.proxyNode.peer != nil {
		err := sb.sendEnodeCertificateMsg(sb.proxyNode.peer, enodeCertificateMsg)
		if err != nil {
			logger.Error("Error in sending versioned enode msg to proxy", "err", err)
			return err
		}
	}
	// Don't send any of the following messages if this node is not in the validator conn set
	if !validatorConnSet[sb.Address()] {
		logger.Trace("Not in the validator conn set, not updating announce version")
		return nil
	}
	payload, err := enodeCertificateMsg.Payload()
	if err != nil {
		return err
	}
	destAddresses := make([]common.Address, len(validatorConnSet))
	i := 0
	for address := range validatorConnSet {
		destAddresses[i] = address
		i++
	}
	err = sb.Multicast(destAddresses, payload, istanbulEnodeCertificateMsg)
	if err != nil {
		return err
	}

	// Generate and gossip a new signed announce version
	newSignedAnnVersion, err := sb.generateSignedAnnounceVersion(version)
	if err != nil {
		return err
	}
	return sb.upsertAndGossipSignedAnnounceVersionEntries([]*vet.SignedAnnounceVersionEntry{
		newSignedAnnVersion.Entry(),
	})
}

func (sb *Backend) getEnodeURL() (string, error) {
	if sb.config.Proxied {
		if sb.proxyNode != nil {
			return sb.proxyNode.externalNode.URLv4(), nil
		}
		return "", errNoProxyConnection
	}
	return sb.p2pserver.Self().URLv4(), nil
}

func getTimestamp() uint {
	// Unix() returns a int64, but we need a uint for the golang rlp encoding implmentation. Warning: This timestamp value will be truncated in 2106.
	return uint(time.Now().Unix())
}

type enodeCertificate struct {
	EnodeURL string
	Version  uint
}

// ==============================================
//
// define the functions that needs to be provided for rlp Encoder/Decoder.

// EncodeRLP serializes ec into the Ethereum RLP format.
func (ec *enodeCertificate) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{ec.EnodeURL, ec.Version})
}

// DecodeRLP implements rlp.Decoder, and load the ec fields from a RLP stream.
func (ec *enodeCertificate) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		EnodeURL string
		Version  uint
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	ec.EnodeURL, ec.Version = msg.EnodeURL, msg.Version
	return nil
}

// retrieveEnodeCertificateMsg gets the most recent enode certificate message.
// May be nil if no message was generated as a result of the core not being
// started, or if a proxy has not received a message from its proxied validator
func (sb *Backend) retrieveEnodeCertificateMsg() (*istanbul.Message, error) {
	sb.enodeCertificateMsgMu.Lock()
	defer sb.enodeCertificateMsgMu.Unlock()
	if sb.enodeCertificateMsg == nil {
		return nil, nil
	}
	return sb.enodeCertificateMsg.Copy(), nil
}

// generateEnodeCertificateMsg generates an enode certificate message with the enode
// this node is publicly accessible at. If this node is proxied, the proxy's
// public enode is used.
func (sb *Backend) generateEnodeCertificateMsg(version uint) (*istanbul.Message, error) {
	logger := sb.logger.New("func", "generateEnodeCertificateMsg")

	var enodeURL string
	if sb.config.Proxied {
		if sb.proxyNode != nil {
			enodeURL = sb.proxyNode.externalNode.URLv4()
		} else {
			return nil, errNoProxyConnection
		}
	} else {
		enodeURL = sb.p2pserver.Self().URLv4()
	}

	enodeCertificate := &enodeCertificate{
		EnodeURL: enodeURL,
		Version:  version,
	}
	enodeCertificateBytes, err := rlp.EncodeToBytes(enodeCertificate)
	if err != nil {
		return nil, err
	}
	msg := &istanbul.Message{
		Code:    istanbulEnodeCertificateMsg,
		Address: sb.Address(),
		Msg:     enodeCertificateBytes,
	}
	// Sign the message
	if err := msg.Sign(sb.Sign); err != nil {
		return nil, err
	}
	logger.Trace("Generated Istanbul Enode Certificate message", "enodeCertificate", enodeCertificate, "address", msg.Address)
	return msg, nil
}

// handleEnodeCertificateMsg handles an enode certificate message.
// If this node is a proxy and the enode certificate is from a remote validator
// (ie not the proxied validator), this node will forward the enode certificate
// to its proxied validator. If the proxied validator decides this node should process
// the enode certificate and upsert it into its val enode table, the proxied validator
// will send it back to this node.
// If the proxied validator sends an enode certificate for itself to this node,
// this node will set the enode certificate as its own for handshaking.
func (sb *Backend) handleEnodeCertificateMsg(peer consensus.Peer, payload []byte) error {
	logger := sb.logger.New("func", "handleEnodeCertificateMsg")

	var msg istanbul.Message
	// Decode payload into msg
	err := msg.FromPayload(payload, istanbul.GetSignatureAddress)
	if err != nil {
		logger.Error("Error in decoding received Istanbul Enode Certificate message", "err", err, "payload", hex.EncodeToString(payload))
		return err
	}
	logger = logger.New("msg address", msg.Address)

	var enodeCertificate enodeCertificate
	if err := rlp.DecodeBytes(msg.Msg, &enodeCertificate); err != nil {
		logger.Warn("Error in decoding received Istanbul Enode Certificate message content", "err", err, "IstanbulMsg", msg.String())
		return err
	}
	logger.Trace("Received Istanbul Enode Certificate message", "enodeCertificate", enodeCertificate)

	parsedNode, err := enode.ParseV4(enodeCertificate.EnodeURL)
	if err != nil {
		logger.Warn("Malformed v4 node in received Istanbul Enode Certificate message", "enodeCertificate", enodeCertificate, "err", err)
		return err
	}

	if sb.config.Proxy && sb.proxiedPeer != nil {
		if sb.proxiedPeer.Node().ID() == peer.Node().ID() {
			// if this message is from the proxied peer and contains the proxied
			// validator's enodeCertificate, save it for handshake use
			if msg.Address == sb.config.ProxiedValidatorAddress {
				existingVersion := sb.getEnodeCertificateMsgVersion()
				if enodeCertificate.Version < existingVersion {
					logger.Warn("Enode certificate from proxied peer contains version lower than existing enode msg", "msg version", enodeCertificate.Version, "existing", existingVersion)
					return errors.New("Version too low")
				}
				// There may be a difference in the URLv4 string because of `discport`,
				// so instead compare the ID
				selfNode := sb.p2pserver.Self()
				if parsedNode.ID() != selfNode.ID() {
					logger.Warn("Received Istanbul Enode Certificate message with an incorrect enode url", "message enode url", enodeCertificate.EnodeURL, "self enode url", sb.p2pserver.Self().URLv4())
					return errors.New("Incorrect enode url")
				}
				if err := sb.setEnodeCertificateMsg(&msg); err != nil {
					logger.Warn("Error setting enode certificate msg", "err", err)
					return err
				}
				return nil
			}
		} else {
			// If this message is not from the proxied validator, send it to the
			// proxied validator without upserting it in this node. If the validator
			// decides this proxy should upsert the enodeCertificate, then it
			// will send it back to this node.
			if err := sb.sendEnodeCertificateMsg(sb.proxiedPeer, &msg); err != nil {
				logger.Warn("Error forwarding enodeCertificate to proxied validator", "err", err)
			}
			return nil
		}
	}

	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		logger.Debug("Error in retrieving registered/elected valset", "err", err)
		return err
	}

	if !validatorConnSet[msg.Address] {
		logger.Debug("Received Istanbul Enode Certificate message originating from a node not in the validator conn set")
		return errUnauthorizedAnnounceMessage
	}

	if err := sb.valEnodeTable.Upsert([]*vet.AddressEntry{{Address: msg.Address, Node: parsedNode, Version: enodeCertificate.Version}}); err != nil {
		logger.Warn("Error in upserting a val enode table entry", "error", err)
		return err
	}
	return nil
}

func (sb *Backend) sendEnodeCertificateMsg(peer consensus.Peer, msg *istanbul.Message) error {
	logger := sb.logger.New("func", "sendEnodeCertificateMsg")
	payload, err := msg.Payload()
	if err != nil {
		logger.Error("Error getting payload of enode certificate message", "err", err)
		return err
	}
	return peer.Send(istanbulEnodeCertificateMsg, payload)
}

func (sb *Backend) setEnodeCertificateMsg(msg *istanbul.Message) error {
	sb.enodeCertificateMsgMu.Lock()
	var enodeCertificate enodeCertificate
	if err := rlp.DecodeBytes(msg.Msg, &enodeCertificate); err != nil {
		return err
	}
	sb.enodeCertificateMsg = msg
	sb.enodeCertificateMsgVersion = enodeCertificate.Version
	sb.enodeCertificateMsgMu.Unlock()
	return nil
}

func (sb *Backend) getEnodeCertificateMsgVersion() uint {
	sb.enodeCertificateMsgMu.RLock()
	defer sb.enodeCertificateMsgMu.RUnlock()
	return sb.enodeCertificateMsgVersion
}
