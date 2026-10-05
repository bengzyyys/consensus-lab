package consensus

import (
	"fmt"
	"testing"
)

// 本文件钉住竞争候选登记的账户序号规则回归保障：交易可以先进入池中等待，
// 候选却不能借用没有写进自己列表的前序交易；不同账户可以交错出现，同一账户
// 仍须按列表中的实际出现顺序，从该账户已确认序号加一开始连续递增。
//
// 场景：第 1 轮确认 a1、a2、b1，使三个账户的已确认序号分别为 2、1、0（互不
// 相同，且至少一个非零）。第 2 轮本地提议已产生且尚未确认，按费用打包
// [a3, b2, c1, a4]；b3、c2 在提议冻结后才提交，仅在池中排队。既有竞争候选
// alt1=[c1, c2] 持有一张未达门槛的票。随后登记的新候选列表 [b2, a3, c1, b3, a4]
// 与本地提议顺序不同：相邻交易来自不同账户，费用高低也不按本地打包顺序排列，
// 但每个账户自己的序号连续（A:3,4；B:2,3；C:1），按现有规则必须登记成功。
//
// 登记成功的公开效果是：新候选入册，按轮次查询保留调用者给出的完整交易顺序，
// 不按账户、序号或费用重排；本地提议保持原来的标识与顺序；登记不推进任何
// 账户的已确认序号、不产生确认块；新候选实际引用的排队交易 b3 才转为等待
// 投票，已有候选引用的交易（含 alt1 独有的 c2）继续保持等待投票。
//
// 拒绝路径区分两种容易误判的情况，都以 sequence-not-consecutive 拒绝：
//   - 某账户需要的前序交易虽在池中（甚至已被本地提议引用），但候选从更后的
//     序号开始或中途跳过它；
//   - 某账户的连续序号交易都在列表里，但后一个序号先于前一个出现，其他账户
//     夹在中间不能掩盖倒序。
// 这些列表都满足其他登记条件（不超单块上限、无未知或重复交易、均在池中），
// 不能让其他原因代替序号拒绝。拒绝后候选集合、本地提议与既有候选票数保持
// 原样；原本只在排队的交易仍等待打包、可加费替换，原本等待投票的交易仍被
// 锁定、不能被解锁。
//
// 本组测试不新增公开入口，也不改变现有登记、投票与交易规则。

// seqScene 收拢“账户序号规则”场景的关键句柄。
type seqScene struct {
	block1 Block
	local  ProposalView
	alt1ID string
	// a3、a4、b2、c1 被本地提议引用：等待投票。
	a3 *Transaction
	a4 *Transaction
	b2 *Transaction
	c1 *Transaction
	// b3 在本地提议冻结后才提交，仅排队；新候选首次引用它。
	b3 *Transaction
	// c2 仅被既有竞争候选 alt1 引用：等待投票。
	c2 *Transaction
	// list 是待登记的竞争候选列表，顺序与本地提议不同。
	list []string
}

// buildSeqScene 构造第 2 轮未决场景：确认历史使账户 A/B/C 的已确认序号为
// 2/1/0；本地提议 [a3, b2, c1, a4]；b3、c2 仅排队；alt1=[c2] 持一票。
func buildSeqScene(t *testing.T, n *Node, keys []testKey) seqScene {
	t.Helper()
	// 第 1 轮：确认 a1、a2、b1，推进已确认序号 A=2、B=1，C 保持 0。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 10, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	confirmByVotes(t, n, keys, p1.BlockID)
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	block1, ok := n.LatestBlock()
	if !ok || block1.Height != 1 {
		t.Fatalf("setup: latest block = %+v ok=%v, want height 1", block1, ok)
	}
	for i, want := range []uint64{2, 1, 0} {
		if got := n.Account(keys[i].pub).ConfirmedSequence; got != want {
			t.Fatalf("setup: account %d confirmed sequence = %d, want %d", i, got, want)
		}
	}

	// 第 2 轮：a3、a4、b2、c1 在本地提议前提交，按费用被打包进提议。
	a3 := NewTransaction(keys[0].priv, 3, []byte("a3"), 50, 100)
	a4 := NewTransaction(keys[0].priv, 4, []byte("a4"), 10, 100)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 40, 100)
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 20, 100)
	for _, tx := range []*Transaction{a3, a4, b2, c1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantLocal := []string{a3.ID(), b2.ID(), c1.ID(), a4.ID()}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("setup: local proposal = %v, want %v", local.TxIDs, wantLocal)
	}

	// 提议冻结后才提交的交易仅在池中排队，不被本地提议引用。
	b3 := NewTransaction(keys[1].priv, 3, []byte("b3"), 30, 100)
	c2 := NewTransaction(keys[2].priv, 2, []byte("c2"), 15, 100)
	for _, tx := range []*Transaction{b3, c2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
		if info := n.mustTx(t, tx.ID()); info.Status != StatusQueued {
			t.Fatalf("setup: %q status = %s, want queued", tx.Content, info.Status)
		}
	}

	// 既有竞争候选引用本地提议的 c1 与排队交易 c2（C 的序号自 1 起连续），
	// 并持有一张未达门槛的票；c2 由此只被既有候选引用。
	alt1, err := n.RegisterCandidate(2, []string{c1.ID(), c2.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if alt1.Existing || alt1.BlockID == local.BlockID {
		t.Fatalf("setup: alt1 must be a fresh candidate: %+v local=%s", alt1, local.BlockID)
	}
	if res, err := n.Vote(keys[0].pub, 2, alt1.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("setup vote for alt1: %+v %v", res, err)
	}

	// 新候选列表：相邻交易来自不同账户，费用不按本地打包顺序排列，
	// 但每个账户自己的序号从已确认序号加一起连续。
	list := []string{b2.ID(), a3.ID(), c1.ID(), b3.ID(), a4.ID()}
	return seqScene{
		block1: block1,
		local:  local,
		alt1ID: alt1.BlockID,
		a3:     a3,
		a4:     a4,
		b2:     b2,
		c1:     c1,
		b3:     b3,
		c2:     c2,
		list:   list,
	}
}

// assertSeqRoundStable 断言失败登记后必须保持不变的轮次状态：轮次 2、确认
// 高度 1；本地提议标识与顺序不变；候选集合仍只有本地提议与 alt1，alt1 保留
// 原票，未投票名单不变。
func assertSeqRoundStable(t *testing.T, n *Node, sc seqScene, keys []testKey) {
	t.Helper()
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 (rejected registers must not advance anything)",
			n.CurrentRound(), n.Height())
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != sc.block1.ID {
		t.Fatalf("latest block = %+v ok=%v, want %s", latest, ok, sc.block1.ID)
	}
	p, ok := n.Proposal()
	if !ok || p.BlockID != sc.local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(sc.local.TxIDs) {
		t.Fatalf("local proposal changed: %+v ok=%v, want %+v", p, ok, sc.local)
	}

	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords || len(rc.Candidates) != 2 {
		t.Fatalf("round 2 must keep exactly the two original pending candidates: %+v", rc)
	}
	wantUnvoted := [][]byte{keys[1].pub, keys[2].pub, keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}
	var gotLocal, gotAlt *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case sc.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case sc.alt1ID:
			gotAlt = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil {
		t.Fatalf("only the original candidates may remain: %+v", rc.Candidates)
	}
	if !gotLocal.Local || gotLocal.Result != CandidatePending ||
		fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(sc.local.TxIDs) || len(gotLocal.Voters) != 0 {
		t.Fatalf("local candidate changed: %+v", gotLocal)
	}
	if gotAlt.Local || gotAlt.Result != CandidatePending ||
		fmt.Sprint(gotAlt.TxIDs) != fmt.Sprint([]string{sc.c1.ID(), sc.c2.ID()}) ||
		len(gotAlt.Voters) != 1 || fmt.Sprintf("%x", gotAlt.Voters[0]) != fmt.Sprintf("%x", keys[0].pub) {
		t.Fatalf("existing competitor or its vote changed: %+v", gotAlt)
	}
}

// TestRegisterCandidateInterleavedSequences 钉住接受路径：当前轮次已有本地
// 提议且尚未确认时，调用者可以登记与本地提议顺序不同的交易列表——各账户
// 已确认序号不同（2/1/0，至少一个非零），相邻交易来自不同账户、费用不按
// 本地打包顺序排列，只要各账户自己的序号连续就必须成功；查询保留调用者
// 给出的完整顺序，本地提议冻结，登记不推进序号也不产生确认块，只有新候选
// 实际引用的排队交易才转为等待投票。
func TestRegisterCandidateInterleavedSequences(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 5)
	sc := buildSeqScene(t, n, keys)

	// 候选接在最新确认块之后：轮次 2、高度 2、前块为第 1 轮确认块。
	wantID := BlockID(2, 2, sc.block1.ID, sc.list)
	if wantID == sc.local.BlockID || wantID == sc.alt1ID {
		t.Fatal("setup: new candidate id must differ from both existing candidates")
	}
	res, err := n.RegisterCandidate(2, sc.list)
	if err != nil {
		t.Fatalf("interleaved candidate with per-account consecutive sequences must register: %v", err)
	}
	if res.Existing {
		t.Fatal("the new list must register as a fresh candidate, not an existing one")
	}
	if res.BlockID != wantID {
		t.Fatalf("block id = %s, want %s (round 2, height 2, prev %s, caller order)",
			res.BlockID, wantID, sc.block1.ID)
	}

	// 登记不推进轮次与确认高度，也不产生确认块。
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 (register must not confirm)", n.CurrentRound(), n.Height())
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 2: %v, want %s", err, ReasonUnknownBlock)
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != sc.block1.ID {
		t.Fatalf("latest block changed: %+v ok=%v, want %s", latest, ok, sc.block1.ID)
	}

	// 本地提议保持原来的标识与顺序。
	p, ok := n.Proposal()
	if !ok || p.BlockID != sc.local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(sc.local.TxIDs) {
		t.Fatalf("local proposal changed: %+v ok=%v, want %+v", p, ok, sc.local)
	}

	// 按轮次查询：新候选保留调用者给出的完整交易顺序，不按账户、序号或
	// 费用重排；本地提议与既有候选及其票数不变。
	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || !rc.HasRecords || len(rc.Candidates) != 3 {
		t.Fatalf("round 2 must have exactly three pending candidates: %+v", rc)
	}
	var gotLocal, gotAlt, gotNew *CandidateView
	for i := range rc.Candidates {
		switch rc.Candidates[i].BlockID {
		case sc.local.BlockID:
			gotLocal = &rc.Candidates[i]
		case sc.alt1ID:
			gotAlt = &rc.Candidates[i]
		case wantID:
			gotNew = &rc.Candidates[i]
		}
	}
	if gotLocal == nil || gotAlt == nil || gotNew == nil {
		t.Fatalf("all three candidates must be present: %+v", rc.Candidates)
	}
	if gotNew.Local || gotNew.Result != CandidatePending || len(gotNew.Voters) != 0 {
		t.Fatalf("new candidate must be a non-local pending candidate with no votes: %+v", gotNew)
	}
	if fmt.Sprint(gotNew.TxIDs) != fmt.Sprint(sc.list) {
		t.Fatalf("candidate tx order = %v, want the exact caller order %v", gotNew.TxIDs, sc.list)
	}
	if !gotLocal.Local || fmt.Sprint(gotLocal.TxIDs) != fmt.Sprint(sc.local.TxIDs) || len(gotLocal.Voters) != 0 {
		t.Fatalf("local candidate changed: %+v", gotLocal)
	}
	if len(gotAlt.Voters) != 1 || fmt.Sprintf("%x", gotAlt.Voters[0]) != fmt.Sprintf("%x", keys[0].pub) {
		t.Fatalf("existing competitor lost its vote: %+v", gotAlt)
	}

	// 登记不推进任何账户的已确认序号；新候选实际引用的排队交易 b3 才转为
	// 等待投票，已有候选引用的交易（含 alt1 独有的 c2）继续保持等待投票。
	assertAccountQueue(t, n, keys[0].pub, 2, map[string]string{
		sc.a3.ID(): "waiting-vote", sc.a4.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 1, map[string]string{
		sc.b2.ID(): "waiting-vote", sc.b3.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{
		sc.c1.ID(): "waiting-vote", sc.c2.ID(): "waiting-vote",
	})
	if info := n.mustTx(t, sc.b3.ID()); info.Status != StatusProposed {
		t.Fatalf("newly referenced b3 status = %s, want proposed", info.Status)
	}
	if info := n.mustTx(t, sc.c2.ID()); info.Status != StatusProposed {
		t.Fatalf("c2 referenced by the earlier candidate must stay proposed, got %s", info.Status)
	}
}

// TestRegisterCandidateSequenceGapRejections 钉住拒绝路径：前序交易在池中
// （甚至已被本地提议引用）但候选跳过它，或同一账户的连续序号在列表中倒序
// 出现，都必须以 sequence-not-consecutive 拒绝；这些列表满足其他登记条件，
// 拒绝后候选集合、本地提议、既有候选票数与两类交易的状态全部保持原样。
func TestRegisterCandidateSequenceGapRejections(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 5)
	sc := buildSeqScene(t, n, keys)

	// 额外的排队交易：a5 的前序 a4 已被本地提议引用，b4 的前序 b3 仅在池中排队。
	a5 := NewTransaction(keys[0].priv, 5, []byte("a5"), 5, 100)
	b4 := NewTransaction(keys[1].priv, 4, []byte("b4"), 5, 100)
	for _, tx := range []*Transaction{a5, b4} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}

	cases := []struct {
		name string
		list []string
	}{
		// 前序交易 a3 在池中且已被本地提议引用，候选却从更后的序号 4 开始。
		{"start beyond needed predecessor", []string{sc.a4.ID()}},
		// 前序交易 a4 在池中且已被本地提议引用，候选中途跳过它。
		{"skip proposed predecessor mid-list", []string{sc.a3.ID(), a5.ID()}},
		// 前序交易 b3 仍在池中排队，候选中途跳过它。
		{"skip queued predecessor mid-list", []string{sc.b2.ID(), b4.ID()}},
		// A 的连续序号都在列表里，但 4 先于 3 出现；B、C 夹在中间不能掩盖倒序。
		{"reversed order within one account", []string{sc.b2.ID(), sc.a4.ID(), sc.c1.ID(), sc.a3.ID()}},
	}
	for _, tc := range cases {
		// 列表均不超单块上限、无未知或重复交易、全部在池中：
		// 拒绝原因必须是序号不连续，而不是其他登记条件。
		if _, err := n.RegisterCandidate(2, tc.list); reason(err) != ReasonSequenceGap {
			t.Fatalf("%s: got %v, want %s", tc.name, err, ReasonSequenceGap)
		}
	}

	// 全部拒绝后：候选集合、本地提议与既有候选票数保持原样。
	assertSeqRoundStable(t, n, sc, keys)

	// 原本只在排队的交易仍显示等待打包，不因失败登记被锁定；
	// 原本等待投票的交易也不能被解锁；已确认序号不推进。
	assertAccountQueue(t, n, keys[0].pub, 2, map[string]string{
		sc.a3.ID(): "waiting-vote", sc.a4.ID(): "waiting-vote", a5.ID(): "waiting-pack",
	})
	assertAccountQueue(t, n, keys[1].pub, 1, map[string]string{
		sc.b2.ID(): "waiting-vote", sc.b3.ID(): "waiting-pack", b4.ID(): "waiting-pack",
	})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{
		sc.c1.ID(): "waiting-vote", sc.c2.ID(): "waiting-vote",
	})
	for _, tx := range []*Transaction{sc.b3, a5, b4} {
		if info := n.mustTx(t, tx.ID()); info.Status != StatusQueued {
			t.Fatalf("queued-only tx %q status = %s, want queued after rejected registers", tx.Content, info.Status)
		}
	}
	for _, tx := range []*Transaction{sc.a3, sc.a4, sc.b2, sc.c1, sc.c2} {
		if info := n.mustTx(t, tx.ID()); info.Status != StatusProposed {
			t.Fatalf("referenced tx %q status = %s, want proposed after rejected registers", tx.Content, info.Status)
		}
	}

	// 排队交易 b3 没有被失败登记锁定：同发送者同序号、费用严格更高仍可替换。
	b3hi := NewTransaction(keys[1].priv, 3, []byte("b3-hi"), 99, 100)
	rep, err := n.Submit(b3hi)
	if err != nil {
		t.Fatalf("queued-only b3 must stay replaceable after rejected registers: %v", err)
	}
	if rep.ReplacedID != sc.b3.ID() {
		t.Fatalf("replacement result = %+v, want ReplacedID %s", rep, sc.b3.ID())
	}
	if info := n.mustTx(t, sc.b3.ID()); info.Status != StatusReplaced || info.ReplacedBy != b3hi.ID() {
		t.Fatalf("b3 after replacement = %+v, want replaced by %s", info, b3hi.ID())
	}
	// 已被候选引用的 a3 继续锁定：加费替换仍以 tx-in-proposal 拒绝。
	a3hi := NewTransaction(keys[0].priv, 3, []byte("a3-hi"), 99, 100)
	if _, err := n.Submit(a3hi); reason(err) != ReasonProposalLocked {
		t.Fatalf("referenced a3 must stay locked, got %v, want %s", err, ReasonProposalLocked)
	}
}
