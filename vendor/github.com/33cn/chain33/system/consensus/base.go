package consensus

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/33cn/chain33/client"
	log "github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/common/merkle"
	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
)

var tlog = log.New("module", "consensus")

var (
	zeroHash [32]byte
)

var randgen *rand.Rand

func init() {
	randgen = rand.New(rand.NewSource(types.Now().UnixNano()))
	QueryData.Register("base", &BaseClient{})
}

type Miner interface {
	CreateGenesisTx() []*types.Transaction
	GetGenesisBlockTime() int64
	CreateBlock()
	CheckBlock(parent *types.Block, current *types.BlockDetail) error
	ProcEvent(msg queue.Message) bool
}

type BaseClient struct {
	client       queue.Client
	api          client.QueueProtocolAPI
	minerStart   int32
	once         sync.Once
	Cfg          *types.Consensus
	currentBlock *types.Block
	mulock       sync.Mutex
	child        Miner
	minerstartCB func()
	isCaughtUp   int32
}

func NewBaseClient(cfg *types.Consensus) *BaseClient {
	var flag int32
	if cfg.Minerstart {
		flag = 1
	}
	client := &BaseClient{minerStart: flag, isCaughtUp: 0}
	client.Cfg = cfg
	log.Info("Enter consensus " + cfg.Name)
	return client
}

func (client *BaseClient) GetGenesisBlockTime() int64 {
	return client.Cfg.GenesisBlockTime
}

func (bc *BaseClient) SetChild(c Miner) {
	bc.child = c
}

func (bc *BaseClient) GetAPI() client.QueueProtocolAPI {
	return bc.api
}

func (bc *BaseClient) InitClient(c queue.Client, minerstartCB func()) {
	log.Info("Enter SetQueueClient method of consensus")
	bc.client = c
	bc.minerstartCB = minerstartCB
	bc.api, _ = client.New(c, nil)
	bc.InitMiner()
}

func (bc *BaseClient) GetQueueClient() queue.Client {
	return bc.client
}

func (bc *BaseClient) RandInt64() int64 {
	return randgen.Int63()
}

func (bc *BaseClient) InitMiner() {
	bc.once.Do(bc.minerstartCB)
}

func (bc *BaseClient) SetQueueClient(c queue.Client) {
	bc.InitClient(c, func() {
		//call init block
		bc.InitBlock()
	})
	go bc.EventLoop()
	go bc.child.CreateBlock()
}

//change init block
func (bc *BaseClient) InitBlock() {
	block, err := bc.RequestLastBlock()
	if err != nil {
		panic(err)
	}
	if block == nil {
		// 创世区块
		newblock := &types.Block{}
		newblock.Height = 0
		newblock.BlockTime = bc.child.GetGenesisBlockTime()
		// TODO: 下面这些值在创世区块中赋值nil，是否合理？
		newblock.ParentHash = zeroHash[:]
		tx := bc.child.CreateGenesisTx()
		newblock.Txs = tx
		newblock.TxHash = merkle.CalcMerkleRoot(newblock.Txs)
		if newblock.Height == 0 {
			newblock.Difficulty = types.GetP(0).PowLimitBits
		}
		bc.WriteBlock(zeroHash[:], newblock)
	} else {
		bc.SetCurrentBlock(block)
	}
}

func (bc *BaseClient) Close() {
	atomic.StoreInt32(&bc.minerStart, 0)
	bc.client.Close()
	log.Info("consensus base closed")
}

//为了不引起交易检查时候产生的无序
func (bc *BaseClient) CheckTxDup(txs []*types.Transaction) (transactions []*types.Transaction) {
	cacheTxs := types.TxsToCache(txs)
	var err error
	cacheTxs, err = util.CheckTxDup(bc.client, cacheTxs, 0)
	if err != nil {
		return txs
	}
	return types.CacheToTxs(cacheTxs)
}

func (bc *BaseClient) IsMining() bool {
	return atomic.LoadInt32(&bc.minerStart) == 1
}

func (bc *BaseClient) IsCaughtUp() bool {
	if bc.client == nil {
		panic("bc not bind message queue.")
	}
	msg := bc.client.NewMessage("blockchain", types.EventIsSync, nil)
	bc.client.Send(msg, true)
	resp, err := bc.client.Wait(msg)
	if err != nil {
		return false
	}
	return resp.GetData().(*types.IsCaughtUp).GetIscaughtup()
}

func (bc *BaseClient) ExecConsensus(data *types.ChainExecutor) (types.Message, error) {
	param, err := QueryData.Decode(data.Driver, data.FuncName, data.Param)
	if err != nil {
		return nil, err
	}
	return QueryData.Call(data.Driver, data.FuncName, param)
}

// 准备新区块
func (bc *BaseClient) EventLoop() {
	// 监听blockchain模块，获取当前最高区块
	bc.client.Sub("consensus")
	go func() {
		for msg := range bc.client.Recv() {
			tlog.Debug("consensus recv", "msg", msg)
			if msg.Ty == types.EventConsensusQuery {
				exec := msg.GetData().(*types.ChainExecutor)
				param, err := QueryData.Decode(exec.Driver, exec.FuncName, exec.Param)
				if err != nil {
					msg.Reply(bc.api.NewMessage("", 0, err))
					continue
				}
				reply, err := QueryData.Call(exec.Driver, exec.FuncName, param)
				if err != nil {
					msg.Reply(bc.api.NewMessage("", 0, err))
				} else {
					msg.Reply(bc.api.NewMessage("", 0, reply))
				}
			} else if msg.Ty == types.EventAddBlock {
				block := msg.GetData().(*types.BlockDetail).Block
				bc.SetCurrentBlock(block)
			} else if msg.Ty == types.EventCheckBlock {
				block := msg.GetData().(*types.BlockDetail)
				err := bc.CheckBlock(block)
				msg.ReplyErr("EventCheckBlock", err)
			} else if msg.Ty == types.EventMinerStart {
				if !atomic.CompareAndSwapInt32(&bc.minerStart, 0, 1) {
					msg.ReplyErr("EventMinerStart", types.ErrMinerIsStared)
				} else {
					bc.InitMiner()
					msg.ReplyErr("EventMinerStart", nil)
				}
			} else if msg.Ty == types.EventMinerStop {
				if !atomic.CompareAndSwapInt32(&bc.minerStart, 1, 0) {
					msg.ReplyErr("EventMinerStop", types.ErrMinerNotStared)
				} else {
					msg.ReplyErr("EventMinerStop", nil)
				}
			} else if msg.Ty == types.EventDelBlock {
				block := msg.GetData().(*types.BlockDetail).Block
				bc.UpdateCurrentBlock(block)
			} else {
				if !bc.child.ProcEvent(msg) {
					msg.ReplyErr("BaseClient.EventLoop() ", types.ErrActionNotSupport)
				}
			}
		}
	}()
}

func (bc *BaseClient) CheckBlock(block *types.BlockDetail) error {
	//check parent
	if block.Block.Height <= 0 { //genesis block not check
		return nil
	}
	parent, err := bc.RequestBlock(block.Block.Height - 1)
	if err != nil {
		return err
	}
	//check base info
	if parent.Height+1 != block.Block.Height {
		return types.ErrBlockHeight
	}
	if types.IsFork(block.Block.Height, "ForkCheckBlockTime") && parent.BlockTime > block.Block.BlockTime {
		return types.ErrBlockTime
	}
	//check parent hash
	if string(block.Block.GetParentHash()) != string(parent.Hash()) {
		return types.ErrParentHash
	}
	//check by drivers
	err = bc.child.CheckBlock(parent, block)
	return err
}

// Mempool中取交易列表
func (bc *BaseClient) RequestTx(listSize int, txHashList [][]byte) []*types.Transaction {
	if bc.client == nil {
		panic("bc not bind message queue.")
	}
	msg := bc.client.NewMessage("mempool", types.EventTxList, &types.TxHashList{Hashes: txHashList, Count: int64(listSize)})
	bc.client.Send(msg, true)
	resp, err := bc.client.Wait(msg)
	if err != nil {
		return nil
	}
	return resp.GetData().(*types.ReplyTxList).GetTxs()
}

func (bc *BaseClient) RequestBlock(start int64) (*types.Block, error) {
	if bc.client == nil {
		panic("bc not bind message queue.")
	}
	msg := bc.client.NewMessage("blockchain", types.EventGetBlocks, &types.ReqBlocks{start, start, false, []string{""}})
	bc.client.Send(msg, true)
	resp, err := bc.client.Wait(msg)
	if err != nil {
		return nil, err
	}
	blocks := resp.GetData().(*types.BlockDetails)
	return blocks.Items[0].Block, nil
}

//获取最新的block从blockchain模块
func (bc *BaseClient) RequestLastBlock() (*types.Block, error) {
	if bc.client == nil {
		panic("client not bind message queue.")
	}
	msg := bc.client.NewMessage("blockchain", types.EventGetLastBlock, nil)
	bc.client.Send(msg, true)
	resp, err := bc.client.Wait(msg)
	if err != nil {
		return nil, err
	}
	block := resp.GetData().(*types.Block)
	return block, nil
}

//del mempool
func (bc *BaseClient) delMempoolTx(deltx []*types.Transaction) error {
	hashList := buildHashList(deltx)
	msg := bc.client.NewMessage("mempool", types.EventDelTxList, hashList)
	bc.client.Send(msg, true)
	resp, err := bc.client.Wait(msg)
	if err != nil {
		return err
	}
	if resp.GetData().(*types.Reply).GetIsOk() {
		return nil
	}
	return errors.New(string(resp.GetData().(*types.Reply).GetMsg()))
}

func buildHashList(deltx []*types.Transaction) *types.TxHashList {
	list := &types.TxHashList{}
	for i := 0; i < len(deltx); i++ {
		list.Hashes = append(list.Hashes, deltx[i].Hash())
	}
	return list
}

// 向blockchain写区块
func (bc *BaseClient) WriteBlock(prev []byte, block *types.Block) error {
	blockdetail := &types.BlockDetail{Block: block}
	msg := bc.client.NewMessage("blockchain", types.EventAddBlockDetail, blockdetail)
	bc.client.Send(msg, true)
	resp, err := bc.client.Wait(msg)
	if err != nil {
		return err
	}
	blockdetail = resp.GetData().(*types.BlockDetail)
	//从mempool 中删除错误的交易
	deltx := diffTx(block.Txs, blockdetail.Block.Txs)
	if len(deltx) > 0 {
		bc.delMempoolTx(deltx)
	}
	if blockdetail != nil {
		bc.SetCurrentBlock(blockdetail.Block)
	} else {
		return errors.New("block detail is nil")
	}
	return nil
}

func diffTx(tx1, tx2 []*types.Transaction) (deltx []*types.Transaction) {
	txlist2 := make(map[string]bool)
	for _, tx := range tx2 {
		txlist2[string(tx.Hash())] = true
	}
	for _, tx := range tx1 {
		hash := string(tx.Hash())
		if _, ok := txlist2[hash]; !ok {
			deltx = append(deltx, tx)
		}
	}
	return deltx
}

func (bc *BaseClient) SetCurrentBlock(b *types.Block) {
	bc.mulock.Lock()
	bc.currentBlock = b
	bc.mulock.Unlock()
}

func (bc *BaseClient) UpdateCurrentBlock(b *types.Block) {
	bc.mulock.Lock()
	defer bc.mulock.Unlock()
	block, err := bc.RequestLastBlock()
	if err != nil {
		log.Error("UpdateCurrentBlock", "RequestLastBlock", err)
		return
	}
	bc.currentBlock = block
}

func (bc *BaseClient) GetCurrentBlock() (b *types.Block) {
	bc.mulock.Lock()
	defer bc.mulock.Unlock()
	return bc.currentBlock
}

func (bc *BaseClient) GetCurrentHeight() int64 {
	bc.mulock.Lock()
	start := bc.currentBlock.Height
	bc.mulock.Unlock()
	return start
}

func (bc *BaseClient) Lock() {
	bc.mulock.Lock()
}

func (bc *BaseClient) Unlock() {
	bc.mulock.Unlock()
}

func (bc *BaseClient) ConsensusTicketMiner(iscaughtup *types.IsCaughtUp) {
	if !atomic.CompareAndSwapInt32(&bc.isCaughtUp, 0, 1) {
		log.Info("ConsensusTicketMiner", "isCaughtUp", bc.isCaughtUp)
	} else {
		log.Info("ConsensusTicketMiner", "isCaughtUp", bc.isCaughtUp)
	}
}

func (bc *BaseClient) AddTxsToBlock(block *types.Block, txs []*types.Transaction) []*types.Transaction {
	size := block.Size()
	max := types.MaxBlockSize - 100000 //留下100K空间，添加其他的交易
	currentcount := int64(len(block.Txs))
	maxTx := types.GetP(block.Height).MaxTxNumber
	addedTx := make([]*types.Transaction, 0, len(txs))
	for i := 0; i < len(txs); i++ {
		txgroup, err := txs[i].GetTxGroup()
		if err != nil {
			continue
		}
		if txgroup == nil {
			if currentcount+1 > maxTx {
				return addedTx
			}
			size += txs[i].Size()
			if size > max {
				return addedTx
			}
			addedTx = append(addedTx, txs[i])
			block.Txs = append(block.Txs, txs[i])
		} else {
			if currentcount+int64(len(txgroup.Txs)) > maxTx {
				return addedTx
			}
			for i := 0; i < len(txgroup.Txs); i++ {
				size += txgroup.Txs[i].Size()
			}
			if size > max {
				return addedTx
			}
			addedTx = append(addedTx, txgroup.Txs...)
			block.Txs = append(block.Txs, txgroup.Txs...)
		}
	}
	return addedTx
}
