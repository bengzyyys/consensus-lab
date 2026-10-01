package consensus

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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

// candidateState 是当前轮次的一个候选块。由 Propose 产生的本地候选 Local 为 true，
// 其余候选由 RegisterCandidate 登记。Votes 为已投票验证者公钥的十六进制，按字典序保存。
type candidateState struct {
	BlockID string   `json:"block_id"`
	TxIDs   []string `json:"tx_ids"`
	Votes   []string `json:"votes"`
	Local   bool     `json:"local,omitempty"`
}

// candidateRecord 是已结束轮次中一个候选的留档。
type candidateRecord struct {
	BlockID string   `json:"block_id"`
	TxIDs   []string `json:"tx_ids"`
	Votes   []string `json:"votes"`
	Local   bool     `json:"local"`
	Status  string   `json:"status"` // "won" 或 "lost"
}

// roundRecord 是已结束轮次的候选留档，供按轮次查询。
type roundRecord struct {
	Round      uint64             `json:"round"`
	Candidates []*candidateRecord `json:"candidates"`
	Ended      bool               `json:"ended"`                 // 主动结束，未确认任何区块
	WonBlockID string             `json:"won_block_id,omitempty"` // 胜出候选区块标识
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
	Proposal   *proposalState               `json:"proposal,omitempty"` // 仅用于读取 v1 状态；新状态不再写入
	Candidates []*candidateState            `json:"candidates,omitempty"`
	History    []*roundRecord               `json:"history,omitempty"`
}

const stateVersion = 2
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

// Open 从 dir 恢复此前由 New 创建的节点，恢复轮次、交易池、未决候选、
// 每人已投的选择与确认历史。v1 状态目录仍可打开：未决提议与票数作为本地候选恢复，
// 但旧轮次没有保存候选详情，查询时明确显示无记录。
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
	if st.Version == 1 {
		// 迁移：v1 的未决提议成为当前轮的本地候选，票数一并保留。
		if st.Proposal != nil && len(st.Candidates) == 0 {
			st.Candidates = []*candidateState{{
				BlockID: st.Proposal.BlockID,
				TxIDs:   append([]string(nil), st.Proposal.TxIDs...),
				Votes:   append([]string(nil), st.Proposal.Votes...),
				Local:   true,
			}}
		}
		st.Proposal = nil
		st.Version = 2
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

// localCandidate 返回当前轮由 Propose 产生的本地候选；不存在时返回 nil。
func (n *Node) localCandidate() *candidateState {
	for _, c := range n.st.Candidates {
		if c.Local {
			return c
		}
	}
	return nil
}

// Proposal 返回当前轮次的本地提议；尚未提议时第二个返回值为 false。
// 重复调用返回同一内容；登记其他候选不会改变本地提议。
func (n *Node) Proposal() (ProposalView, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.localCandidate()
	if c == nil {
		return ProposalView{}, false
	}
	return proposalView(c, n.st.Round), true
}

// Propose 返回当前轮次唯一的本地提议；重复请求返回同一提议。
// 提议生成后即冻结：此后新提交的交易不改变该提议，提议中的交易禁止替换。
// 本地提议是当前轮的候选之一，可与 RegisterCandidate 登记的其他候选一起参与投票。
func (n *Node) Propose() (ProposalView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c := n.localCandidate(); c != nil {
		return proposalView(c, n.st.Round), nil
	}

	st := n.st.clone()
	txIDs := st.selectTransactions()
	height := uint64(len(st.Blocks)) + 1
	blockID := BlockID(st.Round, height, st.LastBlock, txIDs)
	cand := &candidateState{
		BlockID: blockID,
		TxIDs:   txIDs,
		Votes:   []string{},
		Local:   true,
	}
	st.Candidates = append(st.Candidates, cand)
	for _, id := range txIDs {
		st.Entries[id].Status = stProposed
	}
	if err := n.save(st); err != nil {
		return ProposalView{}, err
	}
	n.st = st
	return proposalView(cand, n.st.Round), nil
}

func proposalView(c *candidateState, round uint64) ProposalView {
	ids := append([]string(nil), c.TxIDs...)
	return ProposalView{Round: round, BlockID: c.BlockID, TxIDs: ids, Empty: len(ids) == 0}
}

// candidateView 构造候选的只读视图，深拷贝交易标识与投票者公钥，
// 调用者修改返回结果不会影响节点状态。
func candidateView(c *candidateState, status CandidateStatus) CandidateView {
	ids := append([]string(nil), c.TxIDs...)
	voters := make([][]byte, 0, len(c.Votes))
	for _, v := range c.Votes {
		b, _ := hex.DecodeString(v)
		voters = append(voters, b)
	}
	return CandidateView{
		BlockID: c.BlockID,
		TxIDs:   ids,
		Voters:  voters,
		Status:  status,
		Local:   c.Local,
	}
}

// RegisterCandidate 在当前轮已有本地提议且尚未确认时，按调用者给定的交易顺序
// 登记一个候选块，与本地提议一起参与投票。候选使用当前轮次与下一高度，接在最新
// 确认块之后，不改变已确认历史。
//
// 候选中的交易必须是当前池内排队或已提议的交易；数量不得超过单块上限，允许空块；
// 列表不得重复交易；同一账户的序号必须从该账户已确认序号加一开始连续递增（账户
// 之间的顺序由调用者决定）。
//
// 未产生本地提议、指定非当前轮次、引用未知或已退出池的交易、超过上限或序号不连续
// 时登记失败，返回带具体 Reason 的 *RejectError，节点状态不变。相同列表重复登记
// 返回同一候选并保留已有票数。
func (n *Node) RegisterCandidate(round uint64, txIDs []string) (CandidateView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.localCandidate() == nil {
		return CandidateView{}, reject(ReasonNoProposal, "round %d has no proposal", n.st.Round)
	}
	if round != n.st.Round {
		return CandidateView{}, reject(ReasonWrongRound, "candidate round %d does not match current round %d", round, n.st.Round)
	}

	// 列表不得重复交易。
	seen := make(map[string]bool, len(txIDs))
	for _, id := range txIDs {
		if seen[id] {
			return CandidateView{}, reject(ReasonDuplicateTxInCandidate, "transaction %s appears more than once in candidate", id)
		}
		seen[id] = true
	}
	// 交易必须存在且当前在池中（排队或已提议）。
	for _, id := range txIDs {
		e, ok := n.st.Entries[id]
		if !ok {
			return CandidateView{}, reject(ReasonUnknownTx, "transaction %s not found", id)
		}
		if e.Status != stQueued && e.Status != stProposed {
			return CandidateView{}, reject(ReasonTxNotInPool, "transaction %s is not in pool (status %s)", id, e.Status)
		}
	}
	// 数量不得超过单块上限。
	if uint64(len(txIDs)) > n.st.MaxTxs {
		return CandidateView{}, reject(ReasonTooManyTxs, "candidate has %d transactions, exceeds max %d", len(txIDs), n.st.MaxTxs)
	}
	// 同一账户的序号必须从已确认序号加一开始连续递增。
	senderSeqs := make(map[string][]uint64)
	for _, id := range txIDs {
		sender := n.st.Entries[id].Tx.SenderHex()
		senderSeqs[sender] = append(senderSeqs[sender], n.st.Entries[id].Tx.Sequence)
	}
	for sender, seqs := range senderSeqs {
		expected := n.st.Accounts[sender] + 1
		for _, seq := range seqs {
			if seq != expected {
				return CandidateView{}, reject(ReasonSequenceGap, "sender %s sequence %d is not consecutive after confirmed %d", sender, seq, expected-1)
			}
			expected++
		}
	}

	height := uint64(len(n.st.Blocks)) + 1
	blockID := BlockID(n.st.Round, height, n.st.LastBlock, txIDs)
	// 相同列表（同一区块标识）重复登记返回同一候选，保留已有票数。
	for _, c := range n.st.Candidates {
		if c.BlockID == blockID {
			return candidateView(c, CandidatePending), nil
		}
	}

	st := n.st.clone()
	cand := &candidateState{
		BlockID: blockID,
		TxIDs:   append([]string(nil), txIDs...),
		Votes:   []string{},
	}
	st.Candidates = append(st.Candidates, cand)
	for _, id := range txIDs {
		st.Entries[id].Status = stProposed
	}
	if err := n.save(st); err != nil {
		return CandidateView{}, err
	}
	n.st = st
	return candidateView(cand, CandidatePending), nil
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

// Vote 处理验证者投票。票数严格超过名单人数三分之二时立即确认该候选，
// 其他候选落选，随后进入下一轮。
// 名单外身份、错误轮次、未登记的区块标识返回带具体 Reason 的 *RejectError；
// 重复投同一候选不增加票数但不报错；改投其他候选明确拒绝，原票保留。
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
	if round != n.st.Round {
		return nil, reject(ReasonWrongRound, "vote round %d does not match current round %d", round, n.st.Round)
	}
	var cand *candidateState
	for _, c := range n.st.Candidates {
		if c.BlockID == blockID {
			cand = c
			break
		}
	}
	if cand == nil {
		return nil, reject(ReasonWrongBlock, "block id %s is not a registered candidate in round %d", blockID, round)
	}

	for _, v := range cand.Votes {
		if v == key {
			return &VoteResult{Counted: false}, nil // 重复投同一候选不增加票数
		}
	}
	// 改投其他候选明确拒绝，原票保留。
	for _, c := range n.st.Candidates {
		if c.BlockID == blockID {
			continue
		}
		for _, v := range c.Votes {
			if v == key {
				return nil, reject(ReasonAlreadyVoted, "validator %s already voted for candidate %s in round %d", key, c.BlockID, round)
			}
		}
	}

	st := n.st.clone()
	var cloneCand *candidateState
	for _, c := range st.Candidates {
		if c.BlockID == blockID {
			cloneCand = c
			break
		}
	}
	cloneCand.Votes = append(cloneCand.Votes, key)
	sort.Strings(cloneCand.Votes)

	res := &VoteResult{Counted: true}
	threshold := len(st.Validators) * 2 / 3
	if len(cloneCand.Votes) > threshold {
		block := st.confirmCandidate(cloneCand)
		res.Confirmed = true
		res.Block = &block
	}
	if err := n.save(st); err != nil {
		return nil, err
	}
	n.st = st
	return res, nil
}

// confirmCandidate 在内存状态上完整应用一次候选确认：
// 胜出候选中的交易确认一次（推进账户序号、移出池中、标记已确认）；共享这些交易的
// 落选候选不再改变其状态；落选候选独有的交易回到排队状态。随后记录候选留档、
// 进入下一轮并使到期的池内交易失效。调用方随后通过一次原子保存落盘。
func (n *state) confirmCandidate(winner *candidateState) Block {
	height := uint64(len(n.Blocks)) + 1
	block := Block{
		Height:     height,
		Round:      n.Round,
		ID:         winner.BlockID,
		PreviousID: n.LastBlock,
		TxIDs:      append([]string(nil), winner.TxIDs...),
	}
	if block.TxIDs == nil {
		block.TxIDs = []string{}
	}
	won := make(map[string]bool, len(winner.TxIDs))
	for _, id := range block.TxIDs {
		won[id] = true
		e := n.Entries[id]
		sender := e.Tx.SenderHex()
		// 候选列表保证序号连续且从已确认序号加一开始。
		n.Accounts[sender] = e.Tx.Sequence
		delete(n.Pool[sender], e.Tx.Sequence)
		if len(n.Pool[sender]) == 0 {
			delete(n.Pool, sender)
		}
		e.Status = stConfirmed
		e.ReplacedBy = ""
		n.Confirmed[id] = confirmedRef{Height: height, Block: winner.BlockID}
	}
	// 落选候选独有的交易回到排队状态；共享胜出交易的保持已确认不变。
	for _, c := range n.Candidates {
		if c.BlockID == winner.BlockID {
			continue
		}
		for _, id := range c.TxIDs {
			if won[id] {
				continue
			}
			e := n.Entries[id]
			if e.Status == stProposed {
				e.Status = stQueued
			}
		}
	}
	n.appendHistory(winner.BlockID, false)
	n.Blocks = append(n.Blocks, block)
	n.LastBlock = winner.BlockID
	n.Candidates = nil
	n.Round++
	n.expire()
	return block
}

// appendHistory 把当前轮的候选留档追加到历史。胜出候选状态为 won，其余为 lost；
// 主动结束的轮次所有候选均为 lost 且 Ended 为 true。
func (n *state) appendHistory(wonBlockID string, ended bool) {
	rec := &roundRecord{
		Round:      n.Round,
		Ended:      ended,
		WonBlockID: wonBlockID,
		Candidates: make([]*candidateRecord, 0, len(n.Candidates)),
	}
	for _, c := range n.Candidates {
		status := "lost"
		if c.BlockID == wonBlockID {
			status = "won"
		}
		rec.Candidates = append(rec.Candidates, &candidateRecord{
			BlockID: c.BlockID,
			TxIDs:   append([]string(nil), c.TxIDs...),
			Votes:   append([]string(nil), c.Votes...),
			Local:   c.Local,
			Status:  status,
		})
	}
	n.History = append(n.History, rec)
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

// EndRound 主动结束当前未确认轮次：所有候选落选且不留下确认块，
// 候选中的交易回到排队状态（进入新轮次时到期的显示过期），随后进入下一轮。
// 该轮作为“未确认结束”留档，旧投票不影响新提议。
func (n *Node) EndRound() (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.st.clone()
	for _, c := range st.Candidates {
		for _, id := range c.TxIDs {
			if e, ok := st.Entries[id]; ok && e.Status == stProposed {
				e.Status = stQueued
			}
		}
	}
	st.appendHistory("", true)
	st.Candidates = nil
	st.Round++
	st.expire()
	if err := n.save(st); err != nil {
		return 0, err
	}
	n.st = st
	return st.Round, nil
}

// RoundCandidates 按轮次查询候选：列出每个候选的交易顺序、投票者及未决/胜出/
// 落选结果，并列出本轮尚未投票的验证者；主动结束的轮次明确显示未确认结束。
// 候选按区块标识排序，投票者与未投票验证者按公钥排序。返回结果为深拷贝，
// 调用者修改不影响节点状态。
func (n *Node) RoundCandidates(round uint64) (RoundView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	view := RoundView{Round: round, Candidates: []*CandidateView{}}
	var src []*candidateState
	if round == n.st.Round {
		// 当前轮：候选均未决。
		for _, c := range n.st.Candidates {
			src = append(src, c)
		}
	} else if round < n.st.Round {
		// 历史轮：从留档读取。
		var rec *roundRecord
		for _, h := range n.st.History {
			if h.Round == round {
				rec = h
				break
			}
		}
		if rec == nil {
			return RoundView{}, reject(ReasonUnknownRound, "round %d has no candidate records", round)
		}
		view.Ended = rec.Ended
		view.Confirmed = rec.WonBlockID != ""
		view.WonBlockID = rec.WonBlockID
		for _, cr := range rec.Candidates {
			status := CandidateLost
			if cr.Status == "won" {
				status = CandidateWon
			}
			cv := candidateView(&candidateState{
				BlockID: cr.BlockID,
				TxIDs:   cr.TxIDs,
				Votes:   cr.Votes,
				Local:   cr.Local,
			}, status)
			view.Candidates = append(view.Candidates, &cv)
		}
	} else {
		return RoundView{}, reject(ReasonUnknownRound, "round %d has not been reached (current %d)", round, n.st.Round)
	}

	if round == n.st.Round {
		for _, c := range src {
			cv := candidateView(c, CandidatePending)
			view.Candidates = append(view.Candidates, &cv)
		}
	}

	// 候选按区块标识排序。
	sort.Slice(view.Candidates, func(i, j int) bool {
		return view.Candidates[i].BlockID < view.Candidates[j].BlockID
	})

	// 尚未投票的验证者：名单中未在任何候选投票者里出现的人，按公钥排序。
	voted := make(map[string]bool)
	for _, cv := range view.Candidates {
		for _, v := range cv.Voters {
			voted[fmt.Sprintf("%x", v)] = true
		}
	}
	for _, v := range n.st.Validators {
		if !voted[fmt.Sprintf("%x", v)] {
			view.NotVoted = append(view.NotVoted, append([]byte(nil), v...))
		}
	}
	sort.Slice(view.NotVoted, func(i, j int) bool {
		return string(view.NotVoted[i]) < string(view.NotVoted[j])
	})
	return view, nil
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
