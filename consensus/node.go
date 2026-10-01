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

// candidateState 是一轮中的一个候选块。同一轮多个候选共享相同的轮次、
// 下一高度与前块标识，仅交易顺序可以不同。
type candidateState struct {
	TxIDs   []string `json:"tx_ids"`
	BlockID string   `json:"block_id"`
	Votes   []string `json:"votes"` // 已投票验证者公钥的十六进制，按字典序保存
	// Local 标记该候选是否为本节点 Propose 产生的本地提议。
	Local bool `json:"local"`
	// Decided 表示该候选已决出：胜出确认或在轮次结束时落选。
	Decided bool `json:"decided"`
	// Winner 仅在 Decided 时有意义：true 为胜出，false 为落选。
	Winner bool `json:"winner,omitempty"`
}

// roundState 记录一个轮次的全部候选与结果。轮次决出后仍保留用于历史查询。
type roundState struct {
	Round        uint64                     `json:"round"`
	Height       uint64                     `json:"height"`     // 该轮候选使用的下一高度
	PreviousID   string                     `json:"prev_id"`    // 该轮候选接在其后的最新确认块
	Candidates   map[string]*candidateState `json:"candidates"` // 区块标识 -> 候选
	LocalBlockID string                     `json:"local_block_id"`
	// Ended 本轮已决出：候选确认或被 EndRound 主动结束。
	Ended bool `json:"ended"`
	// EndedByConfirm 决出方式：true 为投票确认，false 为主动结束（全部落选）。
	EndedByConfirm bool `json:"ended_by_confirm,omitempty"`
	// DetailMissing 表示该轮候选详情由旧版本状态迁移而来、并未保存，
	// 查询时明确显示无记录；确认块本身不受影响。
	DetailMissing bool `json:"detail_missing,omitempty"`
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
	// Rounds 按轮次保存候选与结果，含已决出的历史轮次。
	Rounds map[uint64]*roundState `json:"rounds,omitempty"`
	// Proposal 仅用于读取版本 1 的旧状态文件；版本 2 起一律由 Rounds 表达。
	Proposal *proposalState `json:"proposal,omitempty"`
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
		Rounds:     map[uint64]*roundState{},
	}
	n := &Node{dir: dir, st: st}
	if err := n.save(st); err != nil {
		return nil, err
	}
	return n, nil
}

// Open 从 dir 恢复此前由 New 创建的节点，恢复轮次、交易池、未决候选、
// 每人已投的选择、票数与确认历史。版本 1 的旧状态目录仍可打开。
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
	if st.Version != 1 && st.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", st.Version)
	}
	if st.Round == 0 {
		st.Round = 1
	}
	if st.Rounds == nil {
		st.Rounds = map[uint64]*roundState{}
	}
	if st.Version == 1 {
		migrateV1(st)
	}
	return &Node{dir: dir, st: st}, nil
}

// migrateV1 把版本 1 状态迁移到当前版本：
// 未决提议与其票数恢复为当前轮次的本地候选；旧版本未保存候选详情，
// 历史轮次一律标记为无记录（其中确认轮仍可通过确认块查询），已有确认块不变。
func migrateV1(st *state) {
	st.Version = stateVersion
	// 1..Round-1 中每个已跳过的轮次都没有保存候选详情；
	// 无确认块即说明该轮经 EndRound 结束、未确认。
	for r := uint64(1); r < st.Round; r++ {
		if _, ok := st.Rounds[r]; ok {
			continue
		}
		st.Rounds[r] = &roundState{
			Round:         r,
			Candidates:    map[string]*candidateState{},
			Ended:         true,
			DetailMissing: true,
		}
	}
	for _, b := range st.Blocks {
		st.Rounds[b.Round] = &roundState{
			Round:          b.Round,
			Height:         b.Height,
			PreviousID:     b.PreviousID,
			Candidates:     map[string]*candidateState{},
			LocalBlockID:   b.ID,
			Ended:          true,
			EndedByConfirm: true,
			DetailMissing:  true,
		}
	}
	if st.Proposal != nil {
		p := st.Proposal
		rs := &roundState{
			Round:      p.Round,
			Height:     uint64(len(st.Blocks)) + 1,
			PreviousID: st.LastBlock,
			Candidates: map[string]*candidateState{
				p.BlockID: &candidateState{
					TxIDs:   append([]string{}, p.TxIDs...),
					BlockID: p.BlockID,
					Votes:   append([]string{}, p.Votes...),
					Local:   true,
				},
			},
			LocalBlockID: p.BlockID,
		}
		// 被未决提议引用的交易在版本 1 中已标记为 proposed，语义不变，原样保留。
		st.Rounds[p.Round] = rs
	}
	st.Proposal = nil
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
				return nil, reject(ReasonProposalLocked, "sequence %d is referenced by a pending candidate and cannot be replaced", tx.Sequence)
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
	// ReasonProposalLocked 同发送者同序号交易已被未决候选引用，禁止替换。
	ReasonProposalLocked = "tx-in-proposal"
	// ReasonUnknownTx 查询的交易标识不存在。
	ReasonUnknownTx = "unknown-transaction"
	// ReasonUnknownBlock 查询的区块高度不存在。
	ReasonUnknownBlock = "unknown-block"
	// ReasonTxNotInPool 候选引用了已退出交易池的交易（已确认、被替换或已过期）。
	ReasonTxNotInPool = "tx-not-in-pool"
	// ReasonDuplicateInList 候选列表中同一交易出现多次。
	ReasonDuplicateInList = "duplicate-in-list"
	// ReasonTooManyTxs 候选包含的交易超过单块上限。
	ReasonTooManyTxs = "too-many-transactions"
	// ReasonSequenceGap 候选中某账户的序号未从已确认序号加一开始连续递增。
	ReasonSequenceGap = "sequence-not-consecutive"
)

// currentRoundState 返回当前轮次状态；尚未产生本地提议时可能为 nil。
func (s *state) currentRoundState() *roundState {
	return s.Rounds[s.Round]
}

// ProposalView 是当前轮次本地提议的只读视图。
type ProposalView struct {
	Round   uint64
	BlockID string
	TxIDs   []string
	// Empty 表示本轮是否为空块提议（无交易）。
	Empty bool
}

// Proposal 返回当前轮次本地提议；尚未提议时第二个返回值为 false。
// 重复调用、登记其他候选均不改变本地提议。
func (n *Node) Proposal() (ProposalView, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	rs := n.st.currentRoundState()
	if rs == nil || rs.LocalBlockID == "" {
		return ProposalView{}, false
	}
	return proposalViewOf(rs.Round, rs.Candidates[rs.LocalBlockID]), true
}

func proposalViewOf(round uint64, c *candidateState) ProposalView {
	ids := append([]string(nil), c.TxIDs...)
	return ProposalView{Round: round, BlockID: c.BlockID, TxIDs: ids, Empty: len(ids) == 0}
}

// Propose 返回当前轮次唯一的本地提议；重复请求返回同一提议。
// 提议生成后即冻结：此后新提交的交易或新登记的候选都不改变该提议，
// 提议中的交易禁止替换。其他候选用 RegisterCandidate 另行登记。
func (n *Node) Propose() (ProposalView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if rs := n.st.currentRoundState(); rs != nil && rs.LocalBlockID != "" {
		return proposalViewOf(rs.Round, rs.Candidates[rs.LocalBlockID]), nil
	}

	st := n.st.clone()
	rs := st.Rounds[st.Round]
	if rs == nil {
		rs = &roundState{
			Round:      st.Round,
			Height:     uint64(len(st.Blocks)) + 1,
			PreviousID: st.LastBlock,
			Candidates: map[string]*candidateState{},
		}
		st.Rounds[st.Round] = rs
	}
	txIDs := st.selectTransactions()
	blockID := BlockID(st.Round, rs.Height, rs.PreviousID, txIDs)
	c := &candidateState{
		TxIDs:   txIDs,
		BlockID: blockID,
		Votes:   []string{},
		Local:   true,
	}
	rs.Candidates[blockID] = c
	rs.LocalBlockID = blockID
	for _, id := range txIDs {
		st.Entries[id].Status = stProposed
	}
	if err := n.save(st); err != nil {
		return ProposalView{}, err
	}
	n.st = st
	return proposalViewOf(rs.Round, c), nil
}

// CandidateView 是一个已登记候选的只读视图。
type CandidateView struct {
	// Round 候选所属轮次。
	Round uint64
	// BlockID 候选区块标识。
	BlockID string
	// TxIDs 候选中的交易顺序。
	TxIDs []string
	// Local 是否为本节点 Propose 产生的本地提议。
	Local bool
	// Voters 已投给该候选的验证者公钥，按公钥十六进制定序返回。
	Voters [][]byte
	// Result 候选结果："pending"（未决）、"won"（胜出）或 "lost"（落选）。
	Result string
}

// RegisterResult 是候选登记结果。
type RegisterResult struct {
	// BlockID 候选区块标识（沿用稳定区块标识编码）。
	BlockID string
	// Existing 为 true 时表示相同交易列表此前已登记，本次返回同一候选并保留已有票数。
	Existing bool
}

// RegisterCandidate 在当前轮次本地提议已产生且尚未确认时，按给定交易顺序登记一个
// 竞争候选，与本地提议一起参与投票。候选使用当前轮次与下一高度，接在最新确认块之后。
//
// 候选只能包含当前池内排队或已提议的交易，数量不得超过单块上限，允许空块；
// 列表不能重复交易；每个账户在列表中的序号必须从该账户已确认序号加一开始连续递增，
// 不同账户之间的顺序由调用者决定。相同列表重复登记返回同一候选并保留已有票数。
//
// 未产生本地提议、轮次不是当前轮次、引用未知或已退出池的交易、超过上限或序号
// 不连续时登记失败并返回带具体 Reason 的 *RejectError，节点状态不变。
func (n *Node) RegisterCandidate(round uint64, txIDs []string) (*RegisterResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	rs := n.st.currentRoundState()
	// 轮次优先判定：非当前轮次不能登记，即使当前轮次尚未产生本地提议。
	if round != n.st.Round {
		return nil, reject(ReasonWrongRound, "candidate round %d does not match current round %d", round, n.st.Round)
	}
	if rs == nil || rs.LocalBlockID == "" {
		return nil, reject(ReasonNoProposal, "round %d has no local proposal yet", n.st.Round)
	}

	ids := append([]string{}, txIDs...)
	if uint64(len(ids)) > n.st.MaxTxs {
		return nil, reject(ReasonTooManyTxs, "candidate contains %d transactions, max per block is %d", len(ids), n.st.MaxTxs)
	}
	// 列表内不得重复交易。
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return nil, reject(ReasonDuplicateInList, "transaction %s appears more than once in candidate list", id)
		}
		seen[id] = struct{}{}
		e, ok := n.st.Entries[id]
		if !ok {
			return nil, reject(ReasonUnknownTx, "transaction %s not found", id)
		}
		// 必须仍在池中（排队或已被某未决候选引用）；已确认、被替换、已过期均已退出池。
		if e.Status != stQueued && e.Status != stProposed {
			return nil, reject(ReasonTxNotInPool, "transaction %s is not queued or proposed (status %s)", id, e.Status)
		}
		if !inPool(n.st, e.Tx.SenderHex(), e.Tx.Sequence, id) {
			return nil, reject(ReasonTxNotInPool, "transaction %s has left the pool", id)
		}
	}
	// 每个账户的序号必须从已确认序号加一开始连续递增。
	seqsBySender := map[string][]uint64{}
	for _, id := range ids {
		e := n.st.Entries[id]
		sender := e.Tx.SenderHex()
		seqsBySender[sender] = append(seqsBySender[sender], e.Tx.Sequence)
	}
	senders := make([]string, 0, len(seqsBySender))
	for sender := range seqsBySender {
		senders = append(senders, sender)
	}
	sort.Strings(senders)
	for _, sender := range senders {
		seqs := seqsBySender[sender]
		want := n.st.Accounts[sender] + 1
		for i, seq := range seqs {
			if seq != want {
				return nil, reject(ReasonSequenceGap,
					"sender %s candidate sequence at position %d is %d, want %d (must be consecutive starting at confirmed sequence + 1)",
					sender, i, seq, want)
			}
			want++
		}
	}

	blockID := BlockID(rs.Round, rs.Height, rs.PreviousID, ids)
	if existing, ok := rs.Candidates[blockID]; ok {
		// 相同列表重复登记：返回同一候选，保留已有票数，节点状态不变。
		return &RegisterResult{BlockID: existing.BlockID, Existing: true}, nil
	}

	st := n.st.clone()
	rs2 := st.Rounds[st.Round]
	rs2.Candidates[blockID] = &candidateState{
		TxIDs:   ids,
		BlockID: blockID,
		Votes:   []string{},
	}
	for _, id := range ids {
		if st.Entries[id].Status == stQueued {
			st.Entries[id].Status = stProposed
		}
	}
	if err := n.save(st); err != nil {
		return nil, err
	}
	n.st = st
	return &RegisterResult{BlockID: blockID}, nil
}

// inPool 判断标识 id 是否仍是池中某账户某序号的当前交易。
func inPool(st *state, sender string, seq uint64, id string) bool {
	cur, ok := st.Pool[sender][seq]
	return ok && cur == id
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
	// Counted 本次投票是否新计入票数（重复投同一候选为 false）。
	Counted bool
	// Confirmed 本次投票是否令某候选立即确认。
	Confirmed bool
	// Block 确认时为新区块；否则为 nil。
	Block *Block
}

// Vote 处理验证者对当前轮次某已登记候选的投票。
// 每位验证者一轮内只能选择一个候选：重复投同一候选不增加票数但不报错；
// 改投其他候选明确拒绝（ReasonAlreadyVoted），原票保留。
// 票数严格超过名单人数三分之二时立即确认该候选，其他候选落选并进入下一轮。
// 名单外身份、错误轮次、未知区块标识返回带具体 Reason 的 *RejectError。
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
	rs := n.st.currentRoundState()
	// 轮次优先判定：旧轮次投票不能影响新轮次，即使新轮次尚未产生本地提议。
	if rs == nil || rs.LocalBlockID == "" {
		if round != n.st.Round {
			return nil, reject(ReasonWrongRound, "vote round %d does not match current round %d", round, n.st.Round)
		}
		return nil, reject(ReasonNoProposal, "round %d has no proposal", n.st.Round)
	}
	if round != rs.Round {
		return nil, reject(ReasonWrongRound, "vote round %d does not match current round %d", round, rs.Round)
	}
	if _, ok := rs.Candidates[blockID]; !ok {
		return nil, reject(ReasonWrongBlock, "block id %s is not a registered candidate in round %d", blockID, rs.Round)
	}

	// 每位验证者一轮内只能选择一个候选。
	for id, cand := range rs.Candidates {
		for _, vk := range cand.Votes {
			if vk == key {
				if id == blockID {
					return &VoteResult{Counted: false}, nil // 重复投同一候选不增票
				}
				return nil, reject(ReasonAlreadyVoted,
					"validator %s already voted for candidate %s in round %d and cannot switch to %s", key, id, rs.Round, blockID)
			}
		}
	}

	st := n.st.clone()
	rs2 := st.Rounds[st.Round]
	cand := rs2.Candidates[blockID]
	cand.Votes = append(cand.Votes, key)
	sort.Strings(cand.Votes)

	res := &VoteResult{Counted: true}
	threshold := len(st.Validators) * 2 / 3
	if len(cand.Votes) > threshold {
		block := st.resolveRound(cand.BlockID, true)
		res.Confirmed = true
		res.Block = &block
	}
	if err := n.save(st); err != nil {
		return nil, err
	}
	n.st = st
	return res, nil
}

// resolveRound 在内存状态上完整应用一轮的决出：
// confirm 为 true 时胜出候选按其交易顺序生成连续高度的确认块，随后胜出与落选
// 候选中引用的交易分别按规则处理（确认一次 / 回到排队），最后进入下一轮并使
// 到期交易失效。confirm 为 false（EndRound）时不生成确认块，所有候选引用的
// 交易全部回到排队。调用方随后通过一次原子保存落盘。
func (s *state) resolveRound(winnerID string, confirm bool) Block {
	rs := s.Rounds[s.Round]
	var block Block
	winnerTx := map[string]struct{}{}
	if confirm {
		winner := rs.Candidates[winnerID]
		height := uint64(len(s.Blocks)) + 1
		block = Block{
			Height:     height,
			Round:      rs.Round,
			ID:         winner.BlockID,
			PreviousID: s.LastBlock,
			TxIDs:      append([]string(nil), winner.TxIDs...),
		}
		if block.TxIDs == nil {
			block.TxIDs = []string{}
		}
		for _, id := range block.TxIDs {
			winnerTx[id] = struct{}{}
			e := s.Entries[id]
			sender := e.Tx.SenderHex()
			// 候选校验保证序号从已确认序号加一起连续递增。
			s.Accounts[sender] = e.Tx.Sequence
			delete(s.Pool[sender], e.Tx.Sequence)
			if len(s.Pool[sender]) == 0 {
				delete(s.Pool, sender)
			}
			e.Status = stConfirmed
			e.ReplacedBy = ""
			s.Confirmed[id] = confirmedRef{Height: height, Block: block.ID}
		}
		s.Blocks = append(s.Blocks, block)
		s.LastBlock = block.ID
	}
	// 标记全部候选的胜出/落选结果；只有未在胜出块中的交易才回到排队。
	// 被多个候选共享的胜出交易只确认一次，落选候选不能再改变其状态。
	for id, cand := range rs.Candidates {
		cand.Decided = true
		cand.Winner = confirm && id == winnerID
		if !cand.Winner {
			for _, txID := range cand.TxIDs {
				if _, won := winnerTx[txID]; won {
					continue
				}
				if e, ok := s.Entries[txID]; ok && e.Status == stProposed {
					e.Status = stQueued
				}
			}
		}
	}
	rs.Ended = true
	rs.EndedByConfirm = confirm
	s.Round++
	s.expire()
	return block
}

// expire 使池内到期轮次不大于当前轮次的交易失效。轮次已决出，池中剩余的
// 均为排队交易（落选候选独有或从未被引用）。
func (s *state) expire() {
	for sender, seqs := range s.Pool {
		for seq, id := range seqs {
			e := s.Entries[id]
			if e.Tx.Expiry <= s.Round {
				e.Status = stExpired
				delete(seqs, seq)
			}
		}
		if len(seqs) == 0 {
			delete(s.Pool, sender)
		}
	}
}

// EndRound 主动结束当前未确认轮次：所有候选落选且不生成确认块，
// 候选中的交易按落选规则回到排队状态，随后进入下一轮并使到期交易失效。
// 旧轮次投票随候选结果一并保留在历史中，但不能影响新轮次。
func (n *Node) EndRound() (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.st.clone()
	rs := st.Rounds[st.Round]
	if rs == nil {
		// 本轮尚未产生本地提议：仍允许直接结束，空轮留作未确认结束记录。
		rs = &roundState{
			Round:      st.Round,
			Height:     uint64(len(st.Blocks)) + 1,
			PreviousID: st.LastBlock,
			Candidates: map[string]*candidateState{},
		}
		st.Rounds[st.Round] = rs
	}
	st.resolveRound("", false)
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

// 候选结果取值。
const (
	// CandidatePending 候选尚未决出，仍可接受投票。
	CandidatePending = "pending"
	// CandidateWon 候选胜出，其交易已确认。
	CandidateWon = "won"
	// CandidateLost 轮次已结束且该候选落选。
	CandidateLost = "lost"
)

// RoundCandidates 是某一轮候选情况的只读视图。返回内容均为副本，
// 调用方修改切片或字节不会影响节点内部状态。
type RoundCandidates struct {
	// Round 查询的轮次。
	Round uint64
	// Candidates 本轮全部候选，按区块标识字典序排列。
	Candidates []CandidateView
	// Unvoted 本轮尚未投票的验证者公钥，按公钥字典序排列。
	Unvoted [][]byte
	// Ended 本轮是否已经决出（确认或主动结束）。
	Ended bool
	// UnconfirmedEnd 为 true 表示本轮由 EndRound 主动结束、未产生确认块。
	UnconfirmedEnd bool
	// HasRecords 为 false 表示旧版本状态未保存该轮候选详情；
	// 此时 Candidates 为空且不代表“该轮没有候选”。
	HasRecords bool
}

// Candidates 按轮次查看候选：列出每个候选的交易顺序、投票者（按公钥排序）
// 及未决/胜出/落选结果，并列出本轮尚未投票的验证者。旧轮记录保留可查；
// 轮次为 0 或尚未到达时返回 ReasonUnknownRound。
//
// 候选按区块标识排序，身份按公钥排序。主动结束的轮次 UnconfirmedEnd 为 true。
// 过去没有保存候选详情的旧轮 HasRecords 为 false。
func (n *Node) Candidates(round uint64) (*RoundCandidates, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	// 轮次 0、尚未到达的未来轮次以及尚未产生本地提议的当前轮次均无记录。
	// 历史轮次（含旧版本迁移轮）与未决当前轮一定有条目。
	rs := n.st.Rounds[round]
	if round == 0 || rs == nil {
		return nil, reject(ReasonUnknownRound, "round %d does not exist (current round %d)", round, n.st.Round)
	}
	view := &RoundCandidates{
		Round:          round,
		Candidates:     []CandidateView{},
		Unvoted:        [][]byte{},
		Ended:          rs.Ended,
		UnconfirmedEnd: rs.Ended && !rs.EndedByConfirm,
		HasRecords:     !rs.DetailMissing,
	}
	if !rs.DetailMissing {
		ids := make([]string, 0, len(rs.Candidates))
		for id := range rs.Candidates {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		voted := map[string]struct{}{}
		for _, id := range ids {
			c := rs.Candidates[id]
			voters := make([][]byte, 0, len(c.Votes))
			for _, vk := range c.Votes {
				pub, err := hexDecode(vk)
				if err != nil {
					return nil, fmt.Errorf("corrupt voter key %q: %w", vk, err)
				}
				voters = append(voters, pub)
				voted[vk] = struct{}{}
			}
			result := CandidatePending
			if c.Decided {
				if c.Winner {
					result = CandidateWon
				} else {
					result = CandidateLost
				}
			}
			view.Candidates = append(view.Candidates, CandidateView{
				Round:   rs.Round,
				BlockID: c.BlockID,
				TxIDs:   append([]string(nil), c.TxIDs...),
				Local:   c.Local,
				Voters:  voters,
				Result:  result,
			})
		}
		valKeys := make([]string, 0, len(n.st.Validators))
		for _, v := range n.st.Validators {
			k := fmt.Sprintf("%x", v)
			if _, ok := voted[k]; !ok {
				valKeys = append(valKeys, k)
			}
		}
		sort.Strings(valKeys)
		for _, k := range valKeys {
			pub, err := hexDecode(k)
			if err != nil {
				return nil, fmt.Errorf("corrupt validator key %q: %w", k, err)
			}
			view.Unvoted = append(view.Unvoted, pub)
		}
	}
	return view, nil
}

// hexDecode 解码十六进制公钥，非法输入返回错误。
func hexDecode(h string) ([]byte, error) {
	return hex.DecodeString(h)
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
