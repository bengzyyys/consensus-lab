package consensus

import (
	"reflect"
	"testing"
)

// 回归保障：账户序号已经确认后，满池也必须按交易本身的原因拒绝旧序号提交。
// 接收规则的顺序是：先识别历史中的同一笔交易（duplicate-transaction），
// 再判断账户已确认序号（sequence-already-confirmed），最后才考虑容量与淘汰；
// 即使池已满且新交易费用足以挤出排队交易，前两类拒绝也不能被容量判断
// 覆盖成 pool-full，更不能先入池再淘汰。本文件沿用既有公开入口
// （Submit/Tx/Account/Propose/RegisterCandidate/Vote/Candidates/BlockAt）、
// 既有返回类型与既有接收规则，只锁定这些既有结果。
//
// confirmedFullPoolScene 是“已确认序号账户 + 满池 + 未决候选”的固定场景：
//
//	keys[0..3]：验证者（4 名，确认需至少 3 票）
//	keys[4]：账户 A，第 1 轮经正常投票确认序号 1（a1，费用 51）与序号 2（a2，费用 52）
//	keys[5] p1 费用 30、keys[6] p2 费用 40：第 2 轮被本地提议引用，等待投票
//	keys[7] q1 费用 50、q2 费用 60：第 2 轮排队，q1 是唯一可被淘汰的最低费交易
//
// 容量为 4，第 2 轮建成后池恰好满（p1、p2 受未决候选保护，q1、q2 可淘汰）。
// 竞争候选（仅含 p1）已登记并取得一票，本地提议与票数在场景内冻结。
// 账户 A 的旧序号提交费用均高于 q1、到期轮次 100 晚于当前轮次 2、签名有效：
// 容量规则单独判断时确实有可淘汰对象 q1。
type confirmedFullPoolScene struct {
	n     *Node
	keys  []testKey
	a1    *Transaction // 已确认序号 1
	a2    *Transaction // 已确认序号 2
	p1    *Transaction
	p2    *Transaction
	q1    *Transaction
	q2    *Transaction
	block Block // 第 1 轮确认块（含 a1、a2）
	local ProposalView
	altID string
}

func newConfirmedFullPoolScene(t *testing.T) *confirmedFullPoolScene {
	t.Helper()
	keys := genKeys(t, 8)
	vals := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		vals[i] = keys[i].pub
	}
	n, err := New(t.TempDir(), Config{
		Seed: []byte("confirmed-full-pool-scene"), Validators: vals,
		MaxTxsPerBlock: 2, PoolCapacity: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &confirmedFullPoolScene{n: n, keys: keys}
	// 第 1 轮：账户 A 的序号 1、2 入池并打包，经正常投票确认。
	s.a1 = NewTransaction(keys[4].priv, 1, []byte("a1"), 51, 100)
	s.a2 = NewTransaction(keys[4].priv, 2, []byte("a2"), 52, 100)
	for _, tx := range []*Transaction{s.a1, s.a2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(local1.TxIDs, []string{s.a1.ID(), s.a2.ID()}) {
		t.Fatalf("round 1 proposal = %v, want [a1 a2]", local1.TxIDs)
	}
	confirmCandidate(t, n, keys[:4], local1.BlockID)
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("after confirm: round %d height %d, want 2/1", n.CurrentRound(), n.Height())
	}
	block, ok := n.LatestBlock()
	if !ok || block.ID != local1.BlockID {
		t.Fatalf("confirmed block = %+v ok=%v, want id %s", block, ok, local1.BlockID)
	}
	s.block = block
	if acct := n.Account(keys[4].pub); acct.ConfirmedSequence != 2 || len(acct.Pending) != 0 {
		t.Fatalf("account A after confirm = %+v, want confirmed 2 and no pending", acct)
	}

	// 第 2 轮：两笔交易进入本地提议并冻结，两笔排队交易随后占满容量。
	s.p1 = NewTransaction(keys[5].priv, 1, []byte("p1"), 30, 100)
	s.p2 = NewTransaction(keys[6].priv, 1, []byte("p2"), 40, 100)
	for _, tx := range []*Transaction{s.p1, s.p2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 费用高的 p2 在前，p1 在后：本地提议顺序固定。
	if !reflect.DeepEqual(local.TxIDs, []string{s.p2.ID(), s.p1.ID()}) {
		t.Fatalf("round 2 proposal = %v, want [p2 p1]", local.TxIDs)
	}
	s.local = local

	s.q1 = NewTransaction(keys[7].priv, 1, []byte("q1"), 50, 100)
	s.q2 = NewTransaction(keys[7].priv, 2, []byte("q2"), 60, 100)
	for _, tx := range []*Transaction{s.q1, s.q2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.st.poolSize(); got != 4 {
		t.Fatalf("pool size = %d, want 4 (full)", got)
	}

	// 竞争候选仅引用已在本地提议中的 p1：排队交易保持可淘汰。
	s.altID = registerMust(t, n, 2, []string{s.p1.ID()})
	if s.altID == s.local.BlockID {
		t.Fatal("competing candidate must have a distinct block id")
	}
	// 竞争候选先取得一票（未达 3 票门槛），后续提交不得改动已取得的票。
	vres, err := n.Vote(keys[0].pub, 2, s.altID)
	if err != nil || !vres.Counted || vres.Confirmed {
		t.Fatalf("vote for competing candidate: %+v %v", vres, err)
	}

	s.assertMixedShape(t)
	return s
}

// assertMixedShape 固定场景建成时的形态：受保护交易等待投票，q1、q2 等待
// 打包，池计数恰为容量 4，两个候选均 pending，竞争候选已有一票。
func (s *confirmedFullPoolScene) assertMixedShape(t *testing.T) {
	t.Helper()
	n := s.n
	for _, tx := range []*Transaction{s.p1, s.p2} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusProposed {
			t.Fatalf("protected tx %s status = %s, want proposed", tx.ID(), info.Status)
		}
	}
	for _, tx := range []*Transaction{s.q1, s.q2} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusQueued {
			t.Fatalf("queued tx %s status = %s, want queued", tx.ID(), info.Status)
		}
	}
	assertAccountQueue(t, n, s.keys[4].pub, 2, map[string]string{})
	assertAccountQueue(t, n, s.keys[5].pub, 0, map[string]string{s.p1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[6].pub, 0, map[string]string{s.p2.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[7].pub, 0, map[string]string{
		s.q1.ID(): "waiting-pack", s.q2.ID(): "waiting-pack",
	})
	rc := mustCandidates(t, n, 2)
	if len(rc.Candidates) != 2 || rc.Ended {
		t.Fatalf("round state = %+v, want two pending candidates", rc)
	}
	alt := candidateByBlockID(rc, s.altID)
	if alt == nil || len(alt.Voters) != 1 || alt.Result != CandidatePending {
		t.Fatalf("competing candidate = %+v, want pending with 1 vote", alt)
	}
}

// snapshots 记录拒绝前的条目、账户、本地提议与候选视图。
func (s *confirmedFullPoolScene) snapshots(t *testing.T) (map[string]capEntrySnap, map[string]string, ProposalView, *RoundCandidates) {
	t.Helper()
	prop, ok := s.n.Proposal()
	if !ok {
		t.Fatal("local proposal missing")
	}
	return capEntrySnapshot(s.n), capAccountSnap(s.n, s.keys), prop, mustCandidates(t, s.n, 2)
}

// assertFrozen 在旧序号提交被拒绝后断言场景完全冻结：本地提议、候选（交易
// 顺序、区块标识、票数、结果）、全部条目与账户视图和拒绝前一致；轮次仍是 2，
// 确认高度仍是 1，原确认块的交易顺序不变；a1、a2 仍指向原确认记录（confirmed、
// 原确认块关联、无替换关联）；q1、q2 没有被挤出，没有任何 pool-capacity 淘汰
// 记录；账户 A 已确认序号仍为 2，待处理列表不出现旧序号提交。
func (s *confirmedFullPoolScene) assertFrozen(t *testing.T,
	beforeEntries map[string]capEntrySnap, beforeAccounts map[string]string,
	beforeLocal ProposalView, beforeRC *RoundCandidates,
) {
	t.Helper()
	n := s.n
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	if rc := mustCandidates(t, n, 2); !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates view changed:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}
	if got := capEntrySnapshot(n); !reflect.DeepEqual(got, beforeEntries) {
		t.Fatalf("tx entries changed by rejected submit:\nbefore=%v\nafter =%v", beforeEntries, got)
	}
	if got := capAccountSnap(n, s.keys); !reflect.DeepEqual(got, beforeAccounts) {
		t.Fatalf("accounts changed by rejected submit:\nbefore=%v\nafter =%v", beforeAccounts, got)
	}
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, rejected submit must not advance the round", n.CurrentRound())
	}
	if n.Height() != 1 {
		t.Fatalf("height = %d, rejection must not change the confirmed height", n.Height())
	}
	// 原确认块高度、标识与交易顺序不变。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blk, s.block) {
		t.Fatalf("confirmed block changed:\nbefore=%+v\nafter =%+v", s.block, blk)
	}
	if !reflect.DeepEqual(blk.TxIDs, []string{s.a1.ID(), s.a2.ID()}) {
		t.Fatalf("confirmed block tx order = %v, want [a1 a2]", blk.TxIDs)
	}
	// a1、a2 仍指向原来的确认记录：confirmed 状态、原确认块关联、完整内容，
	// 不能被解释成同序号替换（无替换关联）。
	for _, tx := range []*Transaction{s.a1, s.a2} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != s.block.ID {
			t.Fatalf("confirmed tx %s = %+v, want confirmed at height 1 in block %s", tx.ID(), info, s.block.ID)
		}
		if info.ReplacedBy != "" || info.DropReason != "" || info.DropRound != 0 {
			t.Fatalf("confirmed tx %s must not carry replace/drop links: %+v", tx.ID(), info)
		}
		assertTxMatches(t, info.Tx, tx)
		if ref, ok := n.st.Confirmed[tx.ID()]; !ok || ref.Height != 1 || ref.Block != s.block.ID {
			t.Fatalf("confirmed ref for %s = %+v ok=%v, want height 1 block %s", tx.ID(), ref, ok, s.block.ID)
		}
	}
	// 排队交易没有被挤出，也没有任何新的 pool-capacity 淘汰记录。
	for _, tx := range []*Transaction{s.q1, s.q2} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued || info.DropReason != "" || info.DropRound != 0 {
			t.Fatalf("queued tx %s after rejection = %+v, want untouched queued", tx.ID(), info)
		}
		assertTxMatches(t, info.Tx, tx)
	}
	for id, e := range n.st.Entries {
		if e.DropReason == DropReasonPoolCapacity || e.Status == stDropped {
			t.Fatalf("tx %s has eviction record %+v, rejection must not evict", id, e)
		}
	}
	// 账户 A 已确认序号仍为 2，待处理列表为空（旧序号提交没有入池占位）。
	assertAccountQueue(t, n, s.keys[4].pub, 2, map[string]string{})
	if seqs := n.st.Pool[s.a1.SenderHex()]; len(seqs) != 0 {
		t.Fatalf("account A pool slots = %v, want none (old sequences must not occupy the pool)", seqs)
	}
	if got := n.st.poolSize(); got != 4 {
		t.Fatalf("pool size = %d, want 4 (rejection must not free a slot)", got)
	}
}

// assertFreshSeq3EvictsQ1 在拒绝之后提交账户 A 的下一序号（序号 3）全新高费
// 交易：应正常入池并挤出 q1。它证明场景里容量规则单独判断时确有可淘汰对象，
// 此前的拒绝完全来自交易自身的重复/旧序号原因，而不是容量或费用不足。
func (s *confirmedFullPoolScene) assertFreshSeq3EvictsQ1(t *testing.T, beforeLocal ProposalView, beforeRC *RoundCandidates) {
	t.Helper()
	n := s.n
	fresh := NewTransaction(s.keys[4].priv, 3, []byte("a3-fresh"), 55, 100)
	res, err := n.Submit(fresh)
	if err != nil {
		t.Fatalf("valid next-sequence tx must be admitted after rejections: %v", err)
	}
	if res.TxID != fresh.ID() || res.EvictedID != s.q1.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want accept with evicted q1 and no replace link", res)
	}
	if info, _ := n.Tx(fresh.ID()); info.Status != StatusQueued {
		t.Fatalf("fresh tx status = %s, want queued", info.Status)
	}
	if info, _ := n.Tx(s.q1.ID()); info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 2 {
		t.Fatalf("q1 after valid submit = %+v, want dropped/pool-capacity/round 2", info)
	}
	assertAccountQueue(t, n, s.keys[4].pub, 2, map[string]string{fresh.ID(): "waiting-pack"})
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	if rc := mustCandidates(t, n, 2); !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates changed by eviction:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}
	if got := n.st.poolSize(); got != 4 {
		t.Fatalf("pool size after eviction = %d, want 4", got)
	}
}

// 逐字节重交已经确认的序号 2 交易：签名有效、到期轮次晚于当前轮次、费用 52
// 高于可淘汰的 q1（50），仍必须返回 duplicate-transaction，而不是 pool-full，
// 更不能入池。重交标识仍指向原来的确认记录，不能解释成同序号替换。
func TestFullPoolReplayOfConfirmedTxReturnsDuplicate(t *testing.T) {
	s := newConfirmedFullPoolScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	// 按最初被接受时的发送者、序号、内容、费用、到期轮次与签名原样重建。
	// Ed25519 签名确定，逐字节相同的输入必须得到同一交易标识。
	replay := NewTransaction(s.keys[4].priv, 2, []byte("a2"), 52, 100)
	if replay.ID() != s.a2.ID() {
		t.Fatalf("test setup: byte-identical replay id %s != original %s", replay.ID(), s.a2.ID())
	}
	if !replay.Verify() {
		t.Fatal("test setup: replay signature must be valid")
	}
	res, err := n.Submit(replay)
	if reason(err) != ReasonDuplicate {
		t.Fatalf("replay of confirmed tx got %v, want %s (must not be %s or admitted)",
			err, ReasonDuplicate, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil", res)
	}

	// 重交的已有标识仍能查到原交易完整内容、confirmed 状态及原确认块关联。
	info, err := n.Tx(replay.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != s.block.ID || info.ReplacedBy != "" {
		t.Fatalf("replayed confirmed tx = %+v, want confirmed at height 1 in original block, no replace link", info)
	}
	assertTxMatches(t, info.Tx, s.a2)

	s.assertFrozen(t, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
	s.assertFreshSeq3EvictsQ1(t, beforeLocal, beforeRC)
}

// 同一发送者提交内容不同、重新签署且标识从未被接受的旧序号交易：序号为 1
// （小于已确认序号）或恰好等于已确认序号 2，都必须返回
// sequence-already-confirmed，而不是 pool-full，也不能因费用足够高而入池。
// 每次拒绝后新标识查询仍为 unknown-transaction（不留历史）。
func TestFullPoolRejectsOldSequenceFreshIDs(t *testing.T) {
	s := newConfirmedFullPoolScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	// 序号 1：小于已确认序号 2；费用 55 高于 q1，到期轮次 100 晚于当前轮次 2。
	oldSeq1 := NewTransaction(s.keys[4].priv, 1, []byte("old-seq-1"), 55, 100)
	if !oldSeq1.Verify() {
		t.Fatal("test setup: signature must be valid")
	}
	if _, err := n.Tx(oldSeq1.ID()); reason(err) != ReasonUnknownTx {
		t.Fatal("test setup: fresh id must be unknown before submit")
	}
	assertRejectedUnknown(t, n, oldSeq1, ReasonOldSequence)

	// 序号恰好等于已确认序号 2：同样拒绝（序号必须严格大于已确认序号）。
	oldSeq2 := NewTransaction(s.keys[4].priv, 2, []byte("old-seq-2"), 56, 100)
	if !oldSeq2.Verify() {
		t.Fatal("test setup: signature must be valid")
	}
	if oldSeq2.ID() == s.a2.ID() {
		t.Fatal("test setup: distinct content must yield a distinct id")
	}
	assertRejectedUnknown(t, n, oldSeq2, ReasonOldSequence)

	s.assertFrozen(t, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
	s.assertFreshSeq3EvictsQ1(t, beforeLocal, beforeRC)
}
