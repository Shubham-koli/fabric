/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package tests

import (
	"fmt"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/ledger/rwset"
	"github.com/hyperledger/fabric-protos-go/msp"
	protopeer "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/cauthdsl"
	configtxtest "github.com/hyperledger/fabric/common/configtx/test"
	"github.com/hyperledger/fabric/common/crypto"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/metrics/disabled"
	"github.com/hyperledger/fabric/core/ledger/kvledger/tests/fakes"
	lutils "github.com/hyperledger/fabric/core/ledger/util"
	"github.com/hyperledger/fabric/core/ledger/util/couchdb"
	"github.com/hyperledger/fabric/integration/runner"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/stretchr/testify/require"
)

var logger = flogging.MustGetLogger("test2")

// collConf helps writing tests with less verbose code by specifying coll configuration
// in a simple struct in place of 'common.CollectionConfigPackage'. (the test heplers' apis
// use 'collConf' as parameters and return values and transform back and forth to/from proto
// message internally (using func 'convertToCollConfigProtoBytes' and 'convertFromCollConfigProto')
type collConf struct {
	name    string
	btl     uint64
	members []string
}

type txAndPvtdata struct {
	Txid     string
	Envelope *common.Envelope
	Pvtws    *rwset.TxPvtReadWriteSet
}

//go:generate counterfeiter -o fakes/signer.go --fake-name Signer . signer

type signer interface {
	Sign(msg []byte) ([]byte, error)
	Serialize() ([]byte, error)
}

func convertToCollConfigProtoBytes(collConfs []*collConf) ([]byte, error) {
	var protoConfArray []*common.CollectionConfig
	for _, c := range collConfs {
		protoConf := &common.CollectionConfig{
			Payload: &common.CollectionConfig_StaticCollectionConfig{
				StaticCollectionConfig: &common.StaticCollectionConfig{
					Name:             c.name,
					BlockToLive:      c.btl,
					MemberOrgsPolicy: convertToMemberOrgsPolicy(c.members),
				},
			},
		}
		protoConfArray = append(protoConfArray, protoConf)
	}
	return proto.Marshal(&common.CollectionConfigPackage{Config: protoConfArray})
}

func convertToMemberOrgsPolicy(members []string) *common.CollectionPolicyConfig {
	var data [][]byte
	for _, member := range members {
		data = append(data, []byte(member))
	}
	return &common.CollectionPolicyConfig{
		Payload: &common.CollectionPolicyConfig_SignaturePolicy{
			SignaturePolicy: cauthdsl.Envelope(cauthdsl.Or(cauthdsl.SignedBy(0), cauthdsl.SignedBy(1)), data),
		},
	}
}

func convertFromMemberOrgsPolicy(policy *common.CollectionPolicyConfig) []string {
	if policy.GetSignaturePolicy() == nil {
		return nil
	}
	ids := policy.GetSignaturePolicy().Identities
	var members []string
	for _, id := range ids {
		role := &msp.MSPRole{}
		err := proto.Unmarshal(id.Principal, role)
		if err == nil {
			// This is for sample ledger generated by fabric (e.g., integration test),
			// where id.Principal was properly marshalled during sample ledger generation.
			members = append(members, role.MspIdentifier)
		} else {
			// This is for sample ledger generated by sampleDataHelper.populateLedger,
			// where id.Principal was a []byte cast from a string (not a marshalled msp.MSPRole)
			members = append(members, string(id.Principal))
		}
	}
	return members
}

func convertFromCollConfigProto(collConfPkg *common.CollectionConfigPackage) []*collConf {
	var collConfs []*collConf
	protoConfArray := collConfPkg.Config
	for _, protoConf := range protoConfArray {
		p := protoConf.GetStaticCollectionConfig()
		collConfs = append(collConfs,
			&collConf{
				name:    p.Name,
				btl:     p.BlockToLive,
				members: convertFromMemberOrgsPolicy(p.MemberOrgsPolicy),
			},
		)
	}
	return collConfs
}

func constructTransaction(txid string, simulationResults []byte) (*common.Envelope, error) {
	channelid := "dummyChannel"
	ccid := &protopeer.ChaincodeID{
		Name:    "dummyCC",
		Version: "dummyVer",
	}
	txenv, _, err := constructUnsignedTxEnv(
		channelid,
		ccid,
		&protopeer.Response{Status: 200},
		simulationResults,
		txid,
		nil,
		nil,
		common.HeaderType_ENDORSER_TRANSACTION,
	)
	return txenv, err
}

// constructUnsignedTxEnv creates a Transaction envelope from given inputs
func constructUnsignedTxEnv(
	channelID string,
	ccid *protopeer.ChaincodeID,
	response *protopeer.Response,
	simulationResults []byte,
	txid string,
	events []byte,
	visibility []byte,
	headerType common.HeaderType,
) (*common.Envelope, string, error) {

	sigID := &fakes.Signer{}
	sigID.SerializeReturns([]byte("signer"), nil)
	sigID.SignReturns([]byte("signature"), nil)

	ss, err := sigID.Serialize()
	if err != nil {
		return nil, "", err
	}

	var prop *protopeer.Proposal
	if txid == "" {
		// if txid is not set, then we need to generate one while creating the proposal message
		prop, txid, err = protoutil.CreateChaincodeProposal(
			headerType,
			channelID,
			&protopeer.ChaincodeInvocationSpec{
				ChaincodeSpec: &protopeer.ChaincodeSpec{
					ChaincodeId: ccid,
				},
			},
			ss,
		)

	} else {
		// if txid is set, we should not generate a txid instead reuse the given txid
		nonce, err := crypto.GetRandomNonce()
		if err != nil {
			return nil, "", err
		}
		prop, txid, err = protoutil.CreateChaincodeProposalWithTxIDNonceAndTransient(
			txid,
			headerType,
			channelID,
			&protopeer.ChaincodeInvocationSpec{
				ChaincodeSpec: &protopeer.ChaincodeSpec{
					ChaincodeId: ccid,
				},
			},
			nonce,
			ss,
			nil,
		)
	}
	if err != nil {
		return nil, "", err
	}

	presp, err := protoutil.CreateProposalResponse(
		prop.Header,
		prop.Payload,
		response,
		simulationResults,
		nil,
		ccid,
		sigID,
	)
	if err != nil {
		return nil, "", err
	}

	env, err := protoutil.CreateSignedTx(prop, sigID, presp)
	if err != nil {
		return nil, "", err
	}
	return env, txid, nil
}

func constructTestGenesisBlock(channelid string) (*common.Block, error) {
	blk, err := configtxtest.MakeGenesisBlock(channelid)
	if err != nil {
		return nil, err
	}
	setBlockFlagsToValid(blk)
	return blk, nil
}

func setBlockFlagsToValid(block *common.Block) {
	protoutil.InitBlockMetadata(block)
	block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] =
		lutils.NewTxValidationFlagsSetValue(len(block.Data.Data), protopeer.TxValidationCode_VALID)
}

func couchDBSetup(t *testing.T, couchdbMountDir string, localdHostDir string) (addr string, cleanup func()) {
	couchDB := &runner.CouchDB{
		Name: "ledger13_upgrade_test",
		Binds: []string{
			fmt.Sprintf("%s:%s", couchdbMountDir, "/opt/couchdb/data"),
			fmt.Sprintf("%s:%s", localdHostDir, "/opt/couchdb/etc/local.d"),
		},
	}
	err := couchDB.Start()
	require.NoError(t, err)
	return couchDB.Address(), func() {
		couchDB.Stop()
	}
}

func dropCouchDBs(t *testing.T, couchdbConfig *couchdb.Config) {
	couchInstance, err := couchdb.CreateCouchInstance(couchdbConfig, &disabled.Provider{})
	require.NoError(t, err)
	dbNames, err := couchInstance.RetrieveApplicationDBNames()
	require.NoError(t, err)
	for _, dbName := range dbNames {
		db := &couchdb.CouchDatabase{
			CouchInstance: couchInstance,
			DBName:        dbName,
		}
		response, err := db.DropDatabase()
		require.NoError(t, err)
		require.True(t, response.Ok)
	}
}
