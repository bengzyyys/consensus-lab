package consensus

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// capProtectScene 是“满池中等待投票与排队交易混合存在”的固定场景：
//
//	keys[0] lp     费用 10：仅被本地提议引用（本地提议独有）
//	keys[1] cp     费用 20：仅被竞争候选引用（其他已登记候选独有）
//	keys[2] shared 费用 30：同时被本地提议与竞争候选引用（多候选共享）
//	keys[3] q1..q3 费用 50/60/70：始终排队，q1 是唯一可被淘汰的最低费交易
//	keys[4]：提交新交易使用的“其他发送者”
//
// 容量为 6，场景建成后池恰好满：三笔受保护交易占三个位置，三笔排队交易占三个
// 位置。竞争候选已取得一票，本地提议、候选顺序、区块标识与票数在场景内冻结。
type capProtectScene struct {
	n      *Node
	keys   []testKey
	lp     *Transaction
	cp     *Transaction
	shared *Transaction
	q1     *Transaction
	q2     *Transaction
	q3     *Transaction
	local  ProposalView
	altID  string
}

func newCapProtectScene(t *testing.T) *capProtectScene {
	t.Helper()
	keys := genKeys(t, 5)
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(t.TempDir(), Config{
		Seed: []byte("cap-protect-scene"), Validators: vals,
		MaxTxsPerBlock: 2, PoolCapacity: 6,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &capProtectScene{n: n, keys: keys}
	// 先只放入会进入本地提议的两笔，冻结提议；其余交易在提议之后提交。
	s.lp = NewTransaction(keys[0].priv, 1, []byte("local-only"), 10, 100)
	s.shared = NewTransaction(keys[2].priv, 1, []byte("shared"), 30, 100)
	for _, tx := range []*Transaction{s.lp, s.shared} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 费用高的 shared 在前，lp 在后：本地提议顺序固定。
	if !reflect.DeepEqual(local.TxIDs, []string{s.shared.ID(), s.lp.ID()}) {
		t.Fatalf("local proposal = %v, want [shared lp]", local.TxIDs)
	}
	s.local = local

	// 提议冻结后新交易不再进入本地提议：三笔排队交易与竞争候选独有交易入池。
	s.q1 = NewTransaction(keys[3].priv, 1, []byte("q1"), 50, 100)
	s.q2 = NewTransaction(keys[3].priv, 2, []byte("q2"), 60, 100)
	s.q3 = NewTransaction(keys[3].priv, 3, []byte("q3"), 70, 100)
	s.cp = NewTransaction(keys[1].priv, 1, []byte("candidate-only"), 20, 100)
	for _, tx := range []*Transaction{s.q1, s.q2, s.q3, s.cp} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 竞争候选同时引用 shared（已在本地提议中）与 cp（排队）：
	// 登记后 cp 转为等待投票，shared 被两个候选共同引用仍只占一个位置。
	s.altID = registerMust(t, n, 1, []string{s.shared.ID(), s.cp.ID()})
	if s.altID == s.local.BlockID {
		t.Fatal("competing candidate must have a distinct block id")
	}

	// 竞争候选先取得一票，后续提交不得改动已取得的票。
	vres, err := n.Vote(keys[0].pub, 1, s.altID)
	if err != nil || !vres.Counted {
		t.Fatalf("vote for competing candidate: %+v %v", vres, err)
	}

	s.assertMixedShape(t)
	return s
}

// assertMixedShape 固定场景建成时的混合形态：受保护交易等待投票，
// q1..q3 等待打包，池计数恰为容量 6，两个候选均 pending。
func (s *capProtectScene) assertMixedShape(t *testing.T) {
	t.Helper()
	n := s.n
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size = %d, want 6 (full)", got)
	}
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusProposed {
			t.Fatalf("protected tx %s status = %s, want proposed", tx.ID(), info.Status)
		}
	}
	for _, tx := range []*Transaction{s.q1, s.q2, s.q3} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusQueued {
			t.Fatalf("queued tx %s status = %s, want queued", tx.ID(), info.Status)
		}
	}
	// 账户视角：受保护交易等待投票，排队交易等待打包。
	assertAccountQueue(t, n, s.keys[0].pub, 0, map[string]string{s.lp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{s.cp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{
		s.q1.ID(): "waiting-pack", s.q2.ID(): "waiting-pack", s.q3.ID(): "waiting-pack",
	})
	rc := mustCandidates(t, n, 1)
	if len(rc.Candidates) != 2 || rc.Ended {
		t.Fatalf("round state = %+v, want two pending candidates", rc)
	}
	for _, c := range rc.Candidates {
		if c.Result != CandidatePending {
			t.Fatalf("candidate %s result = %s, want pending", c.BlockID, c.Result)
		}
	}
}

// capEntrySnap 记录一笔交易在提交前后的状态与全部关联字段。
type capEntrySnap struct {
	status     TxStatus
	replacedBy string
	dropReason string
	dropRound  uint64
}

func capEntrySnapshot(n *Node) map[string]capEntrySnap {
	out := make(map[string]capEntrySnap, len(n.st.Entries))
	for id, e := range n.st.Entries {
		out[id] = capEntrySnap{
			status:     TxStatus(e.Status),
			replacedBy: e.ReplacedBy,
			dropReason: e.DropReason,
			dropRound:  e.DropRound,
		}
	}
	return out
}

// capAccountSnap 记录全部相关账户的已确认序号、缺口与待处理（标识+等待说明）序列。
func capAccountSnap(n *Node, keys []testKey) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		acct := n.Account(k.pub)
		parts := make([]string, 0, len(acct.Pending))
		for _, p := range acct.Pending {
			parts = append(parts, p.ID+":"+p.Note)
		}
		out[acct.Sender] = joinSnap(acct.ConfirmedSequence, acct.Gap, parts)
	}
	return out
}

func joinSnap(confirmed, gap uint64, pending []string) string {
	return "confirmed=" + strconv.FormatUint(confirmed, 10) + "|gap=" + strconv.FormatUint(gap, 10) + "|pending=" + strings.Join(pending, ",")
}

// assertFrozenViews 断言本地提议、候选（交易顺序、区块标识、票数、结果）、
// 全部历史条目状态与账户待处理列表与提交前快照完全一致；没有新确认块。
func (s *capProtectScene) assertFrozenViews(t *testing.T,
	beforeEntries map[string]capEntrySnap, beforeAccounts map[string]string,
	beforeLocal ProposalView, beforeRC *RoundCandidates,
) {
	t.Helper()
	n := s.n
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	if rc := mustCandidates(t, n, 1); !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates view changed:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}
	if got := capEntrySnapshot(n); !reflect.DeepEqual(got, beforeEntries) {
		t.Fatalf("tx entries changed by rejected submit:\nbefore=%v\nafter =%v", beforeEntries, got)
	}
	if got := capAccountSnap(n, s.keys); !reflect.DeepEqual(got, beforeAccounts) {
		t.Fatalf("accounts changed by rejected submit:\nbefore=%v\nafter =%v", beforeAccounts, got)
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, rejection must not confirm anything", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("rejection must not leave a confirmed block")
	}
}

func (s *capProtectScene) snapshots(t *testing.T) (map[string]capEntrySnap, map[string]string, ProposalView, *RoundCandidates) {
	t.Helper()
	prop, ok := s.n.Proposal()
	if !ok {
		t.Fatal("local proposal missing")
	}
	return capEntrySnapshot(s.n), capAccountSnap(s.n, s.keys), prop, mustCandidates(t, s.n, 1)
}

// 成功路径：新交易费用高于唯一可淘汰的 q1 时，挤出 q1 且只涉及 q1。
// 受保护交易即使费用最低（lp 费用 10）也保留；被挤出的是可淘汰交易中费用最低者。
func TestCapacityMixedProtectionEvictsOnlyQueued(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	win := NewTransaction(s.keys[4].priv, 1, []byte("winner"), 51, 100) // 高于 q1(50)，低于 q2/q3
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("higher-fee tx must displace the lowest queued tx: %v", err)
	}
	if res.TxID != win.ID() || res.EvictedID != s.q1.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want accept with evicted q1 and no replace link", res)
	}

	// 被挤出的 q1：dropped / pool-capacity / 发生轮次 1，原内容保留，无确认块与替换关联。
	info, err := n.Tx(s.q1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("q1 after eviction = %+v, want dropped/pool-capacity/round 1", info)
	}
	if info.BlockHeight != 0 || info.BlockID != "" || info.ReplacedBy != "" {
		t.Fatalf("dropped q1 must not carry block/replace links: %+v", info)
	}
	assertTxMatches(t, info.Tx, s.q1)

	// q1 从账户待处理列表退出，已确认序号不推进；q2、q3 保留且留下序号 1 的缺口。
	acct3 := n.Account(s.keys[3].pub)
	if acct3.ConfirmedSequence != 0 || acct3.Gap != 1 || len(acct3.Pending) != 2 {
		t.Fatalf("q1 sender account = %+v, want confirmed 0 gap 1 two pending", acct3)
	}
	if acct3.Pending[0].ID != s.q2.ID() || acct3.Pending[1].ID != s.q3.ID() {
		t.Fatalf("remaining pending = [%s %s], want [q2 q3]", acct3.Pending[0].ID, acct3.Pending[1].ID)
	}

	// 新交易排队等待打包，提交结果不带替换关联。
	winInfo, err := n.Tx(win.ID())
	if err != nil || winInfo.Status != StatusQueued {
		t.Fatalf("new tx = %+v %v, want queued", winInfo, err)
	}
	assertAccountQueue(t, n, s.keys[4].pub, 0, map[string]string{win.ID(): "waiting-pack"})

	// 其余交易与受保护交易状态、关联记录一律不变。
	for id, before := range beforeEntries {
		if id == s.q1.ID() {
			continue
		}
		after, ok := n.st.Entries[id]
		if !ok {
			t.Fatalf("tx %s vanished", id)
		}
		got := capEntrySnap{
			status:     TxStatus(after.Status),
			replacedBy: after.ReplacedBy,
			dropReason: after.DropReason,
			dropRound:  after.DropRound,
		}
		if got != before {
			t.Fatalf("tx %s changed: before=%+v after=%+v", id, before, got)
		}
		// 其余排队交易（q2、q3）内容原样保留。
		if after.Status == stQueued {
			if want := lookupSceneTx(s, id); want != nil {
				assertTxMatches(t, mustTx(t, n, id).Tx, want)
			}
		}
	}
	// 受保护交易内容原样保留。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared, s.q2, s.q3} {
		assertTxMatches(t, mustTx(t, n, tx.ID()).Tx, tx)
	}
	// 受保护账户仍只有等待投票的原交易。
	assertAccountQueue(t, n, s.keys[0].pub, 0, map[string]string{s.lp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{s.cp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{
		s.q2.ID(): "waiting-pack", s.q3.ID(): "waiting-pack",
	})
	// 三个受保护账户的待处理序列化与提交前完全相同；新交易账户此前为空。
	// 账户 3 因 q1 被挤出而变化，已在上面单独校验。
	afterAccounts := capAccountSnap(n, s.keys)
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("%x", s.keys[i].pub)
		if afterAccounts[k] != beforeAccounts[k] {
			t.Fatalf("protected account %d changed:\nbefore=%s\nafter =%s", i, beforeAccounts[k], afterAccounts[k])
		}
	}
	if before := beforeAccounts[fmt.Sprintf("%x", s.keys[4].pub)]; before != "confirmed=0|gap=0|pending=" {
		t.Fatalf("new sender should have been empty before submit, got %s", before)
	}

	// 本地提议、竞争候选的交易顺序、区块标识与已取得的票均不受影响。
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	rc := mustCandidates(t, n, 1)
	if !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates changed:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}
	local := candidateByBlockID(rc, s.local.BlockID)
	alt := candidateByBlockID(rc, s.altID)
	if local == nil || alt == nil || !local.Local {
		t.Fatalf("candidate set after eviction wrong: local=%v alt=%v", local, alt)
	}
	if !reflect.DeepEqual(local.TxIDs, []string{s.shared.ID(), s.lp.ID()}) {
		t.Fatalf("local order = %v, want [shared lp]", local.TxIDs)
	}
	if !reflect.DeepEqual(alt.TxIDs, []string{s.shared.ID(), s.cp.ID()}) {
		t.Fatalf("competing order = %v, want [shared cp]", alt.TxIDs)
	}
	if len(alt.Voters) != 1 {
		t.Fatalf("competing candidate votes = %d, want 1 preserved", len(alt.Voters))
	}
	// 计数守恒：挤一出一，池仍恰好满。
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size after eviction = %d, want 6", got)
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, eviction must not confirm", n.Height())
	}
}

// 同费但标识更小时同样挤出 q1，其余一切与高费成功路径一致。
func TestCapacityMixedProtectionSameFeeSmallerID(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n

	smaller := craftTxWithID(t, s.keys[4].priv, 1, 50, s.q1.ID(), true)
	if smaller.ID() >= s.q1.ID() {
		t.Fatal("test setup: crafted id must be smaller than q1")
	}
	res, err := n.Submit(smaller)
	if err != nil {
		t.Fatalf("same-fee smaller-id tx must evict q1: %v", err)
	}
	if res.EvictedID != s.q1.ID() || res.ReplacedID != "" {
		t.Fatalf("result = %+v, want evicted q1 without replacement", res)
	}
	if info, _ := n.Tx(s.q1.ID()); info.Status != StatusDropped || info.DropRound != 1 {
		t.Fatalf("q1 = %+v, want dropped in round 1", info)
	}
	if info, _ := n.Tx(smaller.ID()); info.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", info.Status)
	}
	// 受保护交易即使标识/费用不占优也保持等待投票。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusProposed {
			t.Fatalf("protected tx %s = %s, want proposed", tx.ID(), info.Status)
		}
	}
}

// 拒绝路径一：新交易费用高于每一笔受保护交易（lp 10、cp 20、shared 30），
// 却低于所有可淘汰交易（q1 50 起步）时，必须 pool-full：不能挤出候选中的低费
// 交易，也不能仅因比受保护交易贵就被接收。
func TestCapacityMixedProtectionRejectHigherThanProtected(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	rejected := NewTransaction(s.keys[4].priv, 1, []byte("rejected-40"), 40, 100)
	res, err := n.Submit(rejected)
	if reason(err) != ReasonPoolFull {
		t.Fatalf("submit got %v, want %s", err, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil", res)
	}
	if _, err := n.Tx(rejected.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s", err, ReasonUnknownTx)
	}
	// q1 仍是排队交易，没有任何淘汰记录。
	if info, _ := n.Tx(s.q1.ID()); info.Status != StatusQueued || info.DropReason != "" {
		t.Fatalf("q1 after rejection = %+v, want untouched queued", info)
	}
	s.assertFrozenViews(t, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
}

// 拒绝路径二：与可淘汰对象 q1 同费、但标识更大时，同样 pool-full；
// 受保护交易与候选视图保持提交前结果。
func TestCapacityMixedProtectionRejectSameFeeLargerID(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	larger := craftTxWithID(t, s.keys[4].priv, 1, 50, s.q1.ID(), false)
	if larger.ID() <= s.q1.ID() {
		t.Fatal("test setup: crafted id must be larger than q1")
	}
	res, err := n.Submit(larger)
	if reason(err) != ReasonPoolFull {
		t.Fatalf("submit got %v, want %s", err, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil", res)
	}
	if _, err := n.Tx(larger.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s", err, ReasonUnknownTx)
	}
	s.assertFrozenViews(t, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
}

func mustTx(t *testing.T, n *Node, id string) *TxInfo {
	t.Helper()
	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func lookupSceneTx(s *capProtectScene, id string) *Transaction {
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared, s.q1, s.q2, s.q3} {
		if tx.ID() == id {
			return tx
		}
	}
	return nil
}
