package consensus

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"sync"
)

// 交易与提议的生命周期状态。
const (
	StatusQueued    = "queued"    // 等待打包
	StatusProposed  = "proposed"  // 等待投票
	StatusConfirmed = "confirmed" // 已确认入块
	StatusReplaced  = "replaced"  // 被更高费用交易替换
	StatusExpired   = "expired"   // 到期失效
)

// 拒绝与查询错误。
var (
	ErrInvalidSignature = errors.New("consensus: invalid signature")
	ErrInvalidSender    = errors.New("consensus: sender must be a 32-byte ed25519 public key")
	ErrInvalidSeq       = errors.New("consensus: seq must be positive")
	ErrTxExpired        = errors.New("consensus: tx expired")
	ErrSeqTooLow        = errors.New("consensus: seq not greater than confirmed seq")
	ErrDuplicateTx      = errors.New("consensus: duplicate tx")
	ErrTxProposed       = errors.New("consensus: tx already proposed and cannot be replaced")
	ErrFeeNotHigher     = errors.New("consensus: replacement fee not strictly higher")
	ErrUnknownTx        = errors.New("consensus: unknown tx")
	ErrNoProposal       = errors.New("consensus: no proposal for current round")
	ErrNotValidator     = errors.New("consensus: identity is not in validator set")
	ErrWrongRound       = errors.New("consensus: vote round does not match current round")
	ErrWrongBlock       = errors.New("consensus: block id does not match current proposal")
	ErrDuplicateVote    = errors.New("consensus: duplicate vote")
	ErrNotFound         = errors.New("consensus: not found")
	ErrInvalidConfig    = errors.New("consensus: invalid config")
)

// Config 是节点初始化配置。种子、验证者名单和交易上限决定确定性出块。
type Config struct {
	Seed       int64    `json:"seed"`
	Validators [][]byte `json:"validators"`
	TxLimit    uint64   `json:"txLimit"`
}

// Proposal 是当前轮的提议。
type Proposal struct {
	Round uint64 `json:"round"`
	Block *Block `json:"block"`
}

// TxRecord 记录一笔交易及其生命周期状态。
type TxRecord struct {
	Tx          Tx     `json:"tx"`
	Status      string `json:"status"`
	BlockHeight uint64 `json:"blockHeight,omitempty"`
	ReplacedBy  string `json:"replacedBy,omitempty"`
}

// state 是节点的完整持久化状态。
type state struct {
	Config       Config                       `json:"config"`
	Round        uint64                       `json:"round"`
	Height       uint64                       `json:"height"`
	Blocks       []*Block                     `json:"blocks"`
	Txs          map[string]*TxRecord          `json:"txs"`
	Pool         map[string]map[string]string  `json:"pool"`
	Proposal     *Proposal                    `json:"proposal"`
	Votes        map[string]string            `json:"votes"`
	ConfirmedSeq map[string]uint64            `json:"confirmedSeq"`
	Seen         map[string]bool              `json:"seen"`
}

// Node 是本地共识节点。所有方法并发安全；状态变更先落盘再生效，
// 落盘失败时调用返回错误且内存状态保持不变。
type Node struct {
	dir   string
	cfg   Config
	order [][]byte
	mu    sync.Mutex
	st    *state
}

// Open 打开（或首次创建）一个节点。目录下已有状态时恢复轮次、池、
// 未决提议、票数与确认历史；否则使用给定配置初始化：轮次从 1 开始，
// 账户已确认序号初值为 0。
func Open(dir string, cfg Config) (*Node, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	n := &Node{dir: dir, cfg: cfg, order: shuffleValidators(cfg)}
	if st, err := loadState(dir); err == nil {
		n.st = st
		n.cfg = st.Config
		n.order = shuffleValidators(st.Config)
		rebuildSeen(n.st)
		return n, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	n.st = newState(cfg)
	if err := n.persist(); err != nil {
		return nil, err
	}
	return n, nil
}

func validateConfig(cfg Config) error {
	if len(cfg.Validators) == 0 {
		return fmt.Errorf("%w: empty validator set", ErrInvalidConfig)
	}
	if cfg.TxLimit == 0 {
		return fmt.Errorf("%w: tx limit must be positive", ErrInvalidConfig)
	}
	seen := make(map[string]bool, len(cfg.Validators))
	for _, v := range cfg.Validators {
		if len(v) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: validator must be a 32-byte ed25519 public key", ErrInvalidConfig)
		}
		h := hex.EncodeToString(v)
		if seen[h] {
			return fmt.Errorf("%w: duplicate validator %s", ErrInvalidConfig, h)
		}
		seen[h] = true
	}
	return nil
}

func newState(cfg Config) *state {
	return &state{
		Config:       cfg,
		Round:        1,
		Height:       0,
		Blocks:       []*Block{},
		Txs:          map[string]*TxRecord{},
		Pool:         map[string]map[string]string{},
		Votes:        map[string]string{},
		ConfirmedSeq: map[string]uint64{},
		Seen:         map[string]bool{},
	}
}

// rebuildSeen 从事务记录重建去重集合，保证重启后重复提交仍被拒绝。
func rebuildSeen(st *state) {
	st.Seen = make(map[string]bool, len(st.Txs))
	for idHex := range st.Txs {
		st.Seen[idHex] = true
	}
}

// shuffleValidators 用种子对验证者名单做 Fisher-Yates 洗牌，
// 出块者按轮次在洗牌后的名单中轮转，保证确定性。
func shuffleValidators(cfg Config) [][]byte {
	order := make([][]byte, len(cfg.Validators))
	for i, v := range cfg.Validators {
		order[i] = append([]byte(nil), v...)
	}
	r := rand.New(rand.NewSource(cfg.Seed))
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order
}

func (n *Node) persist() error {
	return saveState(n.dir, n.st)
}

// mutate 在状态副本上执行 fn，落盘成功后才替换内存状态，
// 落盘失败时内存状态保持调用前的完整状态。
func (n *Node) mutate(fn func(*state)) error {
	clone, err := cloneState(n.st)
	if err != nil {
		return err
	}
	fn(clone)
	if err := saveState(n.dir, clone); err != nil {
		return err
	}
	n.st = clone
	return nil
}

func cloneState(st *state) (*state, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var clone state
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func seqKey(seq uint64) string { return strconv.FormatUint(seq, 10) }

func txIDFromHex(s string) (TxID, bool) {
	var id TxID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, false
	}
	copy(id[:], b)
	return id, true
}

func (n *Node) isValidator(v []byte) bool {
	for _, x := range n.cfg.Validators {
		if bytes.Equal(x, v) {
			return true
		}
	}
	return false
}

func (n *Node) proposer() []byte {
	idx := int((n.st.Round - 1) % uint64(len(n.order)))
	return append([]byte(nil), n.order[idx]...)
}

func (n *Node) prevHash() Hash {
	if len(n.st.Blocks) > 0 {
		return n.st.Blocks[len(n.st.Blocks)-1].ID()
	}
	return Hash{}
}

// Round 返回当前轮次（从 1 开始）。
func (n *Node) Round() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st.Round
}

// Height 返回已确认区块高度（从 0 开始，0 表示尚无确认块）。
func (n *Node) Height() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st.Height
}

// Proposer 返回当前轮的出块验证者公钥。
func (n *Node) Proposer() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.proposer()
}

// Config 返回节点配置。
func (n *Node) Config() Config {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg
}

// SubmitTx 提交一笔交易。依次校验签名、到期、重复性与序号；
// 同发送者同序号在池中仅保留一笔，费用严格更高才可替换，
// 已提议的交易禁止替换。返回交易标识或具体拒绝原因。
func (n *Node) SubmitTx(tx *Tx) (TxID, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(tx.Sender) != ed25519.PublicKeySize {
		return TxID{}, ErrInvalidSender
	}
	if tx.Seq == 0 {
		return TxID{}, ErrInvalidSeq
	}
	if !ed25519.Verify(ed25519.PublicKey(tx.Sender), SigningPayload(tx), tx.Signature) {
		return TxID{}, ErrInvalidSignature
	}
	if tx.Expiry <= n.st.Round {
		return TxID{}, ErrTxExpired
	}
	id := TxIDOf(tx)
	idHex := hex.EncodeToString(id[:])
	if n.st.Seen[idHex] {
		return TxID{}, ErrDuplicateTx
	}
	senderHex := hex.EncodeToString(tx.Sender)
	if tx.Seq <= n.st.ConfirmedSeq[senderHex] {
		return TxID{}, ErrSeqTooLow
	}
	var replacedHex string
	if acc, ok := n.st.Pool[senderHex]; ok {
		if existingHex, ok := acc[seqKey(tx.Seq)]; ok {
			existing := n.st.Txs[existingHex]
			if existing.Status == StatusProposed {
				return TxID{}, ErrTxProposed
			}
			if tx.Fee <= existing.Tx.Fee {
				return TxID{}, ErrFeeNotHigher
			}
			replacedHex = existingHex
		}
	}

	err := n.mutate(func(st *state) {
		st.Seen[idHex] = true
		st.Txs[idHex] = &TxRecord{Tx: *tx, Status: StatusQueued}
		if st.Pool[senderHex] == nil {
			st.Pool[senderHex] = map[string]string{}
		}
		st.Pool[senderHex][seqKey(tx.Seq)] = idHex
		if replacedHex != "" {
			st.Txs[replacedHex].Status = StatusReplaced
			st.Txs[replacedHex].ReplacedBy = idHex
		}
	})
	return id, err
}

// Propose 生成（或返回已有的）当前轮提议。重复请求返回同一提议。
// 打包时从各账户下一条可确认交易中选费用最高者，费用相同按交易标识
// 字典序选择；选入后继续考虑该账户后续序号，直到达到单块上限或没有
// 可选交易。没有可选交易时返回空块。提议中的交易等待投票，此后提交的
// 交易不改变当前提议。
func (n *Node) Propose() (*Block, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.st.Proposal != nil {
		return n.st.Proposal.Block, nil
	}

	type candidate struct {
		senderHex string
		seq       uint64
		idHex     string
		fee       uint64
	}
	var cands []candidate
	for senderHex, acc := range n.st.Pool {
		seq := n.st.ConfirmedSeq[senderHex] + 1
		if idHex, ok := acc[seqKey(seq)]; ok && n.st.Txs[idHex].Status == StatusQueued {
			cands = append(cands, candidate{senderHex, seq, idHex, n.st.Txs[idHex].Tx.Fee})
		}
	}
	less := func(i, j int) bool {
		if cands[i].fee != cands[j].fee {
			return cands[i].fee > cands[j].fee
		}
		return cands[i].idHex < cands[j].idHex
	}
	sort.Slice(cands, less)

	var selected []string
	for len(selected) < int(n.st.Config.TxLimit) && len(cands) > 0 {
		c := cands[0]
		selected = append(selected, c.idHex)
		nextSeq := c.seq + 1
		if idHex, ok := n.st.Pool[c.senderHex][seqKey(nextSeq)]; ok && n.st.Txs[idHex].Status == StatusQueued {
			cands[0] = candidate{c.senderHex, nextSeq, idHex, n.st.Txs[idHex].Tx.Fee}
			sort.Slice(cands, less)
		} else {
			cands = cands[1:]
		}
	}

	blk := &Block{
		Height:   n.st.Height + 1,
		Round:    n.st.Round,
		PrevHash: n.prevHash(),
		Proposer: n.proposer(),
	}
	for _, idHex := range selected {
		id, _ := txIDFromHex(idHex)
		blk.TxIDs = append(blk.TxIDs, id)
	}

	err := n.mutate(func(st *state) {
		st.Proposal = &Proposal{Round: st.Round, Block: blk}
		for _, idHex := range selected {
			st.Txs[idHex].Status = StatusProposed
		}
	})
	if err != nil {
		return nil, err
	}
	return blk, nil
}

// Vote 由名单内验证者对当前轮次和区块标识投票。重复投同一票不增加票数；
// 名单外身份、错误轮次或错误区块标识均被拒绝。票数严格超过名单人数
// 三分之二时立即确认。返回当前票数。
func (n *Node) Vote(validator []byte, round uint64, blockID Hash) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.isValidator(validator) {
		return 0, ErrNotValidator
	}
	if round != n.st.Round {
		return 0, ErrWrongRound
	}
	if n.st.Proposal == nil {
		return 0, ErrNoProposal
	}
	if blockID != n.st.Proposal.Block.ID() {
		return 0, ErrWrongBlock
	}
	vHex := hex.EncodeToString(validator)
	if _, voted := n.st.Votes[vHex]; voted {
		return len(n.st.Votes), ErrDuplicateVote
	}
	if err := n.mutate(func(st *state) {
		st.Votes[vHex] = hex.EncodeToString(blockID[:])
	}); err != nil {
		return 0, err
	}
	count := len(n.st.Votes)
	if count*3 > len(n.st.Config.Validators)*2 {
		if err := n.confirm(); err != nil {
			return count, err
		}
	}
	return count, nil
}

// confirm 在票数达标后执行完整确认：记录连续高度、前块标识与交易顺序，
// 推进账户序号并移除已确认交易，随后进入下一轮。确认在状态副本上完成后
// 一次性落盘；落盘失败则内存状态不变，已确认历史不会被改写。
func (n *Node) confirm() error {
	prop := n.st.Proposal
	if prop == nil {
		return ErrNoProposal
	}
	blk := prop.Block
	return n.mutate(func(st *state) {
		st.Blocks = append(st.Blocks, blk)
		st.Height = blk.Height
		for _, id := range blk.TxIDs {
			idHex := hex.EncodeToString(id[:])
			rec := st.Txs[idHex]
			senderHex := hex.EncodeToString(rec.Tx.Sender)
			st.ConfirmedSeq[senderHex] = rec.Tx.Seq
			delete(st.Pool[senderHex], seqKey(rec.Tx.Seq))
			if len(st.Pool[senderHex]) == 0 {
				delete(st.Pool, senderHex)
			}
			rec.Status = StatusConfirmed
			rec.BlockHeight = blk.Height
		}
		st.Proposal = nil
		st.Votes = map[string]string{}
		st.Round++
		expirePool(st)
	})
}

// EndRound 主动结束未确认轮次：不留下确认块，原提议交易回到排队状态，
// 随后进入下一轮。进入新轮次时，到期轮次不大于当前轮次的池内交易失效。
func (n *Node) EndRound() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.mutate(func(st *state) {
		if st.Proposal != nil {
			for _, id := range st.Proposal.Block.TxIDs {
				idHex := hex.EncodeToString(id[:])
				st.Txs[idHex].Status = StatusQueued
			}
			st.Proposal = nil
		}
		st.Votes = map[string]string{}
		st.Round++
		expirePool(st)
	})
}

// expirePool 使到期轮次不大于当前轮次的池内（含排队）交易失效。
func expirePool(st *state) {
	for senderHex, acc := range st.Pool {
		for k, idHex := range acc {
			rec := st.Txs[idHex]
			if rec.Tx.Expiry <= st.Round {
				rec.Status = StatusExpired
				delete(acc, k)
			}
		}
		if len(acc) == 0 {
			delete(st.Pool, senderHex)
		}
	}
}

// Block 按高度查询已确认块。
func (n *Node) Block(height uint64) (*Block, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, b := range n.st.Blocks {
		if b.Height == height {
			return b, nil
		}
	}
	return nil, ErrNotFound
}

// TxStatusInfo 是交易状态查询结果。
type TxStatusInfo struct {
	TxID        TxID
	Status      string
	Tx          Tx
	BlockHeight uint64
	Block       *Block
	ReplacedBy  TxID
}

// TxStatus 按交易标识查询状态：排队、已提议、已确认、已替换或已过期，
// 已确认交易关联所在区块，已替换交易关联替代交易标识。
func (n *Node) TxStatus(id TxID) (*TxStatusInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	rec, ok := n.st.Txs[hex.EncodeToString(id[:])]
	if !ok {
		return nil, ErrUnknownTx
	}
	info := &TxStatusInfo{TxID: id, Status: rec.Status, Tx: rec.Tx, BlockHeight: rec.BlockHeight}
	if rec.ReplacedBy != "" {
		info.ReplacedBy, _ = txIDFromHex(rec.ReplacedBy)
	}
	if rec.Status == StatusConfirmed {
		info.Block, _ = n.blockAt(rec.BlockHeight)
	}
	return info, nil
}

func (n *Node) blockAt(height uint64) (*Block, error) {
	for _, b := range n.st.Blocks {
		if b.Height == height {
			return b, nil
		}
	}
	return nil, ErrNotFound
}

// AccountTx 是账户待处理交易条目。
type AccountTx struct {
	TxID   TxID
	Seq    uint64
	Fee    uint64
	Status string // queued（等待打包）或 proposed（等待投票）
}

// AccountInfo 是账户查询结果：已确认序号、待处理交易及最早缺口序号。
type AccountInfo struct {
	Address      []byte
	ConfirmedSeq uint64
	Pending      []AccountTx
	Gap          uint64 // 0 表示无缺口
	HasGap       bool
}

// Account 按账户查询已确认序号与待处理交易；序号有缺口时指出最早缺少
// 的序号，已提议交易显示等待投票，其余池内交易显示等待打包。
func (n *Node) Account(addr []byte) (*AccountInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	senderHex := hex.EncodeToString(addr)
	info := &AccountInfo{Address: addr, ConfirmedSeq: n.st.ConfirmedSeq[senderHex]}
	acc := n.st.Pool[senderHex]

	type pending struct {
		seq    uint64
		idHex  string
		status string
		fee    uint64
	}
	var list []pending
	for k, idHex := range acc {
		seq, _ := strconv.ParseUint(k, 10, 64)
		rec := n.st.Txs[idHex]
		list = append(list, pending{seq, idHex, rec.Status, rec.Tx.Fee})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seq < list[j].seq })
	for _, p := range list {
		id, _ := txIDFromHex(p.idHex)
		info.Pending = append(info.Pending, AccountTx{TxID: id, Seq: p.seq, Fee: p.fee, Status: p.status})
	}

	if len(list) > 0 {
		s := info.ConfirmedSeq + 1
		for {
			if _, ok := acc[seqKey(s)]; ok {
				s++
				continue
			}
			break
		}
		maxSeq := list[len(list)-1].seq
		if s <= maxSeq {
			info.Gap = s
			info.HasGap = true
		}
	}
	return info, nil
}
