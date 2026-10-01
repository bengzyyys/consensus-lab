package consensus

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Config 是节点初始化时固定的配置。
type Config struct {
	// Seed 决定每轮提议者的种子，任意非空字节。
	Seed []byte
	// Validators 固定、非空、无重复的验证者公钥名单（Ed25519 32 字节公钥）。
	Validators [][]byte
	// MaxTxsPerBlock 单块交易上限，必须为正整数。
	MaxTxsPerBlock uint64
}

// entryStatus 与对外的 TxStatus 取值一致，额外区分内部存储。
type entryStatus string

const (
	stQueued    entryStatus = "queued"
	stProposed  entryStatus = "proposed"
	stConfirmed entryStatus = "confirmed"
	stReplaced  entryStatus = "replaced"
	stExpired   entryStatus = "expired"
)

type poolEntry struct {
	Tx         *Transaction `json:"tx"`
	Status     entryStatus  `json:"status"`
	ReplacedBy string       `json:"replaced_by,omitempty"`
}

type confirmedRef struct {
	Height uint64 `json:"height"`
	Block  string `json:"block_id"`
}

type proposalState struct {
	Round   uint64   `json:"round"`
	TxIDs   []string `json:"tx_ids"`
	BlockID string   `json:"block_id"`
	Votes   []string `json:"votes"` // 已投票验证者公钥的十六进制，按字典序保存
}

// state 是持久化到磁盘的完整节点状态。
type state struct {
	Version    int                          `json:"version"`
	Seed       []byte                       `json:"seed"`
	Validators [][]byte                     `json:"validators"`
	MaxTxs     uint64                       `json:"max_txs_per_block"`
	Round      uint64                       `json:"round"`
	LastBlock  string                       `json:"last_block_id"`
	Accounts   map[string]uint64            `json:"accounts"` // 发送者十六进制公钥 -> 已确认序号
	Pool       map[string]map[uint64]string `json:"pool"`     // 发送者 -> 序号 -> 交易标识（含已提议交易）
	Entries    map[string]*poolEntry        `json:"entries"`
	Confirmed  map[string]confirmedRef      `json:"confirmed"`
	Blocks     []Block                      `json:"blocks"`
	Proposal   *proposalState               `json:"proposal,omitempty"`
}

const stateVersion = 1
const stateFileName = "state.json"

// Node 是一个本地共识节点。一个进程内可串行调用其方法。
type Node struct {
	dir string
	mu  sync.Mutex
	st  *state
	// injectSaveErr 仅供同包测试使用：非 nil 时 save 直接失败，模拟写入错误。
	injectSaveErr error
}

func validateConfig(cfg Config) error {
	if len(cfg.Seed) == 0 {
		return errors.New("seed must be non-empty")
	}
	if len(cfg.Validators) == 0 {
		return errors.New("validators must be non-empty")
	}
	if cfg.MaxTxsPerBlock == 0 {
		return errors.New("max txs per block must be a positive integer")
	}
	seen := make(map[string]struct{}, len(cfg.Validators))
	for i, v := range cfg.Validators {
		if len(v) == 0 {
			return fmt.Errorf("validator %d is empty", i)
		}
		key := fmt.Sprintf("%x", v)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate validator %s", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// New 在 dir 中初始化一个新节点。目录不存在会创建；若已有状态文件则报错，
// 以免覆盖既有链历史，请改用 Open。
func New(dir string, cfg Config) (*Node, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, stateFileName)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("state file already exists at %s; use Open to resume", path)
	}
	validators := make([][]byte, len(cfg.Validators))
	for i, v := range cfg.Validators {
		validators[i] = append([]byte(nil), v...)
	}
	st := &state{
		Version:    stateVersion,
		Seed:       append([]byte(nil), cfg.Seed...),
		Validators: validators,
		MaxTxs:     cfg.MaxTxsPerBlock,
		Round:      1, // 轮次从 1 开始
		Accounts:   map[string]uint64{},
		Pool:       map[string]map[uint64]string{},
		Entries:    map[string]*poolEntry{},
		Confirmed:  map[string]confirmedRef{},
		Blocks:     []Block{},
	}
	n := &Node{dir: dir, st: st}
	if err := n.save(st); err != nil {
		return nil, err
	}
	return n, nil
}

// Open 从 dir 恢复此前由 New 创建的节点，恢复轮次、交易池、未决提议、票数与确认历史。
func Open(dir string) (*Node, error) {
	path := filepath.Join(dir, stateFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := &state{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", st.Version)
	}
	if st.Round == 0 {
		st.Round = 1
	}
	return &Node{dir: dir, st: st}, nil
}

// ConfigSnapshot 返回节点的固定配置副本。
type ConfigSnapshot struct {
	Seed           []byte
	Validators     [][]byte
	MaxTxsPerBlock uint64
}

// Config 返回初始化时的配置副本。
func (n *Node) Config() ConfigSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	vals := make([][]byte, len(n.st.Validators))
	for i, v := range n.st.Validators {
		vals[i] = append([]byte(nil), v...)
	}
	return ConfigSnapshot{
		Seed:           append([]byte(nil), n.st.Seed...),
		Validators:     vals,
		MaxTxsPerBlock: n.st.MaxTxs,
	}
}

// CurrentRound 返回当前轮次，首轮为 1。
func (n *Node) CurrentRound() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st.Round
}

// proposerAt 是确定性的轮次提议者选择：
// 对 seed || round(大端 8 字节) 取 SHA-256，结果对名单人数取模。
// 种子、名单顺序与轮次相同时，提议者必然相同。
func proposerAt(seed []byte, validators [][]byte, round uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], round)
	h := sha256.New()
	h.Write(seed)
	h.Write(buf[:])
	sum := h.Sum(nil)
	idx := binary.BigEndian.Uint64(sum[:8]) % uint64(len(validators))
	return validators[idx]
}

// ProposerFor 返回指定轮次的提议者公钥。
func (n *Node) ProposerFor(round uint64) []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]byte(nil), proposerAt(n.st.Seed, n.st.Validators, round)...)
}

// CurrentProposer 返回当前轮次的提议者公钥。
func (n *Node) CurrentProposer() []byte {
	return n.ProposerFor(n.CurrentRound())
}

// SubmitResult 是交易提交结果。
type SubmitResult struct {
	// TxID 被接受交易的标识。
	TxID string
	// ReplacedID 若本次提交替换了旧交易，为旧交易标识；否则为空。
	ReplacedID string
}

// Submit 校验并接收一笔交易进入交易池。
// 拒绝时返回 *RejectError，可通过其 Reason 取得具体原因。
func (n *Node) Submit(tx *Transaction) (*SubmitResult, error) {
	if tx == nil {
		return nil, errors.New("nil transaction")
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	cur := n.st
	if tx.Sequence == 0 {
		return nil, reject("sequence-must-be-positive", "sequence must be a positive integer")
	}
	if !tx.Verify() {
		return nil, reject(ReasonBadSignature, "signature verification failed for sender %x", tx.Sender)
	}
	id := tx.ID()
	// 同一标识的交易再次提交，一律视为重复（即使此前已确认、被替换或过期）。
	if _, ok := cur.Entries[id]; ok {
		return nil, reject(ReasonDuplicate, "transaction %s already exists", id)
	}
	if cur.Round >= tx.Expiry {
		return nil, reject(ReasonExpired, "expiry round %d is not greater than current round %d", tx.Expiry, cur.Round)
	}
	sender := tx.SenderHex()
	if confirmed := cur.Accounts[sender]; tx.Sequence <= confirmed {
		return nil, reject(ReasonOldSequence, "sequence %d is not greater than confirmed sequence %d", tx.Sequence, confirmed)
	}
	seqs := cur.Pool[sender]
	if seqs != nil {
		if oldID, ok := seqs[tx.Sequence]; ok {
			old := cur.Entries[oldID]
			if old.Status == stProposed {
				return nil, reject(ReasonProposalLocked, "sequence %d is already in the current proposal and cannot be replaced", tx.Sequence)
			}
			if tx.Fee <= old.Tx.Fee {
				return nil, reject(ReasonLowFee, "new fee %d is not strictly higher than existing fee %d", tx.Fee, old.Tx.Fee)
			}
		}
	}

	// 校验通过后在副本上修改，落盘成功才替换内存，失败时确认前状态保持不变。
	st := cur.clone()
	result := &SubmitResult{TxID: id}
	if oldID, ok := st.Pool[sender][tx.Sequence]; ok {
		old := st.Entries[oldID]
		old.Status = stReplaced
		old.ReplacedBy = id
		result.ReplacedID = oldID
	}

	txCopy := *tx
	txCopy.Sender = append([]byte(nil), tx.Sender...)
	txCopy.Content = append([]byte(nil), tx.Content...)
	txCopy.Signature = append([]byte(nil), tx.Signature...)
	st.Entries[id] = &poolEntry{Tx: &txCopy, Status: stQueued}
	if st.Pool[sender] == nil {
		st.Pool[sender] = map[uint64]string{}
	}
	st.Pool[sender][tx.Sequence] = id

	if err := n.save(st); err != nil {
		return nil, err
	}
	n.st = st
	return result, nil
}

// 额外拒绝原因。
const (
	// ReasonProposalLocked 同发送者同序号交易已在当前提议中，禁止替换。
	ReasonProposalLocked = "tx-in-proposal"
	// ReasonUnknownTx 查询的交易标识不存在。
	ReasonUnknownTx = "unknown-transaction"
	// ReasonUnknownBlock 查询的区块高度不存在。
	ReasonUnknownBlock = "unknown-block"
)

// ProposalView 是当前轮次提议的只读视图。
type ProposalView struct {
	Round   uint64
	BlockID string
	TxIDs   []string
	// Empty 表示本轮是否为空块提议（无交易）。
	Empty bool
}

// Proposal 返回当前轮次提议；尚未提议时第二个返回值为 false。
// 重复调用返回同一内容。
func (n *Node) Proposal() (ProposalView, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.st.Proposal == nil {
		return ProposalView{}, false
	}
	ids := append([]string(nil), n.st.Proposal.TxIDs...)
	return ProposalView{
		Round:   n.st.Proposal.Round,
		BlockID: n.st.Proposal.BlockID,
		TxIDs:   ids,
		Empty:   len(ids) == 0,
	}, true
}

// Propose 返回当前轮次唯一的提议；重复请求返回同一提议。
// 提议生成后即冻结：此后新提交的交易不改变该提议，提议中的交易禁止替换。
func (n *Node) Propose() (ProposalView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.st.Proposal != nil {
		return n.proposalView(n.st.Proposal), nil
	}

	st := n.st.clone()
	txIDs := st.selectTransactions()
	height := uint64(len(st.Blocks)) + 1
	blockID := BlockID(st.Round, height, st.LastBlock, txIDs)
	p := &proposalState{
		Round:   st.Round,
		TxIDs:   txIDs,
		BlockID: blockID,
		Votes:   []string{},
	}
	st.Proposal = p
	for _, id := range txIDs {
		st.Entries[id].Status = stProposed
	}
	if err := n.save(st); err != nil {
		return ProposalView{}, err
	}
	n.st = st
	return n.proposalView(p), nil
}

func (n *Node) proposalView(p *proposalState) ProposalView {
	ids := append([]string(nil), p.TxIDs...)
	return ProposalView{Round: p.Round, BlockID: p.BlockID, TxIDs: ids, Empty: len(ids) == 0}
}

// selectTransactions 执行确定性打包：
// 每轮从各账户“下一条可确认”的交易中选费用最高者，费用相同按交易标识字典序；
// 选入某账户后才继续考虑其后续序号，直到达到上限或没有可选交易。
func (n *state) selectTransactions() []string {
	var picked []string
	// 每个账户在本次打包中下一个可考虑的序号。
	next := map[string]uint64{}
	for sender := range n.Pool {
		next[sender] = n.Accounts[sender] + 1
	}
	for uint64(len(picked)) < n.MaxTxs {
		var bestID string
		var bestFee uint64
		var bestSet bool
		// 遍历发送者名单需确定序：按发送者十六进制定序，保证选择只取决于费用与标识。
		senders := make([]string, 0, len(n.Pool))
		for s := range n.Pool {
			senders = append(senders, s)
		}
		sort.Strings(senders)
		for _, sender := range senders {
			id, ok := n.Pool[sender][next[sender]]
			if !ok {
				continue
			}
			e := n.Entries[id]
			if e.Status != stQueued {
				continue
			}
			if !bestSet || e.Tx.Fee > bestFee || (e.Tx.Fee == bestFee && id < bestID) {
				bestID = id
				bestFee = e.Tx.Fee
				bestSet = true
			}
		}
		if !bestSet {
			break
		}
		picked = append(picked, bestID)
		next[n.Entries[bestID].Tx.SenderHex()]++
	}
	if picked == nil {
		picked = []string{}
	}
	return picked
}

// VoteResult 是投票处理结果。
type VoteResult struct {
	// Counted 本次投票是否新计入票数（重复投同一票为 false）。
	Counted bool
	// Confirmed 本次投票是否令区块立即确认。
	Confirmed bool
	// Block 确认时为新区块；否则为 nil。
	Block *Block
}

// Vote 处理验证者投票。票数严格超过名单人数三分之二时立即确认。
// 名单外身份、错误轮次、错误区块标识返回带具体 Reason 的 *RejectError；
// 重复投同一票不增加票数但不报错。
func (n *Node) Vote(validator []byte, round uint64, blockID string) (*VoteResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	key := fmt.Sprintf("%x", validator)
	allowed := false
	for _, v := range n.st.Validators {
		if fmt.Sprintf("%x", v) == key {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, reject(ReasonNotValidator, "validator %s is not in the validator set", key)
	}
	// 轮次优先判定：旧轮次投票不能影响新提议，即使新轮次尚未产生提议。
	if n.st.Proposal == nil {
		if round != n.st.Round {
			return nil, reject(ReasonWrongRound, "vote round %d does not match current round %d", round, n.st.Round)
		}
		return nil, reject(ReasonNoProposal, "round %d has no proposal", n.st.Round)
	}
	if round != n.st.Proposal.Round {
		return nil, reject(ReasonWrongRound, "vote round %d does not match current round %d", round, n.st.Proposal.Round)
	}
	if blockID != n.st.Proposal.BlockID {
		return nil, reject(ReasonWrongBlock, "block id %s does not match proposal block id %s", blockID, n.st.Proposal.BlockID)
	}

	for _, v := range n.st.Proposal.Votes {
		if v == key {
			return &VoteResult{Counted: false}, nil // 重复投票不增加票数
		}
	}

	st := n.st.clone()
	st.Proposal.Votes = append(st.Proposal.Votes, key)
	sort.Strings(st.Proposal.Votes)

	res := &VoteResult{Counted: true}
	threshold := len(st.Validators) * 2 / 3
	if len(st.Proposal.Votes) > threshold {
		block := st.confirmProposal()
		res.Confirmed = true
		res.Block = &block
	}
	if err := n.save(st); err != nil {
		return nil, err
	}
	n.st = st
	return res, nil
}

// confirmProposal 在内存状态上完整应用一次确认：
// 记录连续高度/前块标识/交易顺序，推进账户序号，移除已确认交易，进入下一轮，
// 并使新轮次中到期的池内交易失效。调用方随后通过一次原子保存落盘。
func (n *state) confirmProposal() Block {
	p := n.Proposal
	height := uint64(len(n.Blocks)) + 1
	block := Block{
		Height:     height,
		Round:      p.Round,
		ID:         p.BlockID,
		PreviousID: n.LastBlock,
		TxIDs:      append([]string(nil), p.TxIDs...),
	}
	if block.TxIDs == nil {
		block.TxIDs = []string{}
	}
	for _, id := range block.TxIDs {
		e := n.Entries[id]
		sender := e.Tx.SenderHex()
		// 打包保证序号连续且恰为下一条可确认序号。
		n.Accounts[sender] = e.Tx.Sequence
		delete(n.Pool[sender], e.Tx.Sequence)
		if len(n.Pool[sender]) == 0 {
			delete(n.Pool, sender)
		}
		e.Status = stConfirmed
		e.ReplacedBy = ""
		n.Confirmed[id] = confirmedRef{Height: height, Block: block.ID}
	}
	n.Blocks = append(n.Blocks, block)
	n.LastBlock = block.ID
	n.Proposal = nil
	n.Round++
	n.expire()
	return block
}

// expire 使池内到期轮次不大于当前轮次的交易失效。旧提议此时已不存在，
// 因此剩余池内交易均处于排队状态。
func (n *state) expire() {
	for sender, seqs := range n.Pool {
		for seq, id := range seqs {
			e := n.Entries[id]
			if e.Tx.Expiry <= n.Round {
				e.Status = stExpired
				delete(seqs, seq)
			}
		}
		if len(seqs) == 0 {
			delete(n.Pool, sender)
		}
	}
}

// EndRound 主动结束当前未确认轮次：该轮不留下确认块，
// 原提议中的交易回到排队状态，随后进入下一轮并使到期交易失效。
// 旧轮次投票随提议一并清除，不能影响新提议。
func (n *Node) EndRound() (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.st.clone()
	if st.Proposal != nil {
		for _, id := range st.Proposal.TxIDs {
			if e, ok := st.Entries[id]; ok {
				e.Status = stQueued
			}
		}
		st.Proposal = nil
	}
	st.Round++
	st.expire()
	if err := n.save(st); err != nil {
		return 0, err
	}
	n.st = st
	return st.Round, nil
}

// Height 返回已确认区块数（最新高度）。
func (n *Node) Height() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return uint64(len(n.st.Blocks))
}

// BlockAt 按连续高度查询确认块，高度从 1 开始。
func (n *Node) BlockAt(height uint64) (Block, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if height == 0 || height > uint64(len(n.st.Blocks)) {
		return Block{}, reject(ReasonUnknownBlock, "block at height %d does not exist", height)
	}
	return n.st.Blocks[height-1], nil
}

// LatestBlock 返回最新确认块；尚无确认块时第二返回值为 false。
func (n *Node) LatestBlock() (Block, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.st.Blocks) == 0 {
		return Block{}, false
	}
	return n.st.Blocks[len(n.st.Blocks)-1], true
}

// Tx 按交易标识查询其状态并关联区块或替代交易。
func (n *Node) Tx(id string) (*TxInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if e, ok := n.st.Entries[id]; ok {
		info := &TxInfo{
			ID:         id,
			Status:     TxStatus(e.Status),
			Tx:         copyTx(e.Tx),
			ReplacedBy: e.ReplacedBy,
		}
		if e.Status == stConfirmed {
			if ref, ok := n.st.Confirmed[id]; ok {
				info.BlockHeight = ref.Height
				info.BlockID = ref.Block
			}
		}
		return info, nil
	}
	return nil, reject(ReasonUnknownTx, "transaction %s not found", id)
}

// Account 按发送者公钥查询已确认序号与全部待处理交易。
// 因序号缺口等待时 Gap 给出最早缺少的序号；
// 已提议交易显示等待投票，其余池内交易显示等待打包。
func (n *Node) Account(sender []byte) *AccountInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := fmt.Sprintf("%x", sender)
	info := &AccountInfo{
		Sender:            key,
		ConfirmedSequence: n.st.Accounts[key],
		Pending:           []*TxInfo{},
	}
	seqs := n.st.Pool[key]
	if len(seqs) == 0 {
		return info
	}
	seqList := make([]uint64, 0, len(seqs))
	for seq := range seqs {
		seqList = append(seqList, seq)
	}
	sort.Slice(seqList, func(i, j int) bool { return seqList[i] < seqList[j] })
	expected := info.ConfirmedSequence + 1
	var gap uint64
	for _, seq := range seqList {
		if gap == 0 {
			if seq == expected {
				expected++
			} else if seq > expected {
				gap = expected
			}
		}
		id := seqs[seq]
		e := n.st.Entries[id]
		txInfo := &TxInfo{
			ID:     id,
			Status: TxStatus(e.Status),
			Tx:     copyTx(e.Tx),
		}
		if e.Status == stProposed {
			txInfo.Note = "waiting-vote"
		} else {
			txInfo.Note = "waiting-pack"
		}
		info.Pending = append(info.Pending, txInfo)
	}
	info.Gap = gap
	return info
}

func copyTx(tx *Transaction) *Transaction {
	if tx == nil {
		return nil
	}
	c := *tx
	c.Sender = append([]byte(nil), tx.Sender...)
	c.Content = append([]byte(nil), tx.Content...)
	c.Signature = append([]byte(nil), tx.Signature...)
	return &c
}
