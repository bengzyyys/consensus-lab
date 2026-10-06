package consensus

import (
	"reflect"
	"testing"
)

// 回归保障：满池时提交无效交易，拒绝原因必须反映交易本身的问题
// （bad-signature / tx-expired），不能被容量判断统一变成 pool-full；
// 更不能先挤出一笔合法交易再报告失败。场景复用 capProtectScene：
// 容量 6 恰好占满，lp/cp/shared 被未决候选引用等待投票，q1..q3 排队
// 可淘汰（q1 费用 50 最低）。新交易均来自 keys[4]（已确认序号 0、无待
// 处理交易），序号 1，费用 51 严格高于 q1，标识从未被节点接受——若通过
// 校验，按原有容量规则会获准入池并挤出 q1。

// fullPoolRejectSnap 在拒绝后断言场景完全冻结：视图、条目、账户与提交前
// 一致，轮次不变，q1 没有任何淘汰记录，池仍恰好满。
func fullPoolRejectSnap(t *testing.T, s *capProtectScene,
	beforeEntries map[string]capEntrySnap, beforeAccounts map[string]string,
	beforeLocal ProposalView, beforeRC *RoundCandidates,
) {
	t.Helper()
	n := s.n
	s.assertFrozenViews(t, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
	if got := n.CurrentRound(); got != 1 {
		t.Fatalf("round = %d, rejected submit must not advance the round", got)
	}
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size = %d, want 6 (rejection must not free a slot)", got)
	}
	// q1 仍在账户待处理列表中：内容、序号与等待打包说明不变，没有新增淘汰记录。
	info, err := n.Tx(s.q1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued || info.DropReason != "" || info.DropRound != 0 {
		t.Fatalf("q1 after rejection = %+v, want untouched queued", info)
	}
	assertTxMatches(t, info.Tx, s.q1)
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{
		s.q1.ID(): "waiting-pack", s.q2.ID(): "waiting-pack", s.q3.ID(): "waiting-pack",
	})
	// 新交易发送者的已确认序号保持 0，且没有留下任何待处理交易。
	assertAccountQueue(t, n, s.keys[4].pub, 0, map[string]string{})
}

// assertRejectedUnknown 断言提交以指定原因拒绝、不返回成功结果，
// 且按该交易标识查询得到 unknown-transaction（不留历史）。
func assertRejectedUnknown(t *testing.T, n *Node, tx *Transaction, wantReason string) {
	t.Helper()
	res, err := n.Submit(tx)
	if reason(err) != wantReason {
		t.Fatalf("submit got %v, want %s", err, wantReason)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil", res)
	}
	if _, err := n.Tx(tx.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx query = %v, want %s", err, ReasonUnknownTx)
	}
}

// assertValidSubmitEvictsQ1 在拒绝之后提交一笔签名有效、未到期的全新交易：
// 应正常入池并挤出 q1，受候选保护的交易继续保留。
func assertValidSubmitEvictsQ1(t *testing.T, s *capProtectScene, beforeLocal ProposalView, beforeRC *RoundCandidates) {
	t.Helper()
	n := s.n
	win := NewTransaction(s.keys[4].priv, 1, []byte("valid-winner"), 51, 100)
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("valid higher-fee tx must be admitted after rejections: %v", err)
	}
	if res.TxID != win.ID() || res.EvictedID != s.q1.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want accept with evicted q1 and no replace link", res)
	}
	if info, _ := n.Tx(win.ID()); info.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", info.Status)
	}
	if info, _ := n.Tx(s.q1.ID()); info.Status != StatusDropped || info.DropReason != DropReasonPoolCapacity || info.DropRound != 1 {
		t.Fatalf("q1 after valid submit = %+v, want dropped/pool-capacity/round 1", info)
	}
	// 受候选保护的交易继续保留等待投票，候选视图不受淘汰影响。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		if info, _ := n.Tx(tx.ID()); info.Status != StatusProposed {
			t.Fatalf("protected tx %s = %s, want proposed", tx.ID(), info.Status)
		}
	}
	assertAccountQueue(t, n, s.keys[0].pub, 0, map[string]string{s.lp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{s.cp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})
	if got := mustCandidates(t, n, 1); !reflect.DeepEqual(got, beforeRC) {
		t.Fatalf("candidates changed by eviction:\nbefore=%+v\nafter =%+v", beforeRC, got)
	}
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size after eviction = %d, want 6", got)
	}
}

// 满池时签名与交易内容不匹配（公钥与签名长度均正确）必须返回 bad-signature，
// 而不是 pool-full；该交易同时已经到期时结果不变。拒绝不挤出任何交易，
// 之后合法交易仍按原有容量规则入池并挤出 q1。
func TestFullPoolRejectsBadSignature(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	// 签名长度正确但内容与签名不匹配；费用 51 本可挤出 q1。
	badSig := NewTransaction(s.keys[4].priv, 1, []byte("bad-sig"), 51, 100)
	badSig.Signature[0] ^= 0xff
	if badSig.Verify() {
		t.Fatal("test setup: tampered signature must not verify")
	}
	assertRejectedUnknown(t, n, badSig, ReasonBadSignature)

	// 同时已经到期（到期轮次等于当前轮次 1）的坏签名交易：仍返回 bad-signature。
	badSigExpired := NewTransaction(s.keys[4].priv, 1, []byte("bad-sig-expired"), 51, 1)
	badSigExpired.Signature[0] ^= 0xff
	if badSigExpired.Verify() {
		t.Fatal("test setup: tampered signature must not verify")
	}
	assertRejectedUnknown(t, n, badSigExpired, ReasonBadSignature)

	fullPoolRejectSnap(t, s, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
	assertValidSubmitEvictsQ1(t, s, beforeLocal, beforeRC)
}

// 满池时签名有效但已到期（到期轮次不大于当前轮次）的全新交易必须返回
// tx-expired，而不是 pool-full；到期轮次恰好等于当前轮次也属于拒绝范围。
// 拒绝不挤出任何交易，之后合法交易仍按原有容量规则入池并挤出 q1。
func TestFullPoolRejectsExpired(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	// 到期轮次恰好等于当前轮次 1：拒绝。
	expiredEqual := NewTransaction(s.keys[4].priv, 1, []byte("expired-equal"), 51, 1)
	if !expiredEqual.Verify() {
		t.Fatal("test setup: signature must be valid")
	}
	assertRejectedUnknown(t, n, expiredEqual, ReasonExpired)

	// 到期轮次小于当前轮次：同样拒绝。
	expiredBefore := NewTransaction(s.keys[4].priv, 1, []byte("expired-before"), 51, 0)
	assertRejectedUnknown(t, n, expiredBefore, ReasonExpired)

	fullPoolRejectSnap(t, s, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
	assertValidSubmitEvictsQ1(t, s, beforeLocal, beforeRC)
}
