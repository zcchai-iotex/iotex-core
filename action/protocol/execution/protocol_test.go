// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package execution

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/golang/mock/gomock"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/action/protocol/account"
	accountutil "github.com/iotexproject/iotex-core/action/protocol/account/util"
	"github.com/iotexproject/iotex-core/action/protocol/execution/evm"
	"github.com/iotexproject/iotex-core/action/protocol/rolldpos"
	"github.com/iotexproject/iotex-core/action/protocol/vote"
	"github.com/iotexproject/iotex-core/address"
	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/blockchain/genesis"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/pkg/hash"
	"github.com/iotexproject/iotex-core/pkg/keypair"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/pkg/unit"
	"github.com/iotexproject/iotex-core/test/mock/mock_blockchain"
	"github.com/iotexproject/iotex-core/test/testaddress"
	"github.com/iotexproject/iotex-core/testutil"
)

// ExpectedBalance defines an account-balance pair
type ExpectedBalance struct {
	Account    string `json:"account"`
	RawBalance string `json:"rawBalance"`
}

func (eb *ExpectedBalance) Balance() *big.Int {
	balance, ok := new(big.Int).SetString(eb.RawBalance, 10)
	if !ok {
		log.L().Panic("invalid balance", zap.String("balance", eb.RawBalance))
	}

	return balance
}

type Log struct {
	Topics []string `json:"topics"`
	Data   string   `json:"data"`
}

type ExecutionConfig struct {
	Comment                 string            `json:"comment"`
	ContractIndex           int               `json:"contractIndex"`
	AppendContractAddress   bool              `json:"appendContractAddress"`
	ContractIndexToAppend   int               `json:"contractIndexToAppend"`
	ContractAddressToAppend string            `json:"contractAddressToAppend"`
	ReadOnly                bool              `json:"readOnly"`
	RawPrivateKey           string            `json:"rawPrivateKey"`
	RawByteCode             string            `json:"rawByteCode"`
	RawAmount               string            `json:"rawAmount"`
	RawGasLimit             uint              `json:"rawGasLimit"`
	RawGasPrice             string            `json:"rawGasPrice"`
	Failed                  bool              `json:"failed"`
	RawReturnValue          string            `json:"rawReturnValue"`
	RawExpectedGasConsumed  uint              `json:"rawExpectedGasConsumed"`
	ExpectedBalances        []ExpectedBalance `json:"expectedBalances"`
	ExpectedLogs            []Log             `json:"expectedLogs"`
}

func (cfg *ExecutionConfig) PrivateKey() keypair.PrivateKey {
	priKey, err := keypair.HexStringToPrivateKey(cfg.RawPrivateKey)
	if err != nil {
		log.L().Panic(
			"invalid private key",
			zap.String("privateKey", cfg.RawPrivateKey),
			zap.Error(err),
		)
	}

	return priKey
}

func (cfg *ExecutionConfig) Executor() address.Address {
	priKey := cfg.PrivateKey()
	addr, err := address.FromBytes(priKey.PublicKey().Hash())
	if err != nil {
		log.L().Panic(
			"invalid private key",
			zap.String("privateKey", cfg.RawPrivateKey),
			zap.Error(err),
		)
	}

	return addr
}

func (cfg *ExecutionConfig) ByteCode() []byte {
	byteCode, err := hex.DecodeString(cfg.RawByteCode)
	if err != nil {
		log.L().Panic(
			"invalid byte code",
			zap.String("byteCode", cfg.RawByteCode),
			zap.Error(err),
		)
	}
	if cfg.AppendContractAddress {
		addr, err := address.FromString(cfg.ContractAddressToAppend)
		if err != nil {
			log.L().Panic(
				"invalid contract address to append",
				zap.String("contractAddressToAppend", cfg.ContractAddressToAppend),
				zap.Error(err),
			)
		}
		ba := addr.Bytes()
		ba = append(make([]byte, 12), ba...)
		byteCode = append(byteCode, ba...)
	}

	return byteCode
}

func (cfg *ExecutionConfig) Amount() *big.Int {
	amount, ok := new(big.Int).SetString(cfg.RawAmount, 10)
	if !ok {
		log.L().Panic("invalid amount", zap.String("amount", cfg.RawAmount))
	}

	return amount
}

func (cfg *ExecutionConfig) GasPrice() *big.Int {
	price, ok := new(big.Int).SetString(cfg.RawGasPrice, 10)
	if !ok {
		log.L().Panic("invalid gas price", zap.String("gasPrice", cfg.RawGasPrice))
	}

	return price
}

func (cfg *ExecutionConfig) GasLimit() uint64 {
	return uint64(cfg.RawGasLimit)
}

func (cfg *ExecutionConfig) ExpectedGasConsumed() uint64 {
	return uint64(cfg.RawExpectedGasConsumed)
}

func (cfg *ExecutionConfig) ExpectedReturnValue() []byte {
	retval, err := hex.DecodeString(cfg.RawReturnValue)
	if err != nil {
		log.L().Panic(
			"invalid return value",
			zap.String("returnValue", cfg.RawReturnValue),
			zap.Error(err),
		)
	}

	return retval
}

type SmartContractTest struct {
	// the order matters
	InitBalances []ExpectedBalance `json:"initBalances"`
	Deployments  []ExecutionConfig `json:"deployments"`
	Executions   []ExecutionConfig `json:"executions"`
}

func NewSmartContractTest(t *testing.T, file string) {
	require := require.New(t)
	jsonFile, err := os.Open(file)
	require.NoError(err)
	sctBytes, err := ioutil.ReadAll(jsonFile)
	require.NoError(err)
	sct := &SmartContractTest{}
	require.NoError(json.Unmarshal(sctBytes, sct))
	sct.run(require)
}

func runExecution(
	bc blockchain.Blockchain,
	ecfg *ExecutionConfig,
	contractAddr string,
) ([]byte, *action.Receipt, error) {
	log.S().Info(ecfg.Comment)
	nonce, err := bc.Nonce(ecfg.Executor().String())
	if err != nil {
		return nil, nil, err
	}
	exec, err := action.NewExecution(
		contractAddr,
		nonce+1,
		ecfg.Amount(),
		ecfg.GasLimit(),
		ecfg.GasPrice(),
		ecfg.ByteCode(),
	)
	if err != nil {
		return nil, nil, err
	}
	if ecfg.ReadOnly { // read
		addr, err := address.FromBytes(ecfg.PrivateKey().PublicKey().Hash())
		if err != nil {
			return nil, nil, err
		}
		return bc.ExecuteContractRead(addr, exec)
	}
	builder := &action.EnvelopeBuilder{}
	elp := builder.SetAction(exec).
		SetNonce(exec.Nonce()).
		SetGasLimit(ecfg.GasLimit()).
		SetGasPrice(ecfg.GasPrice()).
		Build()
	selp, err := action.Sign(elp, ecfg.PrivateKey())
	if err != nil {
		return nil, nil, err
	}
	actionMap := make(map[string][]action.SealedEnvelope)
	actionMap[ecfg.Executor().String()] = []action.SealedEnvelope{selp}
	blk, err := bc.MintNewBlock(
		actionMap,
		testutil.TimestampNow(),
	)
	if err != nil {
		return nil, nil, err
	}
	if err := bc.ValidateBlock(blk); err != nil {
		return nil, nil, err
	}
	if err := bc.CommitBlock(blk); err != nil {
		return nil, nil, err
	}
	receipt, err := bc.GetReceiptByActionHash(exec.Hash())

	return nil, receipt, err
}

func (sct *SmartContractTest) prepareBlockchain(
	ctx context.Context,
	r *require.Assertions,
) blockchain.Blockchain {
	cfg := config.Default
	cfg.Plugins[config.GatewayPlugin] = true
	cfg.Chain.EnableAsyncIndexWrite = false
	registry := protocol.Registry{}
	acc := account.NewProtocol()
	registry.Register(account.ProtocolID, acc)
	rp := rolldpos.NewProtocol(cfg.Genesis.NumCandidateDelegates, cfg.Genesis.NumDelegates, cfg.Genesis.NumSubEpochs)
	registry.Register(rolldpos.ProtocolID, rp)
	bc := blockchain.NewBlockchain(
		cfg,
		blockchain.InMemDaoOption(),
		blockchain.InMemStateFactoryOption(),
		blockchain.RegistryOption(&registry),
	)
	r.NotNil(bc)
	registry.Register(vote.ProtocolID, vote.NewProtocol(bc))
	bc.Validator().AddActionEnvelopeValidators(protocol.NewGenericValidator(bc, genesis.Default.ActionGasLimit))
	bc.Validator().AddActionValidators(account.NewProtocol(), NewProtocol(bc))
	sf := bc.GetFactory()
	r.NotNil(sf)
	sf.AddActionHandlers(NewProtocol(bc))
	r.NoError(bc.Start(ctx))
	ws, err := sf.NewWorkingSet()
	r.NoError(err)
	for _, expectedBalance := range sct.InitBalances {
		_, err = accountutil.LoadOrCreateAccount(ws, expectedBalance.Account, expectedBalance.Balance())
		r.NoError(err)
	}
	ctx = protocol.WithRunActionsCtx(ctx,
		protocol.RunActionsCtx{
			Producer: testaddress.Addrinfo["producer"],
			GasLimit: uint64(10000000),
		})
	_, err = ws.RunActions(ctx, 0, nil)
	r.NoError(err)
	r.NoError(sf.Commit(ws))

	return bc
}

func (sct *SmartContractTest) deployContracts(
	bc blockchain.Blockchain,
	r *require.Assertions,
) (contractAddresses []string) {
	for i, contract := range sct.Deployments {
		_, receipt, err := runExecution(bc, &contract, action.EmptyAddress)
		r.NoError(err)
		r.NotNil(receipt)
		if sct.Deployments[i].Failed {
			r.Equal(action.FailureReceiptStatus, receipt.Status)
			return []string{}
		}
		if sct.Deployments[i].ExpectedGasConsumed() != 0 {
			r.Equal(sct.Deployments[i].ExpectedGasConsumed(), receipt.GasConsumed)
		}

		ws, err := bc.GetFactory().NewWorkingSet()
		r.NoError(err)
		stateDB := evm.NewStateDBAdapter(bc, ws, uint64(0), hash.ZeroHash256)
		var evmContractAddrHash common.Address
		addr, _ := address.FromString(receipt.ContractAddress)
		copy(evmContractAddrHash[:], addr.Bytes())
		r.True(bytes.Contains(sct.Deployments[i].ByteCode(), stateDB.GetCode(evmContractAddrHash)))
		contractAddresses = append(contractAddresses, receipt.ContractAddress)
	}
	return
}

func (sct *SmartContractTest) run(r *require.Assertions) {
	// prepare blockchain
	ctx := context.Background()
	bc := sct.prepareBlockchain(ctx, r)
	defer r.NoError(bc.Stop(ctx))

	// deploy smart contract
	contractAddresses := sct.deployContracts(bc, r)
	if len(contractAddresses) == 0 {
		return
	}

	// run executions
	for _, exec := range sct.Executions {
		contractAddr := contractAddresses[exec.ContractIndex]
		if exec.AppendContractAddress {
			exec.ContractAddressToAppend = contractAddresses[exec.ContractIndexToAppend]
		}
		retval, receipt, err := runExecution(bc, &exec, contractAddr)
		r.NoError(err)
		r.NotNil(receipt)
		if exec.Failed {
			r.Equal(action.FailureReceiptStatus, receipt.Status)
		} else {
			r.Equal(action.SuccessReceiptStatus, receipt.Status)
		}
		if exec.ExpectedGasConsumed() != 0 {
			r.Equal(exec.ExpectedGasConsumed(), receipt.GasConsumed)
		}
		if exec.ReadOnly {
			expected := exec.ExpectedReturnValue()
			if len(expected) == 0 {
				r.Equal(0, len(retval))
			} else {
				r.Equal(expected, retval)
			}
			return
		}
		for _, expectedBalance := range exec.ExpectedBalances {
			account := expectedBalance.Account
			if account == "" {
				account = contractAddr
			}
			balance, err := bc.Balance(account)
			r.NoError(err)
			r.Equal(0, balance.Cmp(expectedBalance.Balance()))
		}
		r.Equal(len(exec.ExpectedLogs), len(receipt.Logs))
		// TODO: check value of logs
	}
}

func TestProtocol_Handle(t *testing.T) {
	testEVM := func(t *testing.T) {
		log.S().Info("Test EVM")
		require := require.New(t)

		ctx := context.Background()
		cfg := config.Default

		testTrieFile, _ := ioutil.TempFile(os.TempDir(), "trie")
		testTriePath := testTrieFile.Name()
		testDBFile, _ := ioutil.TempFile(os.TempDir(), "db")
		testDBPath := testDBFile.Name()

		cfg.Plugins[config.GatewayPlugin] = true
		cfg.Chain.TrieDBPath = testTriePath
		cfg.Chain.ChainDBPath = testDBPath
		cfg.Chain.EnableAsyncIndexWrite = false
		registry := protocol.Registry{}
		acc := account.NewProtocol()
		registry.Register(account.ProtocolID, acc)
		rp := rolldpos.NewProtocol(cfg.Genesis.NumCandidateDelegates, cfg.Genesis.NumDelegates, cfg.Genesis.NumSubEpochs)
		registry.Register(rolldpos.ProtocolID, rp)
		bc := blockchain.NewBlockchain(
			cfg,
			blockchain.DefaultStateFactoryOption(),
			blockchain.BoltDBDaoOption(),
			blockchain.RegistryOption(&registry),
		)
		registry.Register(vote.ProtocolID, vote.NewProtocol(bc))
		bc.Validator().AddActionEnvelopeValidators(protocol.NewGenericValidator(bc, genesis.Default.ActionGasLimit))
		bc.Validator().AddActionValidators(account.NewProtocol(), NewProtocol(bc))
		sf := bc.GetFactory()
		require.NotNil(sf)
		sf.AddActionHandlers(NewProtocol(bc))

		require.NoError(bc.Start(ctx))
		require.NotNil(bc)
		defer func() {
			err := bc.Stop(ctx)
			require.NoError(err)
		}()
		ws, err := sf.NewWorkingSet()
		require.NoError(err)
		_, err = accountutil.LoadOrCreateAccount(ws, testaddress.Addrinfo["producer"].String(), unit.ConvertIotxToRau(1000000000))
		require.NoError(err)
		gasLimit := testutil.TestGasLimit
		ctx = protocol.WithRunActionsCtx(ctx,
			protocol.RunActionsCtx{
				Producer: testaddress.Addrinfo["producer"],
				GasLimit: gasLimit,
			})
		_, err = ws.RunActions(ctx, 0, nil)
		require.NoError(err)
		require.NoError(sf.Commit(ws))

		data, _ := hex.DecodeString("608060405234801561001057600080fd5b5060df8061001f6000396000f3006080604052600436106049576000357c0100000000000000000000000000000000000000000000000000000000900463ffffffff16806360fe47b114604e5780636d4ce63c146078575b600080fd5b348015605957600080fd5b5060766004803603810190808035906020019092919050505060a0565b005b348015608357600080fd5b50608a60aa565b6040518082815260200191505060405180910390f35b8060008190555050565b600080549050905600a165627a7a7230582002faabbefbbda99b20217cf33cb8ab8100caf1542bf1f48117d72e2c59139aea0029")
		execution, err := action.NewExecution(action.EmptyAddress, 1, big.NewInt(0), uint64(100000), big.NewInt(0), data)
		require.NoError(err)

		bd := &action.EnvelopeBuilder{}
		elp := bd.SetAction(execution).
			SetNonce(1).
			SetGasLimit(100000).Build()
		selp, err := action.Sign(elp, testaddress.Keyinfo["producer"].PriKey)
		require.NoError(err)

		actionMap := make(map[string][]action.SealedEnvelope)
		actionMap[testaddress.Addrinfo["producer"].String()] = []action.SealedEnvelope{selp}
		blk, err := bc.MintNewBlock(
			actionMap,
			testutil.TimestampNow(),
		)
		require.NoError(err)
		require.NoError(bc.ValidateBlock(blk))
		require.Nil(bc.CommitBlock(blk))
		require.Equal(1, len(blk.Receipts))

		eHash := execution.Hash()
		r, _ := bc.GetReceiptByActionHash(eHash)
		require.Equal(eHash, r.ActionHash)
		contract, err := address.FromString(r.ContractAddress)
		require.NoError(err)
		ws, err = sf.NewWorkingSet()
		require.NoError(err)

		stateDB := evm.NewStateDBAdapter(bc, ws, uint64(0), hash.ZeroHash256)
		var evmContractAddrHash common.Address
		copy(evmContractAddrHash[:], contract.Bytes())
		code := stateDB.GetCode(evmContractAddrHash)
		require.Nil(err)
		require.Equal(data[31:], code)

		exe, err := bc.GetActionByActionHash(eHash)
		require.Nil(err)
		require.Equal(eHash, exe.Hash())

		exes, err := bc.GetActionsFromAddress(testaddress.Addrinfo["producer"].String())
		require.Nil(err)
		require.Equal(1, len(exes))
		require.Equal(eHash, exes[0])

		blkHash, err := bc.GetBlockHashByActionHash(eHash)
		require.Nil(err)
		require.Equal(blk.HashBlock(), blkHash)

		// store to key 0
		data, _ = hex.DecodeString("60fe47b1000000000000000000000000000000000000000000000000000000000000000f")
		execution, err = action.NewExecution(r.ContractAddress, 2, big.NewInt(0), uint64(120000), big.NewInt(0), data)
		require.NoError(err)

		bd = &action.EnvelopeBuilder{}
		elp = bd.SetAction(execution).
			SetNonce(2).
			SetGasLimit(120000).Build()
		selp, err = action.Sign(elp, testaddress.Keyinfo["producer"].PriKey)
		require.NoError(err)

		log.S().Infof("execution %+v", execution)

		actionMap = make(map[string][]action.SealedEnvelope)
		actionMap[testaddress.Addrinfo["producer"].String()] = []action.SealedEnvelope{selp}
		blk, err = bc.MintNewBlock(
			actionMap,
			testutil.TimestampNow(),
		)
		require.NoError(err)
		require.NoError(bc.ValidateBlock(blk))
		require.Nil(bc.CommitBlock(blk))
		require.Equal(1, len(blk.Receipts))

		ws, err = sf.NewWorkingSet()
		require.NoError(err)
		stateDB = evm.NewStateDBAdapter(bc, ws, uint64(0), hash.ZeroHash256)
		var emptyEVMHash common.Hash
		v := stateDB.GetState(evmContractAddrHash, emptyEVMHash)
		require.Equal(byte(15), v[31])

		eHash = execution.Hash()
		r, _ = bc.GetReceiptByActionHash(eHash)
		require.Equal(eHash, r.ActionHash)

		// read from key 0
		data, _ = hex.DecodeString("6d4ce63c")
		execution, err = action.NewExecution(r.ContractAddress, 3, big.NewInt(0), uint64(120000), big.NewInt(0), data)
		require.NoError(err)

		bd = &action.EnvelopeBuilder{}
		elp = bd.SetAction(execution).
			SetNonce(3).
			SetGasLimit(120000).Build()
		selp, err = action.Sign(elp, testaddress.Keyinfo["producer"].PriKey)
		require.NoError(err)

		log.S().Infof("execution %+v", execution)
		actionMap = make(map[string][]action.SealedEnvelope)
		actionMap[testaddress.Addrinfo["producer"].String()] = []action.SealedEnvelope{selp}
		blk, err = bc.MintNewBlock(
			actionMap,
			testutil.TimestampNow(),
		)
		require.NoError(err)
		require.NoError(bc.ValidateBlock(blk))
		require.Nil(bc.CommitBlock(blk))
		require.Equal(1, len(blk.Receipts))

		eHash = execution.Hash()
		r, _ = bc.GetReceiptByActionHash(eHash)
		require.Equal(eHash, r.ActionHash)

		data, _ = hex.DecodeString("608060405234801561001057600080fd5b5060df8061001f6000396000f3006080604052600436106049576000357c0100000000000000000000000000000000000000000000000000000000900463ffffffff16806360fe47b114604e5780636d4ce63c146078575b600080fd5b348015605957600080fd5b5060766004803603810190808035906020019092919050505060a0565b005b348015608357600080fd5b50608a60aa565b6040518082815260200191505060405180910390f35b8060008190555050565b600080549050905600a165627a7a7230582002faabbefbbda99b20217cf33cb8ab8100caf1542bf1f48117d72e2c59139aea0029")
		execution1, err := action.NewExecution(action.EmptyAddress, 4, big.NewInt(0), uint64(100000), big.NewInt(10), data)
		require.NoError(err)
		bd = &action.EnvelopeBuilder{}

		elp = bd.SetAction(execution1).
			SetNonce(4).
			SetGasLimit(100000).SetGasPrice(big.NewInt(10)).Build()
		selp, err = action.Sign(elp, testaddress.Keyinfo["producer"].PriKey)
		require.NoError(err)

		actionMap = make(map[string][]action.SealedEnvelope)
		actionMap[testaddress.Addrinfo["producer"].String()] = []action.SealedEnvelope{selp}
		blk, err = bc.MintNewBlock(
			actionMap,
			testutil.TimestampNow(),
		)
		require.NoError(err)
		require.NoError(bc.ValidateBlock(blk))
		require.Nil(bc.CommitBlock(blk))
		require.Equal(1, len(blk.Receipts))
	}

	t.Run("EVM", func(t *testing.T) {
		testEVM(t)
	})
	/**
	 * source of smart contract: https://etherscan.io/address/0x6fb3e0a217407efff7ca062d46c26e5d60a14d69#code
	 */
	t.Run("ERC20", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/erc20.json")
	})
	/**
	 * Source of smart contract: https://etherscan.io/address/0x8dd5fbce2f6a956c3022ba3663759011dd51e73e#code
	 */
	t.Run("DelegateERC20", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/delegate_erc20.json")
	})
	/*
	 * Source code: https://kovan.etherscan.io/address/0x81f85886749cbbf3c2ec742db7255c6b07c63c69
	 */
	t.Run("InfiniteLoop", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/infiniteloop.json")
	})
	// RollDice
	t.Run("RollDice", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/rolldice.json")
	})
	// ChangeState
	t.Run("ChangeState", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/changestate.json")
	})
	// array-return
	t.Run("ArrayReturn", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/array-return.json")
	})
	// basic-token
	t.Run("BasicToken", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/basic-token.json")
	})
	// call-dynamic
	t.Run("CallDynamic", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/call-dynamic.json")
	})
	// factory
	t.Run("Factory", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/factory.json")
	})
	// mapping-delete
	t.Run("MappingDelete", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/mapping-delete.json")
	})
	// f.value
	t.Run("F.value", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/f.value.json")
	})
	// proposal
	t.Run("Proposal", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/proposal.json")
	})
	// public-length
	t.Run("PublicLength", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/public-length.json")
	})
	// public-mapping
	t.Run("PublicMapping", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/public-mapping.json")
	})
	// multisend
	t.Run("Multisend", func(t *testing.T) {
		NewSmartContractTest(t, "testdata/multisend.json")
	})
}

func TestProtocol_Validate(t *testing.T) {
	require := require.New(t)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mbc := mock_blockchain.NewMockBlockchain(ctrl)
	protocol := NewProtocol(mbc)
	// Case I: Oversized data
	tmpPayload := [32769]byte{}
	data := tmpPayload[:]
	ex, err := action.NewExecution("2", uint64(1), big.NewInt(0), uint64(0), big.NewInt(0), data)
	require.NoError(err)
	err = protocol.Validate(context.Background(), ex)
	require.Equal(action.ErrActPool, errors.Cause(err))
	// Case II: Negative amount
	ex, err = action.NewExecution("2", uint64(1), big.NewInt(-100), uint64(0), big.NewInt(0), []byte{})
	require.NoError(err)
	err = protocol.Validate(context.Background(), ex)
	require.Equal(action.ErrBalance, errors.Cause(err))
	// Case IV: Invalid contract address
	ex, err = action.NewExecution(
		testaddress.Addrinfo["bravo"].String()+"bbb",
		uint64(1),
		big.NewInt(0),
		uint64(0),
		big.NewInt(0),
		[]byte{},
	)
	require.NoError(err)
	err = protocol.Validate(context.Background(), ex)
	require.Error(err)
	require.True(strings.Contains(err.Error(), "error when validating contract's address"))
}

/*


func TestSimpleSum(t *testing.T) {
	sct := &smartContractTest{
		prepare: map[string]*big.Int{testaddress.Addrinfo["alfa"].String(): big.NewInt(9876543210)},
		deploy: execCfg{
			executor:            testaddress.Addrinfo["alfa"].String(),
			privateKey:          testaddress.Keyinfo["alfa"].PriKey,
			codeHex:             "608060405234801561001057600080fd5b5060c58061001f6000396000f300608060405260043610603f576000357c0100000000000000000000000000000000000000000000000000000000900463ffffffff168063cad0899b146044575b600080fd5b348015604f57600080fd5b5060766004803603810190808035906020019092919080359060200190929190505050608c565b6040518082815260200191505060405180910390f35b60008183019050929150505600a165627a7a72305820b6506f4075e1d6b6a02a720b45c6cb465437e8ad240dc65eb6377a92889e6e020029",
			amount:              0,
			gasLimit:            uint64(2100000),
			gasPrice:            7,
			hasReturnValue:      false,
			expectedReturnValue: "",
			expectedBalances:    map[string]*big.Int{},
		},
		executions: []execCfg{
			func() execCfg {
				retval, _ := hex.DecodeString("000000000000000000000000000000000000000000000000000000000001046a")
				return execCfg{
					executor:            testaddress.Addrinfo["alfa"].String(),
					privateKey:          testaddress.Keyinfo["alfa"].PriKey,
					codeHex:             "cad0899b0000000000000000000000000000000000000000000000000000000000003039000000000000000000000000000000000000000000000000000000000000d431",
					amount:              0,
					gasLimit:            uint64(1000000),
					gasPrice:            0,
					hasReturnValue:      true,
					expectedReturnValue: retval,
					expectedBalances:    map[string]*big.Int{},
				}
			}(),
		},
	}
	sct.run(require.New(t))
}
func TestDouble(t *testing.T) {
	sct := &smartContractTest{
		prepare: map[string]*big.Int{testaddress.Addrinfo["alfa"].String(): big.NewInt(9876543210)},
		deploy: execCfg{
			executor:            testaddress.Addrinfo["alfa"].String(),
			privateKey:          testaddress.Keyinfo["alfa"].PriKey,
			codeHex:             "608060405234801561001057600080fd5b5060c28061001f6000396000f3fe6080604052600436106039576000357c010000000000000000000000000000000000000000000000000000000090048063eee9720614603e575b600080fd5b348015604957600080fd5b50607360048036036020811015605e57600080fd5b81019080803590602001909291905050506089565b6040518082815260200191505060405180910390f35b600081600202905091905056fea165627a7a7230582098239f36a0b72e5504c45d691ed8eb88c07b9e027149cbcb0b384474ffb0c96d0029",
			amount:              0,
			gasLimit:            uint64(2100000),
			gasPrice:            7,
			hasReturnValue:      false,
			expectedReturnValue: "",
			expectedBalances:    map[string]*big.Int{},
		},
		executions: []execCfg{
			func() execCfg {
				retval, _ := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000002")
				return execCfg{
					executor:            testaddress.Addrinfo["alfa"].String(),
					privateKey:          testaddress.Keyinfo["alfa"].PriKey,
					codeHex:             "eee972060000000000000000000000000000000000000000000000000000000000000001",
					amount:              0,
					gasLimit:            uint64(1000000),
					gasPrice:            0,
					hasReturnValue:      true,
					expectedReturnValue: retval,
					expectedBalances:    map[string]*big.Int{},
				}
			}(),
		},
	}
	sct.run(require.New(t))
}
func TestUSBHid(t *testing.T) {
	var rb, wb []byte
	log.S().Infof("Hello Avo\n")
	devinfoarr := hid.Enumerate(0x0525, 0xA4AC)
	if len(devinfoarr) < 0 {
		log.S().Errorf("No avo board found\n")
		return
	}
	devinfo := devinfoarr[0]
	log.S().Infof("Product=%s Manuf=%s PID=%X VID=%X\n", devinfo.Product, devinfo.Manufacturer, devinfo.ProductID, devinfo.VendorID)
	log.S().Infof("path=%s\n", devinfo.Path)

	dev, err := devinfo.Open()
	if err != nil {
		log.S().Errorf("Device open failed\n")
		return
	}
	wb = make([]byte, 3)

	rb = make([]byte, 128)
	sendlen, werr := dev.Write(wb)
	log.S().Infof("send=%d werr=%d\n", sendlen, werr)

	readlen, rerr := dev.Read(rb)
	log.S().Infof("read=%d rerr=%d\n", readlen, rerr)

	dev.Close()

}

*/
