package consensus

import (
	"reflect"
	"testing"
)

// 回归保障：账户序号已经通过正常投票确认后，即使交易池已满且存在费用更低、
// 可以被挤出的排队交易，旧序号提交也必须按“交易本身”的既有规则拒绝，不能被
// 容量判断改成 pool-full，更不能凭高费用入池并挤出排队交易。
//
// Submit 的既有接收规则顺序固定为：先按交易标识识别历史中的同一笔交易
// （duplicate-transaction），再判断到期与账户已确认序号
// （sequence-already-confirmed），之后才是同序号替换与容量/淘汰判断。
// 本文件锁定这些判断与满池淘汰“同时出现”时的先后关系，只使用既有公开入口
// （Submit/Tx/Account/Propose/RegisterCandidate/Vote/Candidates/BlockAt/Open）、
// 既有返回类型与既有拒绝原因，不引入任何新行为。
//
// 固定场景（5 个账户、前 4 个为验证者，单块上限 2、池容量 6）：
//
//	keys[0] Alice：序号 1、2 在第 1 轮经本地提议、三票正常确认，已确认序号为 2
//	keys[1] lp     费用 10：第 2 轮仅被本地提议引用（本地提议独有，等待投票）
//	keys[2] cp     费用 20：第 2 轮仅被竞争候选引用（竞争候选独有，等待投票）
//	keys[3] shared 费用 30：第 2 轮同时被本地提议与竞争候选引用（多候选共享）
//	keys[4] q1..q3 费用 50/60/70：第 2 轮始终排队，q1 是唯一可被淘汰的最低费交易
//	keys[5]：对照提交使用的“其他发送者”
//
// 第 2 轮本地提议与竞争候选均已登记、各持一票但均未确认；池计数恰为 6。
type seqFullScene struct {
	n       *Node
	dir     string
	keys    []testKey
	a1      *Transaction // Alice 已确认序号 1
	a2      *Transaction // Alice 已确认序号 2
	lp      *Transaction
	cp      *Transaction
	shared  *Transaction
	q1      *Transaction
	q2      *Transaction
	q3      *Transaction
	blockID string // 第 1 轮确认块标识
}

func newSeqFullScene(t *testing.T) *seqFullScene {
	t.Helper()
	keys := genKeys(t, 6)
	vals := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		vals[i] = keys[i].pub
	}
	dir := t.TempDir()
	n, err := New(dir, Config{
		Seed: []byte("seq-confirmed-full-pool-scene"), Validators: vals,
		MaxTxsPerBlock: 2, PoolCapacity: 6,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &seqFullScene{n: n, dir: dir, keys: keys}

	// 第 1 轮：Alice 的序号 1、2 正常打包、投票并确认（单块上限 2，恰好同块）。
	s.a1 = NewTransaction(keys[0].priv, 1, []byte("alice-seq1"), 200, 100)
	s.a2 = NewTransaction(keys[0].priv, 2, []byte("alice-seq2"), 190, 100)
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
	confirmCandidate(t, n, s.keys[:4], local1.BlockID)
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round/height = %d/%d, want 2/1", n.CurrentRound(), n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blk.TxIDs, []string{s.a1.ID(), s.a2.ID()}) || blk.Round != 1 {
		t.Fatalf("confirmed block wrong: %+v", blk)
	}
	s.blockID = blk.ID

	// 第 2 轮：先放入进入本地提议的两笔，冻结提议。
	s.lp = NewTransaction(keys[1].priv, 1, []byte("local-only"), 10, 100)
	s.shared = NewTransaction(keys[3].priv, 1, []byte("shared"), 30, 100)
	for _, tx := range []*Transaction{s.lp, s.shared} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(local2.TxIDs, []string{s.shared.ID(), s.lp.ID()}) {
		t.Fatalf("round 2 local proposal = %v, want [shared lp]", local2.TxIDs)
	}

	// 提议冻结后：竞争候选独有交易与三笔排队交易入池，池恰好占满 6 个位置。
	s.cp = NewTransaction(keys[2].priv, 1, []byte("candidate-only"), 20, 100)
	s.q1 = NewTransaction(keys[4].priv, 1, []byte("q1"), 50, 100)
	s.q2 = NewTransaction(keys[4].priv, 2, []byte("q2"), 60, 100)
	s.q3 = NewTransaction(keys[4].priv, 3, []byte("q3"), 70, 100)
	for _, tx := range []*Transaction{s.cp, s.q1, s.q2, s.q3} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 竞争候选引用 shared（已在本地提议）与 cp（排队）：登记后 cp 转为等待投票，
	// shared 被两个候选共同引用仍只占一个位置。
	altID := registerMust(t, n, 2, []string{s.shared.ID(), s.cp.ID()})
	if altID == local2.BlockID {
		t.Fatal("competing candidate must have a distinct block id")
	}

	// 两个候选各持一票，均未确认（四人名单需至少 3 票）。
	if res, err := n.Vote(keys[1].pub, 2, local2.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("local proposal vote: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[2].pub, 2, altID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("competing candidate vote: %+v %v", res, err)
	}

	s.assertShape(t)
	return s
}

// assertShape 固定拒绝操作前的形态：Alice 已确认序号 2、无待处理；三笔受保护
// 交易等待投票，q1..q3 等待打包，池恰好满；两个候选均 pending 且各持一票。
func (s *seqFullScene) assertShape(t *testing.T) {
	t.Helper()
	n := s.n
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size = %d, want 6 (full)", got)
	}
	assertAccountQueue(t, n, s.keys[0].pub, 2, map[string]string{})
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
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{s.lp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.cp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[4].pub, 0, map[string]string{
		s.q1.ID(): "waiting-pack", s.q2.ID(): "waiting-pack", s.q3.ID(): "waiting-pack",
	})

	// 容量规则单独判断时确实存在可淘汰对象：q1 费用 50 是唯一最低费排队交易。
	victim, ok := n.st.evictionVictim()
	if !ok || victim != s.q1.ID() {
		t.Fatalf("eviction victim = %q (%v), want q1 %s", victim, ok, s.q1.ID())
	}

	rc := mustCandidates(t, n, 2)
	if len(rc.Candidates) != 2 || rc.Ended {
		t.Fatalf("round 2 state = %+v, want two pending candidates", rc)
	}
	for _, c := range rc.Candidates {
		if c.Result != CandidatePending || len(c.Voters) != 1 {
			t.Fatalf("candidate %s = result %s voters %d, want pending with 1 vote",
				c.BlockID, c.Result, len(c.Voters))
		}
	}
}

// 核心回归：满池、且容量规则单独判断确有可淘汰对象（q1，费用 50）时，
// Alice（已确认序号 2）的三类旧序号提交仍必须按交易本身的原因拒绝：
//   - 逐字节重交已确认的序号 2 交易：duplicate-transaction（历史同标识优先）；
//   - 同发送者、内容不同、重新签署、标识从未被接受的序号 1 交易：
//     sequence-already-confirmed；
//   - 同上但序号恰好等于已确认序号 2：sequence-already-confirmed。
//
// 三笔提交签名均有效、到期轮次晚于当前轮次、费用（190/191/192）远高于
// 可挤出的 q1（50）——若错误地走到容量判断，它们都会入池并挤出 q1。
func TestFullPoolOldSequenceReplayRejectedByTxRules(t *testing.T) {
	s := newSeqFullScene(t)
	n := s.n

	// 快照拒绝操作前的全部状态：条目、账户、两个候选视图与确认块。
	beforeEntries := capEntrySnapshot(n)
	beforeAccounts := capAccountSnap(n, s.keys)
	beforeRC := mustCandidates(t, n, 2)
	beforeLocal, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal missing")
	}
	blkBefore, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	// 1) 逐字节重交已确认的序号 2：同发送者、同序号、同内容、同费用、同到期、
	// 同签名，标识因此与原确认记录完全一致。
	replay2 := NewTransaction(s.keys[0].priv, 2, []byte("alice-seq2"), 190, 100)
	if replay2.ID() != s.a2.ID() {
		t.Fatalf("test setup: byte-identical replay id %s != confirmed id %s", replay2.ID(), s.a2.ID())
	}
	if res, err := n.Submit(replay2); err == nil {
		t.Fatalf("replay of confirmed tx must be rejected, got result %+v", res)
	} else if got := reason(err); got != ReasonDuplicate {
		t.Fatalf("confirmed tx replay reason = %q, want %s (must not be %s or %s)",
			got, ReasonDuplicate, ReasonOldSequence, ReasonPoolFull)
	} else if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 2) 序号 1（小于已确认序号 2）、内容不同、重新签署、标识从未被接受。
	freshSeq1 := NewTransaction(s.keys[0].priv, 1, []byte("stale-seq1-resigned"), 191, 100)
	if freshSeq1.ID() == s.a1.ID() || freshSeq1.ID() == s.a2.ID() {
		t.Fatal("test setup: fresh seq1 tx must have a never-accepted id")
	}
	if !freshSeq1.Verify() || n.CurrentRound() >= freshSeq1.Expiry {
		t.Fatal("test setup: fresh seq1 tx must be valid and unexpired")
	}
	if res, err := n.Submit(freshSeq1); err == nil {
		t.Fatalf("old sequence tx must be rejected, got result %+v", res)
	} else if got := reason(err); got != ReasonOldSequence {
		t.Fatalf("fresh seq1 reason = %q, want %s (must not be %s despite fee %d > evictable q1 fee 50)",
			got, ReasonOldSequence, ReasonPoolFull, freshSeq1.Fee)
	} else if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 3) 序号恰好等于已确认序号 2，同样全新标识：仍是 sequence-already-confirmed，
	// 不能解释成“同序号替换”（替换只针对池中待处理同序号交易）。
	freshSeq2 := NewTransaction(s.keys[0].priv, 2, []byte("stale-seq2-resigned"), 192, 100)
	if freshSeq2.ID() == s.a2.ID() {
		t.Fatal("test setup: fresh seq2 tx must have a never-accepted id")
	}
	if !freshSeq2.Verify() || n.CurrentRound() >= freshSeq2.Expiry {
		t.Fatal("test setup: fresh seq2 tx must be valid and unexpired")
	}
	if res, err := n.Submit(freshSeq2); err == nil {
		t.Fatalf("equal-to-confirmed sequence tx must be rejected, got result %+v", res)
	} else if got := reason(err); got != ReasonOldSequence {
		t.Fatalf("fresh seq2 reason = %q, want %s (must not be %s, fee-not-higher or a replacement)",
			got, ReasonOldSequence, ReasonPoolFull)
	} else if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	s.assertNoSideEffects(t, beforeEntries, beforeAccounts, beforeRC, beforeLocal, blkBefore, freshSeq1, freshSeq2)
}

// assertNoSideEffects 在每次旧序号拒绝后断言整个节点与操作前完全一致：
// 新标识无历史，原确认记录与确认块关联不变，排队交易未被挤出、无淘汰记录，
// 等待投票交易/本地提议/竞争候选/已有投票冻结，轮次、确认高度与确认块交易顺序
// 不变，账户已确认序号仍为 2，待处理列表不出现旧序号提交。
func (s *seqFullScene) assertNoSideEffects(t *testing.T,
	beforeEntries map[string]capEntrySnap, beforeAccounts map[string]string,
	beforeRC *RoundCandidates, beforeLocal ProposalView, blkBefore Block,
	freshTxs ...*Transaction,
) {
	t.Helper()
	n := s.n

	// 被拒绝的全新标识一律查不到历史，条目总数不增不减。
	for _, tx := range freshTxs {
		if _, err := n.Tx(tx.ID()); reason(err) != ReasonUnknownTx {
			t.Fatalf("rejected fresh tx %s lookup = %v, want %s", tx.ID(), err, ReasonUnknownTx)
		}
	}
	if got := capEntrySnapshot(n); !reflect.DeepEqual(got, beforeEntries) {
		t.Fatalf("tx entries changed by rejected submits:\nbefore=%v\nafter =%v", beforeEntries, got)
	}
	// 明确不允许任何 pool-capacity 淘汰记录（q1..q3 状态也已由上面的快照锁定）。
	for id, e := range n.st.Entries {
		if e.Status == stDropped || e.DropReason != "" || e.DropRound != 0 {
			t.Fatalf("unexpected eviction record for %s: status=%s drop=%s/%d", id, e.Status, e.DropReason, e.DropRound)
		}
	}

	// 逐字节重交的序号 2 仍指向原来的确认记录：confirmed、完整内容、原确认块关联。
	info, err := n.Tx(s.a2.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != s.blockID || info.ReplacedBy != "" {
		t.Fatalf("confirmed a2 record wrong after replays: %+v", info)
	}
	assertTxMatches(t, info.Tx, s.a2)
	info1, err := n.Tx(s.a1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info1.Status != StatusConfirmed || info1.BlockHeight != 1 || info1.BlockID != s.blockID {
		t.Fatalf("confirmed a1 record wrong after replays: %+v", info1)
	}

	// 原确认块：高度、轮次、标识与交易顺序（先 a1 后 a2）均不变化。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(blk, blkBefore) {
		t.Fatalf("confirmed block changed:\nbefore=%+v\nafter =%+v", blkBefore, blk)
	}
	if !reflect.DeepEqual(blk.TxIDs, []string{s.a1.ID(), s.a2.ID()}) {
		t.Fatalf("confirmed block order = %v, want [a1 a2]", blk.TxIDs)
	}
	if latest, ok := n.LatestBlock(); !ok || !reflect.DeepEqual(latest, blkBefore) {
		t.Fatalf("latest block changed: %+v ok=%v", latest, ok)
	}

	// 轮次与确认高度不推进，池仍恰好满（没有交易被挤出腾位）。
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round/height = %d/%d, want 2/1", n.CurrentRound(), n.Height())
	}
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size = %d, want 6 (rejections must not evict)", got)
	}

	// 排队三笔内容与状态原样保留，无淘汰关联。
	for _, tx := range []*Transaction{s.q1, s.q2, s.q3} {
		qi := mustTx(t, n, tx.ID())
		if qi.Status != StatusQueued || qi.DropReason != "" || qi.DropRound != 0 {
			t.Fatalf("queued tx %s = %+v, want untouched queued without drop record", tx.ID(), qi)
		}
		assertTxMatches(t, qi.Tx, tx)
	}
	// 等待投票的三笔继续等待投票。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		if qi := mustTx(t, n, tx.ID()); qi.Status != StatusProposed {
			t.Fatalf("protected tx %s status = %s, want proposed", tx.ID(), qi.Status)
		}
	}

	// 账户视角：Alice 已确认序号仍为 2，待处理列表为空——旧序号提交不出现，
	// 也没有同序号替换关联；其余四个账户的待处理与等待说明冻结。
	if got := capAccountSnap(n, s.keys); !reflect.DeepEqual(got, beforeAccounts) {
		t.Fatalf("accounts changed by rejected submits:\nbefore=%v\nafter =%v", beforeAccounts, got)
	}
	alice := n.Account(s.keys[0].pub)
	if alice.ConfirmedSequence != 2 || alice.Gap != 0 || len(alice.Pending) != 0 {
		t.Fatalf("alice account = %+v, want confirmed seq 2 with no pending old-sequence submits", alice)
	}

	// 本地提议、竞争候选（交易顺序、区块标识、投票者、pending 结果）冻结。
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	if rc := mustCandidates(t, n, 2); !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates view changed:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}
}

// 同一场景落盘重开后，旧序号提交仍按同一顺序拒绝，确认记录、确认块关联与
// 第 2 轮未决候选/投票随状态目录完整恢复；拒绝同样不留下任何副作用。
func TestFullPoolOldSequenceReplayRejectedAfterReopen(t *testing.T) {
	s := newSeqFullScene(t)

	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CurrentRound() != 2 || reopened.Height() != 1 {
		t.Fatalf("reopened round/height = %d/%d, want 2/1", reopened.CurrentRound(), reopened.Height())
	}
	// 恢复后原确认记录仍关联原确认块。
	info, err := reopened.Tx(s.a2.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != s.blockID {
		t.Fatalf("reopened a2 = %+v, want confirmed at height 1 / block %s", info, s.blockID)
	}
	// 恢复后第 2 轮两个候选、票数与满池形态不变。
	s.n = reopened
	s.assertShape(t)

	entriesBefore := len(reopened.st.Entries)

	replay2 := NewTransaction(s.keys[0].priv, 2, []byte("alice-seq2"), 190, 100)
	if res, err := reopened.Submit(replay2); reason(err) != ReasonDuplicate || res != nil {
		t.Fatalf("reopened replay = %+v %v, want %s with nil result", res, err, ReasonDuplicate)
	}
	freshSeq1 := NewTransaction(s.keys[0].priv, 1, []byte("stale-seq1-reopened"), 191, 100)
	if res, err := reopened.Submit(freshSeq1); reason(err) != ReasonOldSequence || res != nil {
		t.Fatalf("reopened fresh seq1 = %+v %v, want %s with nil result", res, err, ReasonOldSequence)
	}
	freshSeq2 := NewTransaction(s.keys[0].priv, 2, []byte("stale-seq2-reopened"), 192, 100)
	if res, err := reopened.Submit(freshSeq2); reason(err) != ReasonOldSequence || res != nil {
		t.Fatalf("reopened fresh seq2 = %+v %v, want %s with nil result", res, err, ReasonOldSequence)
	}

	if len(reopened.st.Entries) != entriesBefore {
		t.Fatalf("entries changed %d -> %d after rejections on reopened node",
			entriesBefore, len(reopened.st.Entries))
	}
	if reopened.CurrentRound() != 2 || reopened.Height() != 1 {
		t.Fatalf("reopened node round/height = %d/%d, want 2/1", reopened.CurrentRound(), reopened.Height())
	}
	if info, _ := reopened.Tx(s.q1.ID()); info.Status != StatusQueued || info.DropReason != "" {
		t.Fatalf("q1 on reopened node = %+v, want untouched queued", info)
	}
	for _, tx := range []*Transaction{freshSeq1, freshSeq2} {
		if _, err := reopened.Tx(tx.ID()); reason(err) != ReasonUnknownTx {
			t.Fatalf("rejected fresh tx %s lookup = %v, want %s", tx.ID(), err, ReasonUnknownTx)
		}
	}
	if alice := reopened.Account(s.keys[0].pub); alice.ConfirmedSequence != 2 || len(alice.Pending) != 0 {
		t.Fatalf("reopened alice account = %+v, want confirmed 2 / no pending", alice)
	}
}

// 对照：同一满池场景下，来自未确认账户的合法全新交易仍按既有容量规则入池，
// 并挤出唯一可淘汰的排队交易 q1；受未决候选引用的交易继续保留。这证明场景中
// “容量规则单独判断时确实有可淘汰对象”，旧序号提交被拒纯粹是接收顺序在容量
// 判断之前生效，而不是池状态本身无法淘汰。
func TestFullPoolOldSequenceSceneStillEvictsForValidTx(t *testing.T) {
	s := newSeqFullScene(t)
	n := s.n
	beforeEntries := capEntrySnapshot(n)
	beforeRC := mustCandidates(t, n, 2)
	beforeLocal, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal missing")
	}

	win := NewTransaction(s.keys[5].priv, 1, []byte("valid-winner"), 51, 100) // 严格高于 q1(50)
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("valid higher-fee tx must be admitted by capacity rule: %v", err)
	}
	if res.TxID != win.ID() || res.EvictedID != s.q1.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want accept with evicted q1 and no replace link", res)
	}

	// q1 被挤出：dropped / pool-capacity / 发生轮次 2，原内容保留，无确认块或替换关联。
	q1Info, err := n.Tx(s.q1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if q1Info.Status != StatusDropped || q1Info.DropReason != DropReasonPoolCapacity || q1Info.DropRound != 2 {
		t.Fatalf("q1 = %+v, want dropped/pool-capacity/round 2", q1Info)
	}
	assertTxMatches(t, q1Info.Tx, s.q1)
	if winInfo, _ := n.Tx(win.ID()); winInfo.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", winInfo.Status)
	}

	// 受未决候选引用的三笔交易一律保留等待投票，候选视图与票数不受淘汰影响。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusProposed {
			t.Fatalf("protected tx %s = %s, want proposed", tx.ID(), info.Status)
		}
	}
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	if rc := mustCandidates(t, n, 2); !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates view changed:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}

	// q1 之外的全部历史条目（含 Alice 的两笔 confirmed 记录）保持操作前状态。
	for id, before := range beforeEntries {
		if id == s.q1.ID() {
			continue
		}
		after, ok := n.st.Entries[id]
		if !ok {
			t.Fatalf("tx %s vanished after control eviction", id)
		}
		got := capEntrySnap{
			status:     TxStatus(after.Status),
			replacedBy: after.ReplacedBy,
			dropReason: after.DropReason,
			dropRound:  after.DropRound,
		}
		if got != before {
			t.Fatalf("tx %s changed by control eviction: before=%+v after=%+v", id, before, got)
		}
	}

	// Alice 的确认结果与确认块完全不受这次合法淘汰影响。
	if alice := n.Account(s.keys[0].pub); alice.ConfirmedSequence != 2 || len(alice.Pending) != 0 {
		t.Fatalf("alice account after control eviction = %+v, want confirmed 2 / no pending", alice)
	}
	if blk, err := n.BlockAt(1); err != nil ||
		!reflect.DeepEqual(blk.TxIDs, []string{s.a1.ID(), s.a2.ID()}) || blk.ID != s.blockID {
		t.Fatalf("confirmed block after control eviction = %+v %v", blk, err)
	}
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size after eviction = %d, want 6", got)
	}
}
