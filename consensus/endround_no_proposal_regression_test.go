package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件为“尚未产生本地提议时主动结束轮次”这一现有功能补充回归保障。
//
// 用户在当前轮次已经提交交易但没有调用 Propose，也可以直接 EndRound：
// 这种结束只推进仿真轮次并处理到期交易，不代表确认了一个空块。节点已有
// 确认历史、账户已确认序号非零时同样适用（主场景构造在第 1 轮确认、当前
// 第 2 轮之上）。
//
// 成功结束后：
//   - 返回轮次与 CurrentRound 都是原轮次 + 1，新轮次仍没有本地提议；
//   - 刚结束的轮次可查：Ended=true、UnconfirmedEnd=true、HasRecords=true（有记录
//     的无候选轮次不能被当作旧版本未保存详情），候选列表为空（不凭空出现空块
//     候选），所有验证者列在未投票名单并按现有公钥次序（十六进制字典序）返回；
//   - 确认高度、最新确认块、已有区块的交易顺序及账户已确认序号保持原样；
//   - 新轮次在尚未生成提议时，Candidates 按现有约定返回 unknown-round。
//
// 轮次推进照常处理到期，到期依据是进入后的轮次（而非费用高低）：
//   - 同一账户的下一笔待确认交易恰好在新轮次到期、更高序号交易仍有效时，前者
//     变为 expired 并保留完整内容供 Tx 查询，不再出现于账户待处理列表；后者继续
//     queued 且账户标注 waiting-pack，最早缺口变成刚退出的序号；
//   - 另一个账户尚未到期的排队交易继续等待打包，不因主动结束进入等待投票；
//   - 完全没有待处理交易时也允许直接结束，轮次记录与确认历史遵守相同规则。
//
// 保存这次结束结果时发生写入错误：返回错误及轮次 0，当前轮次、候选记录、
// 交易状态和账户缺口全部保持操作前结果；不能留下刚结束的轮次记录，也不能
// 提前移除那笔将要到期的交易。保存条件恢复正常后再次结束成功。
//
// 已存在本地提议时的结束、投票确认与各公开调用的兼容性由既有测试覆盖，本文件
// 只补足这一现有操作的回归保障，不新增也不改变任何公开行为。

// noProposalEndMaxTxs 固定场景的单块上限：取足够大的值，使第 1 轮确认块能
// 一次包含两笔交易，后续轮次的池布局只由序号缺口与到期决定。
const noProposalEndMaxTxs = 4

// noProposalEndTxs 汇总主场景中第 2 轮提交的三笔交易。
type noProposalEndTxs struct {
	// gapExpiring 账户1 seq1 fee 高、expiry=3：恰好在进入第 3 轮时到期，
	// 是该账户“下一笔待确认交易”，退出后最早缺口变为 1。
	gapExpiring *Transaction
	// queuedLater 账户1 seq2 expiry=100：序号更高且仍有效，到期后继续
	// queued/waiting-pack；费用低于 gapExpiring，钉住“费用不能改变失效结果”。
	queuedLater *Transaction
	// otherWaiting 账户2 seq1 expiry=100：尚未到期，继续 waiting-pack，
	// 不因主动结束进入 waiting-vote。
	otherWaiting *Transaction
}

// noProposalEndScene 是无候选结束主场景的全部句柄。
type noProposalEndScene struct {
	keys []testKey
	// valCount 为验证者人数（keys 的前 valCount 把）；主场景为 4，
	// 零历史空场景可使用更少验证者。
	valCount      int
	dir           string
	n             *Node
	round1Block   Block // 第 1 轮确认的高度 1 区块（含两笔交易，顺序固定）
	round1TxAlice *Transaction
	round1TxBob   *Transaction
	round2        noProposalEndTxs
}

// buildNoProposalEndScene 在一块确认历史之上构造第 2 轮“已提交但未提议”场景。
//
// 第 1 轮：账户0 seq1（费用 5）与账户3 seq1（费用 8）被本地提议一起确认
// （单块上限 4），账户0、账户3 的已确认序号推进为 1，节点进入第 2 轮、高度 1。
//
// 第 2 轮不调用 Propose，直接提交：
//
//	gapExpiring  账户1 seq1 fee=9 expiry=3    恰好进入轮次 3 时失效
//	queuedLater  账户1 seq2 fee=2 expiry=100  更高序号仍有效（低费用）
//	otherWaiting 账户2 seq1 fee=1 expiry=100  未到期，另一账户
//
// 四把密钥全部为验证者，发送者复用同一名单。
func buildNoProposalEndScene(t *testing.T) *noProposalEndScene {
	t.Helper()
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = keys[i].pub
	}
	n, err := New(dir, Config{
		Seed:           []byte("end-round-no-proposal-seed"),
		Validators:     vals,
		MaxTxsPerBlock: noProposalEndMaxTxs,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 第 1 轮确认历史：两笔不同账户的交易进入同一确认块，费用 8 的账户3
	// 交易按打包规则排在费用 5 的账户0 交易之前。
	alice := NewTransaction(keys[0].priv, 1, []byte("round1-alice"), 5, 100)
	bob := NewTransaction(keys[3].priv, 1, []byte("round1-bob"), 8, 100)
	for _, tx := range []*Transaction{alice, bob} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit %q: %v", tx.Content, err)
		}
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{bob.ID(), alice.ID()}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("setup: round 1 proposal order = %v, want %v", p1.TxIDs, wantOrder)
	}
	// 四验证者门槛为 3 票（严格超过 2/3）；keys[4] 不是验证者，手动投三票。
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, p1.BlockID)
		if err != nil || !res.Counted {
			t.Fatalf("round 1 confirming vote %d: %+v %v", i+1, res, err)
		}
		if (i == 2) != res.Confirmed {
			t.Fatalf("round 1 vote %d confirmed=%v, want %v", i+1, res.Confirmed, i == 2)
		}
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	// 第 2 轮：只提交，不提议。
	scene := &noProposalEndScene{
		keys:          keys,
		valCount:      4,
		dir:           dir,
		n:             n,
		round1Block:   blk,
		round1TxAlice: alice,
		round1TxBob:   bob,
		round2: noProposalEndTxs{
			gapExpiring:  NewTransaction(keys[1].priv, 1, []byte("gap-expiring"), 9, 3),
			queuedLater:  NewTransaction(keys[1].priv, 2, []byte("queued-later"), 2, 100),
			otherWaiting: NewTransaction(keys[2].priv, 1, []byte("other-waiting"), 1, 100),
		},
	}
	for _, tx := range []*Transaction{scene.round2.gapExpiring, scene.round2.queuedLater, scene.round2.otherWaiting} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit %q: %v", tx.Content, err)
		}
	}
	return scene
}

// assertConfirmedBlockPreserved 断言确认高度、最新确认块与高度 1 区块的
// 交易顺序完全保持第 1 轮确认时的原样。
func (s *noProposalEndScene) assertConfirmedBlockPreserved(t *testing.T, n *Node) {
	t.Helper()
	if n.Height() != 1 {
		t.Fatalf("confirmed height = %d, want 1 (ending a proposal-less round confirms nothing)", n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertSameBlock(t, "block at height 1", blk, s.round1Block)
	if latest, ok := n.LatestBlock(); !ok {
		t.Fatal("latest confirmed block disappeared")
	} else {
		assertSameBlock(t, "latest block", latest, s.round1Block)
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 2: %v, want %s", err, ReasonUnknownBlock)
	}
}

// assertConfirmedAccountsPreserved 断言已确认的两笔交易与其账户序号不变。
func (s *noProposalEndScene) assertConfirmedAccountsPreserved(t *testing.T, n *Node) {
	t.Helper()
	for _, x := range []struct {
		tx        *Transaction
		account   []byte
		confirmed uint64
	}{
		{s.round1TxAlice, s.keys[0].pub, 1},
		{s.round1TxBob, s.keys[3].pub, 1},
	} {
		info := n.mustTx(t, x.tx.ID())
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != s.round1Block.ID {
			t.Fatalf("confirmed tx %q changed: %+v", x.tx.Content, info)
		}
		if a := n.Account(x.account); a.ConfirmedSequence != x.confirmed {
			t.Fatalf("account %x confirmed seq = %d, want %d", x.account, a.ConfirmedSequence, x.confirmed)
		}
	}
}

// assertAllValidatorsUnvoted 断言未投票名单恰为全部验证者并按现有公钥次序
// （公钥十六进制字典序）返回，钉住排序约定。
func (s *noProposalEndScene) assertAllValidatorsUnvoted(t *testing.T, rc *RoundCandidates) {
	t.Helper()
	want := make([][]byte, s.valCount)
	for i := range want {
		want[i] = s.keys[i].pub
	}
	// 期望次序即现有公钥次序（十六进制字典序）；got 保持返回时的原始次序，
	// 不再排序，以便真正钉住“按公钥次序返回”而不只是集合相等。
	gotOrder := make([]string, len(rc.Unvoted))
	for i, pub := range rc.Unvoted {
		gotOrder[i] = fmt.Sprintf("%x", pub)
	}
	if fmt.Sprint(gotOrder) != fmt.Sprint(hexKeys(want)) {
		t.Fatalf("unvoted validators = %x\nwant %x (all validators in public-key order)", rc.Unvoted, want)
	}
	if len(rc.Unvoted) != s.valCount {
		t.Fatalf("unvoted count = %d, want %d", len(rc.Unvoted), s.valCount)
	}
}

// assertEndedRecordIsNotEmptyBlock 断言刚结束的无候选轮次：
//   - 已结束、未确认、有详情记录（不是旧版本 DetailMissing 轮）；
//   - 候选列表为空且不凭空出现空块候选；
//   - 全部验证者按公钥次序列为未投票。
func (s *noProposalEndScene) assertEndedRecordIsNotEmptyBlock(t *testing.T, n *Node, endedRound uint64) {
	t.Helper()
	rc, err := n.Candidates(endedRound)
	if err != nil {
		t.Fatalf("Candidates(%d) on just-ended round: %v", endedRound, err)
	}
	if rc.Round != endedRound || !rc.Ended || !rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("ended proposal-less round %d view = %+v, want ended/unconfirmed/with-records", endedRound, rc)
	}
	if len(rc.Candidates) != 0 {
		t.Fatalf("proposal-less end must not fabricate an empty-block candidate: %+v", rc.Candidates)
	}
	s.assertAllValidatorsUnvoted(t, rc)
}

// assertNewRoundHasNoProposal 断言当前轮次为 wantRound、仍无本地提议，
// 且此时 Candidates 按现有约定返回 unknown-round（而不是返回空记录）。
func (s *noProposalEndScene) assertNewRoundHasNoProposal(t *testing.T, n *Node, wantRound uint64) {
	t.Helper()
	if n.CurrentRound() != wantRound {
		t.Fatalf("current round = %d, want %d", n.CurrentRound(), wantRound)
	}
	if p, ok := n.Proposal(); ok {
		t.Fatalf("new round %d must not inherit a local proposal: %+v", wantRound, p)
	}
	if _, err := n.Candidates(wantRound); reason(err) != ReasonUnknownRound {
		t.Fatalf("Candidates(%d) before any proposal = %v, want %s", wantRound, err, ReasonUnknownRound)
	}
}

// assertExpiryOutcome 钉住进入 newRound 后的到期结果：
//   - gapExpiring 查为 expired、完整内容与费用/到期轮次保留、不带确认块或替代关联；
//   - queuedLater 仍 queued/waiting-pack，账户1 只剩它一笔、确认序号仍为 0、
//     最早缺口变成刚退出的序号 1；
//   - otherWaiting 在另一账户继续 queued/waiting-pack，序号 0、无缺口；
//   - 确认历史与账户确认序号不变。
func (s *noProposalEndScene) assertExpiryOutcome(t *testing.T, n *Node, newRound uint64) {
	t.Helper()
	x := s.round2

	// 到期交易变成 expired，完整字段仍可通过 Tx 查询。
	exp := n.mustTx(t, x.gapExpiring.ID())
	if exp.Status != StatusExpired {
		t.Fatalf("gap-expiring status = %s, want expired on entering round %d", exp.Status, newRound)
	}
	if exp.BlockHeight != 0 || exp.BlockID != "" || exp.ReplacedBy != "" ||
		exp.DropReason != "" || exp.DropRound != 0 {
		t.Fatalf("expired tx must carry no block/replace/drop associations: %+v", exp)
	}
	if exp.Tx == nil || string(exp.Tx.Content) != "gap-expiring" ||
		exp.Tx.Sequence != 1 || exp.Tx.Fee != 9 || exp.Tx.Expiry != 3 ||
		fmt.Sprintf("%x", exp.Tx.Sender) != fmt.Sprintf("%x", x.gapExpiring.Sender) ||
		fmt.Sprintf("%x", exp.Tx.Signature) != fmt.Sprintf("%x", x.gapExpiring.Signature) {
		t.Fatalf("expired tx content not preserved in full: %+v", exp.Tx)
	}

	// 更高序号交易仍 queued；账户1 待处理只剩 seq2 一笔且为 waiting-pack；
	// 已确认序号仍为 0，最早缺口变成刚退出的序号 1。
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{
		x.queuedLater.ID(): "waiting-pack",
	})
	if a := n.Account(s.keys[1].pub); a.Gap != 1 {
		t.Fatalf("account 1 gap = %d, want 1 after seq1 expired away", a.Gap)
	}
	ql := n.mustTx(t, x.queuedLater.ID())
	if ql.Status != StatusQueued {
		t.Fatalf("higher-sequence tx status = %s, want queued", ql.Status)
	}

	// 另一账户未到期交易继续等待打包，绝不因主动结束进入等待投票。
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{
		x.otherWaiting.ID(): "waiting-pack",
	})
	if a := n.Account(s.keys[2].pub); a.Gap != 0 {
		t.Fatalf("account 2 gap = %d, want 0", a.Gap)
	}

	s.assertConfirmedAccountsPreserved(t, n)
}

// assertRoundTwoBeforeEnd 断言结束前（或失败结束后）节点仍处于第 2 轮
// 无本地提议状态：三笔交易全部 queued/waiting-pack；账户1 有 seq1、seq2
// 无缺口，账户2 有 seq1；确认历史原样；对第 2 轮的候选查询按 unknown-round
// 处理（没有留下任何轮次记录）。
func (s *noProposalEndScene) assertRoundTwoBeforeEnd(t *testing.T, n *Node) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	if p, ok := n.Proposal(); ok {
		t.Fatalf("no local proposal should exist before EndRound: %+v", p)
	}
	s.assertConfirmedBlockPreserved(t, n)
	s.assertConfirmedAccountsPreserved(t, n)

	x := s.round2
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{
		x.gapExpiring.ID(): "waiting-pack",
		x.queuedLater.ID(): "waiting-pack",
	})
	if a := n.Account(s.keys[1].pub); a.Gap != 0 {
		t.Fatalf("account 1 gap = %d, want 0 before the end", a.Gap)
	}
	assertAccountQueue(t, n, s.keys[2].pub, 0, map[string]string{
		x.otherWaiting.ID(): "waiting-pack",
	})
	for _, tx := range []*Transaction{x.gapExpiring, x.queuedLater, x.otherWaiting} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("tx %q status = %s, want queued before end", tx.Content, info.Status)
		}
	}

	// 尚未产生提议的当前轮次没有轮次记录，候选查询按未知轮次处理；
	// 失败的结束不能在这里留下“已结束”的轮次记录。
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("Candidates(2) without proposal = %v, want %s", err, ReasonUnknownRound)
	}
}

// TestEndRoundWithoutProposalAdvancesAndExpires 覆盖无本地提议结束的主回归序列：
// 第 1 轮有确认历史与非零账户序号，第 2 轮只提交不提议，直接 EndRound 成功
// 推进到第 3 轮并处理到期；空候选结束记录、全部验证者未投票排序、无空块、
// 确认历史与区块顺序不变、新轮 unknown-round、到期/排队/缺口/内容保留全部钉住；
// 成功结果重开后一致。
func TestEndRoundWithoutProposalAdvancesAndExpires(t *testing.T) {
	s := buildNoProposalEndScene(t)
	s.assertRoundTwoBeforeEnd(t, s.n)

	nextRound, err := s.n.EndRound()
	if err != nil {
		t.Fatalf("EndRound without proposal: %v", err)
	}
	if nextRound != 3 {
		t.Fatalf("EndRound return = %d, want 3", nextRound)
	}
	s.assertNewRoundHasNoProposal(t, s.n, 3)
	s.assertEndedRecordIsNotEmptyBlock(t, s.n, 2)
	s.assertConfirmedBlockPreserved(t, s.n)
	s.assertExpiryOutcome(t, s.n, 3)

	// 在第 3 轮仍不提议、直接再结束一次：池内剩余两笔交易到期轮次为 100，
	// 既不会被打包也不会到期，同样允许无候选结束并进入第 4 轮；轮次记录与
	// 确认历史遵守相同规则。（完全空池的结束由 FreshNodeEmptyPool 用例覆盖。）
	nextRound, err = s.n.EndRound()
	if err != nil {
		t.Fatalf("second EndRound with empty pool: %v", err)
	}
	if nextRound != 4 {
		t.Fatalf("second EndRound return = %d, want 4", nextRound)
	}
	s.assertNewRoundHasNoProposal(t, s.n, 4)
	s.assertEndedRecordIsNotEmptyBlock(t, s.n, 3)
	s.assertConfirmedBlockPreserved(t, s.n)
	s.assertExpiryOutcome(t, s.n, 3)

	// 成功结果持久化：重开节点看到完全一致的第 4 轮状态与两条结束记录。
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.assertNewRoundHasNoProposal(t, reopened, 4)
	s.assertEndedRecordIsNotEmptyBlock(t, reopened, 2)
	s.assertEndedRecordIsNotEmptyBlock(t, reopened, 3)
	s.assertConfirmedBlockPreserved(t, reopened)
	s.assertExpiryOutcome(t, reopened, 3)
}

// TestEndRoundWithoutProposalFreshNodeEmptyPool 钉住零历史节点也允许在
// 没有任何待处理交易、没有本地提议时直接结束：返回原轮次 + 1，结束记录为空、
// 未确认且有详情，全部验证者未投票；高度仍为 0，新轮无提议时候选查询为
// unknown-round；连续两次结束行为一致。
func TestEndRoundWithoutProposalFreshNodeEmptyPool(t *testing.T) {
	keys := genKeys(t, 3)
	n, _ := newTestNode(t, keys, 2)
	scene := &noProposalEndScene{keys: keys, valCount: 3}

	if n.Height() != 0 {
		t.Fatalf("fresh height = %d, want 0", n.Height())
	}
	r, err := n.EndRound()
	if err != nil {
		t.Fatalf("EndRound on fresh empty node: %v", err)
	}
	if r != 2 || n.CurrentRound() != 2 {
		t.Fatalf("round after end = %d (current %d), want 2", r, n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height after empty end = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("empty proposal-less end must not produce a latest block")
	}
	scene.assertEndedRecordIsNotEmptyBlock(t, n, 1)
	scene.assertNewRoundHasNoProposal(t, n, 2)

	r, err = n.EndRound()
	if err != nil {
		t.Fatalf("second EndRound on fresh node: %v", err)
	}
	if r != 3 || n.CurrentRound() != 3 {
		t.Fatalf("round after second end = %d (current %d), want 3", r, n.CurrentRound())
	}
	scene.assertEndedRecordIsNotEmptyBlock(t, n, 2)
	scene.assertNewRoundHasNoProposal(t, n, 3)
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0 after two empty ends", n.Height())
	}
}

// TestEndRoundWithoutProposalFeeDoesNotChangeEviction 钉住到期只取决于
// “进入后的轮次”：下一笔待确认交易即使费用很高，也必须在新轮次恰为其
// 到期轮次时失效；更高序号的零费用交易（不能被前者“带过去”）继续排队。
func TestEndRoundWithoutProposalFeeDoesNotChangeEviction(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)
	// 当前第 2 轮：账户0 已确认 seq1（第 1 轮空历史不构造，直接利用初始状态
	// 在第 1 轮提交并确认一笔），使 seq2 成为“账户已确认序号非零”的下一笔。
	first := NewTransaction(keys[0].priv, 1, []byte("first"), 1, 100)
	if _, err := n.Submit(first); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if res, err := n.Vote(keys[i].pub, 1, p1.BlockID); err != nil || !res.Counted {
			t.Fatalf("confirm vote %d: %+v %v", i+1, res, err)
		}
	}
	if n.CurrentRound() != 2 {
		t.Fatalf("setup: current round = %d, want 2", n.CurrentRound())
	}

	// seq2 费用极高但恰好进入第 3 轮到期；seq3 费用为 0 但到期更远，必须留下。
	head := NewTransaction(keys[0].priv, 2, []byte("high-fee-expiring"), 1_000_000, 3)
	tail := NewTransaction(keys[0].priv, 3, []byte("zero-fee-valid"), 0, 100)
	if _, err := n.Submit(head); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(tail); err != nil {
		t.Fatal(err)
	}

	if r, err := n.EndRound(); err != nil || r != 3 {
		t.Fatalf("EndRound = %d, %v, want 3", r, err)
	}
	hinfo := n.mustTx(t, head.ID())
	if hinfo.Status != StatusExpired {
		t.Fatalf("high-fee head tx status = %s, want expired (fee must not override expiry)", hinfo.Status)
	}
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{tail.ID(): "waiting-pack"})
	if a := n.Account(keys[0].pub); a.Gap != 2 {
		t.Fatalf("account gap = %d, want 2 (the just-expired sequence)", a.Gap)
	}
	tinfo := n.mustTx(t, tail.ID())
	if tinfo.Status != StatusQueued {
		t.Fatalf("zero-fee tail tx status = %s, want queued", tinfo.Status)
	}
}

// TestEndRoundWithoutProposalEmptyPoolAfterHistory 钉住“完全没有待处理交易”
// 与“已有确认历史、账户已确认序号非零”两个条件同时成立时也允许直接结束：
// 第 1 轮确认一笔后进入第 2 轮，第 2 轮不提交任何交易、不提议，直接 EndRound。
// 结束记录为空、未确认且有详情，全部验证者未投票；确认高度、最新确认块与区块
// 交易顺序保持原样，已确认账户序号不回退；新轮次仍无提议、候选查询 unknown-round。
func TestEndRoundWithoutProposalEmptyPoolAfterHistory(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 4)
	scene := &noProposalEndScene{keys: keys, valCount: 4, dir: dir, n: n}

	confirmed := NewTransaction(keys[0].priv, 1, []byte("only-confirmed"), 7, 100)
	if _, err := n.Submit(confirmed); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint([]string{confirmed.ID()}) {
		t.Fatalf("setup: round 1 proposal = %v, want [%s]", p1.TxIDs, confirmed.ID())
	}
	for i := 0; i < 3; i++ {
		if res, err := n.Vote(keys[i].pub, 1, p1.BlockID); err != nil || !res.Counted {
			t.Fatalf("confirm vote %d: %+v %v", i+1, res, err)
		}
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	scene.round1Block = blk

	// 第 2 轮：池内完全没有待处理交易，也没有本地提议——直接结束。
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 1 || len(a.Pending) != 0 {
		t.Fatalf("setup: account 0 = %+v, want confirmed seq 1 with empty pending", a)
	}
	r, err := n.EndRound()
	if err != nil {
		t.Fatalf("EndRound with empty pool after history: %v", err)
	}
	if r != 3 || n.CurrentRound() != 3 {
		t.Fatalf("round after end = %d (current %d), want 3", r, n.CurrentRound())
	}
	scene.assertEndedRecordIsNotEmptyBlock(t, n, 2)
	scene.assertNewRoundHasNoProposal(t, n, 3)
	scene.assertConfirmedBlockPreserved(t, n)
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 1 || len(a.Pending) != 0 || a.Gap != 0 {
		t.Fatalf("confirmed account changed after empty end: %+v", a)
	}
	info := n.mustTx(t, confirmed.ID())
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != blk.ID {
		t.Fatalf("confirmed tx changed: %+v", info)
	}

	// 持久化：重开后空结束记录与确认历史一致。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	scene.assertEndedRecordIsNotEmptyBlock(t, reopened, 2)
	scene.assertNewRoundHasNoProposal(t, reopened, 3)
	scene.assertConfirmedBlockPreserved(t, reopened)
}

// TestEndRoundWithoutProposalSaveFailureAtomic 钉住无候选结束保存失败时的
// 原子性：返回错误与轮次 0，当前轮次、候选记录（第 2 轮仍 unknown-round、
// 不留结束记录）、交易状态与账户缺口全部保持操作前；磁盘文件逐字节不变、
// 无临时文件，重开一致；恢复后再次 EndRound 按规则成功并持久化。
func TestEndRoundWithoutProposalSaveFailureAtomic(t *testing.T) {
	s := buildNoProposalEndScene(t)
	s.assertRoundTwoBeforeEnd(t, s.n)

	statePath := filepath.Join(s.dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 无候选结束因写入错误失败：必须返回错误与轮次 0，且是基础设施错误而非拒绝。
	s.n.injectSaveErr = errors.New("disk full (simulated)")
	nextRound, err := s.n.EndRound()
	s.n.injectSaveErr = nil
	if err == nil {
		t.Fatal("EndRound must return an error when saving fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as infrastructure error, not rejection %q: %v", reason(err), err)
	}
	if nextRound != 0 {
		t.Fatalf("returned round = %d, want 0 on save failure", nextRound)
	}

	// 内存：轮次、候选记录、交易状态与账户缺口全部保持操作前。
	s.assertRoundTwoBeforeEnd(t, s.n)

	// 磁盘：文件逐字节不变，目录中只有 state.json，不残留临时文件，
	// 也没有提前移除那笔将要到期的交易。
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("failed save must not leave temp files, dir contains %v", names)
	}

	// 重开：同样停在第 2 轮无提议状态，那笔将到期交易仍在池中。
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.assertRoundTwoBeforeEnd(t, reopened)

	// 保存恢复正常后再次结束：按规则成功进入第 3 轮并持久化。
	nextRound, err = reopened.EndRound()
	if err != nil {
		t.Fatalf("EndRound after recovery: %v", err)
	}
	if nextRound != 3 {
		t.Fatalf("EndRound after recovery = %d, want 3", nextRound)
	}
	s.assertNewRoundHasNoProposal(t, reopened, 3)
	s.assertEndedRecordIsNotEmptyBlock(t, reopened, 2)
	s.assertConfirmedBlockPreserved(t, reopened)
	s.assertExpiryOutcome(t, reopened, 3)

	durable, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.assertNewRoundHasNoProposal(t, durable, 3)
	s.assertEndedRecordIsNotEmptyBlock(t, durable, 2)
	s.assertExpiryOutcome(t, durable, 3)
}
