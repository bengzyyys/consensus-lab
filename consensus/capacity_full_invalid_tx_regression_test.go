package consensus

import (
	"crypto/ed25519"
	"reflect"
	"testing"
)

// 满池时“交易自身校验优先于容量判断”的回归保障。
//
// 背景风险：池满时若容量判断先行（或先落淘汰再做校验），一笔本应因签名/到期被
// 拒绝的交易会被统一报成 pool-full，甚至先挤出一笔合法排队交易、再报告提交失败。
// 现有 Submit 规则要求校验与拒绝原因优先于容量判断：坏签名返回 bad-signature，
// 已到期返回 tx-expired，拒绝不留历史、不淘汰任何交易；规则下本可入池的合法新
// 交易在此后仍按原容量规则挤出费用最低的排队交易，受候选引用的交易继续保留。
//
// 场景直接复用 newCapProtectScene：容量 6 恰好占满，lp/cp/shared 三笔被未决候选
// 引用、等待投票（竞争候选已获一票），q1/q2/q3 排队且 q1(费用 50) 是唯一可淘汰
// 交易；keys[4] 是已确认序号 0、没有待处理交易的全新发送者。下列新交易序号均为 1、
// 费用 51 严格高于 q1，标识从未被节点接受——它们若通过交易校验，本应按容量规则
// 获准入池并挤出 q1，因此任何拒绝都只能来自交易本身。

// assertInvalidFullPoolSubmitRejected 是拒绝路径的共用断言：
// reason 必须等于 want；提交结果必须为 nil；按该交易自身标识查询得到
// unknown-transaction；池内一切视图（条目、账户、本地提议、候选与票数、高度）
// 与提交前快照逐字节一致；唯一可淘汰的 q1 内容原样保留、无淘汰记录。
func assertInvalidFullPoolSubmitRejected(
	t *testing.T, s *capProtectScene, bad *Transaction, want string,
	beforeEntries map[string]capEntrySnap, beforeAccounts map[string]string,
	beforeLocal ProposalView, beforeRC *RoundCandidates,
) {
	t.Helper()
	n := s.n
	id := bad.ID()
	res, err := n.Submit(bad)
	if got := reason(err); got != want {
		t.Fatalf("submit reason = %q (%v), want %s (must not collapse to %s)", got, err, want, ReasonPoolFull)
	}
	if res != nil {
		t.Fatalf("rejected submit returned result %+v, want nil (no accept, no eviction link)", res)
	}
	// 从未被节点接受：按它自己的标识查询只能得到 unknown-transaction。
	if _, err := n.Tx(id); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx %s query = %v, want %s", id, err, ReasonUnknownTx)
	}

	// 可淘汰的排队交易 q1 必须原样留下：仍在排队、内容与序号不变、没有淘汰记录。
	q1 := mustTx(t, n, s.q1.ID())
	if q1.Status != StatusQueued || q1.DropReason != "" || q1.DropRound != 0 {
		t.Fatalf("q1 after invalid submit = status:%s drop:%s/%d, want untouched queued with no drop record",
			q1.Status, q1.DropReason, q1.DropRound)
	}
	assertTxMatches(t, q1.Tx, s.q1)
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{
		s.q1.ID(): "waiting-pack", s.q2.ID(): "waiting-pack", s.q3.ID(): "waiting-pack",
	})

	// 受候选保护的三笔交易继续等待投票，候选区块标识、交易顺序与已有票数不动。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		if info := mustTx(t, n, tx.ID()); info.Status != StatusProposed {
			t.Fatalf("protected tx %s status = %s, want proposed", tx.ID(), info.Status)
		}
	}

	// 条目、账户（含已确认序号）、本地提议、候选、确认高度全部冻结。
	s.assertFrozenViews(t, beforeEntries, beforeAccounts, beforeLocal, beforeRC)
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, rejection must not advance round", n.CurrentRound())
	}
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size = %d, want still 6 (nothing admitted nor evicted)", got)
	}
}

// 公钥与签名长度正确、签名却与交易内容不匹配：即使费用足以挤出 q1，也必须返回
// bad-signature 而不是 pool-full；该交易同时已到期时结果仍是 bad-signature
// （签名校验先于到期判断），两种情况下都不得先挤出 q1。
func TestCapacityFullInvalidSignatureRejectedBeforeCapacity(t *testing.T) {
	cases := []struct {
		name   string
		expiry uint64
	}{
		{"signature mismatch while not expired", 100},
		{"signature mismatch and already expired at current round", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCapProtectScene(t)
			beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

			bad := NewTransaction(s.keys[4].priv, 1, []byte("forged-"+tc.name), 51, tc.expiry)
			// 公钥与签名长度均正确，仅翻转一个签名字节使签名与交易内容不匹配。
			if len(bad.Sender) != ed25519.PublicKeySize || len(bad.Signature) != ed25519.SignatureSize {
				t.Fatalf("test setup: sender/sig length = %d/%d, want %d/%d",
					len(bad.Sender), len(bad.Signature), ed25519.PublicKeySize, ed25519.SignatureSize)
			}
			bad.Signature[0] ^= 0xff
			if bad.Verify() {
				t.Fatal("test setup: tampered signature must not verify")
			}

			assertInvalidFullPoolSubmitRejected(t, s, bad, ReasonBadSignature,
				beforeEntries, beforeAccounts, beforeLocal, beforeRC)
		})
	}
}

// 签名有效但到期轮次不大于当前轮次的全新交易返回 tx-expired：
// 到期轮次恰好等于当前轮次（1 == 1）即在拒绝范围内，更早到期同理。
// 拒绝同样不能被池满掩盖成 pool-full，也不能触发淘汰。
func TestCapacityFullExpiredRejectedBeforeCapacity(t *testing.T) {
	cases := []struct {
		name   string
		expiry uint64
	}{
		{"expiry equals current round", 1}, // 边界：相等也算到期
		{"expiry earlier than current round", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCapProtectScene(t)
			n := s.n
			beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

			expired := NewTransaction(s.keys[4].priv, 1, []byte("expired-"+tc.name), 51, tc.expiry)
			if !expired.Verify() {
				t.Fatal("test setup: transaction signature must be valid")
			}
			if tc.expiry > n.CurrentRound() {
				t.Fatalf("test setup: expiry %d must be <= current round %d", tc.expiry, n.CurrentRound())
			}

			assertInvalidFullPoolSubmitRejected(t, s, expired, ReasonExpired,
				beforeEntries, beforeAccounts, beforeLocal, beforeRC)
		})
	}
}

// 无效交易被拒之后，规则没有任何变化：随后一笔签名有效、尚未到期且费用严格高于
// q1 的全新交易仍正常入池，提交结果明确指出被挤出的排队交易 q1；受候选引用的
// 交易继续保留，候选顺序与已有票数不变。
func TestCapacityFullInvalidRejectionThenValidSubmitEvicts(t *testing.T) {
	s := newCapProtectScene(t)
	n := s.n
	beforeEntries, beforeAccounts, beforeLocal, beforeRC := s.snapshots(t)

	// 先提交一笔本可挤出 q1（费用 51 > 50）却坏签名的交易：必须被拒且不动池状态。
	forged := NewTransaction(s.keys[4].priv, 1, []byte("forged"), 51, 100)
	forged.Signature[0] ^= 0xff
	assertInvalidFullPoolSubmitRejected(t, s, forged, ReasonBadSignature,
		beforeEntries, beforeAccounts, beforeLocal, beforeRC)

	// 再提交同发送者、同序号位、费用相同但签名有效、尚未到期的全新交易：
	// 按原有容量规则获准入池并挤出唯一可淘汰的排队交易 q1。
	win := NewTransaction(s.keys[4].priv, 1, []byte("winner"), 51, 100)
	if win.ID() == forged.ID() {
		t.Fatal("test setup: valid tx must have a distinct id from the rejected one")
	}
	res, err := n.Submit(win)
	if err != nil {
		t.Fatalf("valid higher-fee tx must be admitted under normal capacity rules: %v", err)
	}
	if res.TxID != win.ID() || res.EvictedID != s.q1.ID() || res.ReplacedID != "" {
		t.Fatalf("submit result = %+v, want accept with EvictedID=q1 and no replace link", res)
	}

	// 被挤出的 q1：dropped / pool-capacity / 发生轮次 1，完整内容保留，
	// 不带确认块或替换关联。
	q1 := mustTx(t, n, s.q1.ID())
	if q1.Status != StatusDropped || q1.DropReason != DropReasonPoolCapacity || q1.DropRound != 1 {
		t.Fatalf("q1 after valid submit = %+v, want dropped/pool-capacity/round 1", q1)
	}
	if q1.BlockHeight != 0 || q1.BlockID != "" || q1.ReplacedBy != "" {
		t.Fatalf("evicted q1 must not carry block/replace links: %+v", q1)
	}
	assertTxMatches(t, q1.Tx, s.q1)

	// 新交易排队等待打包；被拒的伪造交易依然查无此交易。
	if info := mustTx(t, n, win.ID()); info.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", info.Status)
	}
	assertAccountQueue(t, n, s.keys[4].pub, 0, map[string]string{win.ID(): "waiting-pack"})
	if _, err := n.Tx(forged.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("earlier rejected tx query = %v, want %s", err, ReasonUnknownTx)
	}

	// q1 退出后，其账户留下序号 1 的缺口，q2/q3 内容与等待打包说明不变，
	// 已确认序号不推进。
	acct3 := n.Account(s.keys[3].pub)
	if acct3.ConfirmedSequence != 0 || acct3.Gap != 1 || len(acct3.Pending) != 2 {
		t.Fatalf("q1 sender account = %+v, want confirmed 0 gap 1 two pending", acct3)
	}
	assertAccountQueue(t, n, s.keys[3].pub, 0, map[string]string{
		s.q2.ID(): "waiting-pack", s.q3.ID(): "waiting-pack",
	})
	assertTxMatches(t, mustTx(t, n, s.q2.ID()).Tx, s.q2)
	assertTxMatches(t, mustTx(t, n, s.q3.ID()).Tx, s.q3)

	// 受候选保护的三笔交易不因入池/淘汰改变：继续等待投票，内容原样保留。
	for _, tx := range []*Transaction{s.lp, s.cp, s.shared} {
		info := mustTx(t, n, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("protected tx %s status = %s, want proposed", tx.ID(), info.Status)
		}
		assertTxMatches(t, info.Tx, tx)
	}
	assertAccountQueue(t, n, s.keys[0].pub, 0, map[string]string{s.lp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{s.cp.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{s.shared.ID(): "waiting-vote"})

	// 候选的区块标识、交易顺序与竞争候选已有的一票保持原样；本地提议同样不变。
	if got, ok := n.Proposal(); !ok || !reflect.DeepEqual(got, beforeLocal) {
		t.Fatalf("local proposal changed:\nbefore=%+v\nafter =%+v ok=%v", beforeLocal, got, ok)
	}
	rc := mustCandidates(t, n, 1)
	if !reflect.DeepEqual(rc, beforeRC) {
		t.Fatalf("candidates view changed:\nbefore=%+v\nafter =%+v", beforeRC, rc)
	}
	local := candidateByBlockID(rc, s.local.BlockID)
	alt := candidateByBlockID(rc, s.altID)
	if local == nil || alt == nil || local.Result != CandidatePending || alt.Result != CandidatePending {
		t.Fatalf("candidate state wrong: local=%+v alt=%+v", local, alt)
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

	// 挤一出一，池仍恰好满；轮次与确认高度不变。
	if got := n.st.poolSize(); got != 6 {
		t.Fatalf("pool size = %d, want 6 after one-for-one eviction", got)
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0", n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("eviction must not leave a confirmed block")
	}
}
