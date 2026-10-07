package consensus

import (
	"fmt"
	"testing"
)

// 本文件为“主动结束未确认轮次后恢复加费替换”的现有功能补充回归保障。
//
// 现有功能：交易进入任一未决候选（本地提议或竞争候选）后，同发送者同序号的
// 替换被 tx-in-proposal 禁止；用户主动 EndRound 结束未确认轮次后，候选交易
// 回到排队。本文件钉住这两个操作衔接后的行为：已经落选的旧候选不能继续阻止
// 交易被替换。
//
// 场景：第 1 轮已有本地提议与竞争候选，两名验证者各投一票、均未达到确认门槛
// （4 名验证者需严格超过 2/3，即 3 票）。待替换交易签名有效、到期轮次（100）
// 晚于结束后进入的新轮次（2）、序号是其账户下一条可确认的序号（已确认序号为
// 0，序号为 1）。两种布局分别覆盖：
//   - shared=true：待替换交易被本地提议与竞争候选共同引用（两候选列表顺序不同）；
//   - shared=false：单块上限 1，本地提议只打包另一账户费用更高的交易，待替换
//     交易仅被竞争候选引用。
//
// 钉住的行为：
//   - 轮次未结束：同发送者同序号、费用更高的另一笔交易被 tx-in-proposal 拒绝，
//     被拒交易不可查询，原交易与候选交易列表保持原样；
//   - EndRound 进入新轮次后、新轮尚未产生提议或登记候选时：同一笔加费交易按
//     普通加费替换接受，提交结果给出新交易标识与被替换的旧标识、不带被挤出
//     标识；旧交易可查为 replaced、替代关联指向新交易且原内容与签名保留，
//     新交易 queued；账户待处理在该序号上只保留新交易（waiting-pack），已确认
//     序号与确认高度不因结束轮次或替换而推进；
//   - 旧轮仍可查询为未确认结束：候选全部落选，原有投票与交易顺序保留，列表
//     继续引用旧交易标识，不随替换改成新标识；随后本地提议若打包该账户这一
//     序号只能使用新交易，旧候选的历史引用也不能让新交易提前显示等待投票；
//   - 解除候选限制并不放宽费用要求：回池后提交内容不同、费用等于或低于原交易
//     的同序号有效交易仍以 fee-not-higher 拒绝且不留记录，原交易继续排队、
//     替代关联为空；这样的拒绝不妨碍之后费用严格更高的合法替换。
//
// 本文件只补足回归保障，沿用现有 Go 库入口、交易状态与拒绝原因，不新增也不
// 改变任何公开行为。

// endRoundReplaceScene 是“结束轮次后恢复加费替换”场景的全部句柄。
type endRoundReplaceScene struct {
	keys []testKey
	dir  string
	n    *Node
	// sender 是待替换交易的发送账户（非验证者）；other 是另一发送账户，
	// 其交易使本地提议与竞争候选的列表不同（相同列表会返回同一候选）。
	sender testKey
	other  testKey
	// oldTx 待替换交易：序号 1、费用 5、到期轮次 100（晚于结束后的新轮次 2）。
	oldTx *Transaction
	// otherTx 另一账户的交易：shared 布局费用 3（低于 oldTx，随 oldTx 一起进
	// 本地提议）；alt-only 布局费用 7（高于 oldTx，独占单块上限为 1 的本地提议）。
	otherTx *Transaction
	// highTx 加费替换交易：同发送者同序号、内容不同、费用 9 严格更高。
	highTx *Transaction
	// localID/altID 为第 1 轮本地提议与竞争候选的区块标识；
	// localOrder/altOrder 为各自的交易顺序（引用的是 oldTx 的标识）。
	localID    string
	altID      string
	localOrder []string
	altOrder   []string
}

// buildEndRoundReplaceScene 构造第 1 轮未决场景：本地提议与竞争候选均已登记，
// 两名验证者各投一票、轮次未决出。shared 决定待替换交易的候选归属。
func buildEndRoundReplaceScene(t *testing.T, shared bool) *endRoundReplaceScene {
	t.Helper()
	keys := genKeys(t, 5)
	dir := t.TempDir()
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = keys[i].pub
	}
	// shared：单块上限 4，本地提议按费用打包 oldTx(5)、otherTx(3) 两笔，
	// 竞争候选以不同顺序引用同一笔 oldTx；alt-only：单块上限 1，本地提议
	// 只打包费用更高的 otherTx(7)，oldTx(5) 留在池内、仅被竞争候选引用。
	maxTxs := uint64(1)
	otherFee := uint64(7)
	if shared {
		maxTxs = 4
		otherFee = 3
	}
	n, err := New(dir, Config{
		Seed:           []byte("end-round-replace-seed"),
		Validators:     vals,
		MaxTxsPerBlock: maxTxs,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &endRoundReplaceScene{
		keys:   keys,
		dir:    dir,
		n:      n,
		sender: keys[4],
		other:  keys[3],
	}
	s.oldTx = NewTransaction(s.sender.priv, 1, []byte("replace-me"), 5, 100)
	s.otherTx = NewTransaction(s.other.priv, 1, []byte("other-account"), otherFee, 100)
	s.highTx = NewTransaction(s.sender.priv, 1, []byte("higher-fee"), 9, 100)
	for _, tx := range []*Transaction{s.oldTx, s.otherTx} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit %q: %v", tx.Content, err)
		}
	}

	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	s.localID = local.BlockID
	if shared {
		s.localOrder = []string{s.oldTx.ID(), s.otherTx.ID()}
		s.altOrder = []string{s.otherTx.ID(), s.oldTx.ID()}
	} else {
		s.localOrder = []string{s.otherTx.ID()}
		s.altOrder = []string{s.oldTx.ID()}
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(s.localOrder) {
		t.Fatalf("setup: local proposal = %v, want %v", local.TxIDs, s.localOrder)
	}
	alt, err := n.RegisterCandidate(1, s.altOrder)
	if err != nil {
		t.Fatal(err)
	}
	if alt.Existing {
		t.Fatal("setup: competing candidate must be newly registered")
	}
	if alt.BlockID == s.localID {
		t.Fatal("setup: competing candidate block id must differ from the local proposal")
	}
	s.altID = alt.BlockID

	// 验证者0 投本地提议、验证者1 投竞争候选，各 1 票，均未达到 3 票门槛。
	if res, err := n.Vote(keys[0].pub, 1, s.localID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("setup: local proposal vote: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[1].pub, 1, s.altID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("setup: competitor vote: %+v %v", res, err)
	}
	return s
}

// assertRoundOneCandidates 钉住第 1 轮两个候选的记录：结果符合当前阶段
// （未决 pending / 结束后 lost）、原有投票与未投票名单保留、交易顺序原样，
// 且列表继续引用旧交易标识、绝不随替换改成新标识。
func (s *endRoundReplaceScene) assertRoundOneCandidates(t *testing.T, n *Node, wantEnded bool, wantResult string) {
	t.Helper()
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended != wantEnded || rc.UnconfirmedEnd != wantEnded || !rc.HasRecords {
		t.Fatalf("round 1 view = %+v, want ended/unconfirmed-end=%v with records", rc, wantEnded)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("round 1 candidate count = %d, want 2", len(rc.Candidates))
	}
	wantUnvoted := [][]byte{s.keys[2].pub, s.keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("round 1 unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}
	for _, c := range rc.Candidates {
		var wantOrder []string
		var wantVoter []byte
		var wantLocal bool
		switch c.BlockID {
		case s.localID:
			wantOrder, wantVoter, wantLocal = s.localOrder, s.keys[0].pub, true
		case s.altID:
			wantOrder, wantVoter = s.altOrder, s.keys[1].pub
		default:
			t.Fatalf("unexpected candidate %s in round 1", c.BlockID)
		}
		if c.Result != wantResult || c.Local != wantLocal || fmt.Sprint(c.TxIDs) != fmt.Sprint(wantOrder) {
			t.Fatalf("candidate %s = result %s local=%v txs %v, want %s local=%v txs %v",
				c.BlockID, c.Result, c.Local, c.TxIDs, wantResult, wantLocal, wantOrder)
		}
		if len(c.Voters) != 1 || fmt.Sprintf("%x", c.Voters[0]) != fmt.Sprintf("%x", wantVoter) {
			t.Fatalf("candidate %s voters = %x, want the single original vote %x", c.BlockID, c.Voters, wantVoter)
		}
		// 替换只改变池内“哪笔交易算数”，不改写旧轮候选的历史引用。
		for _, id := range c.TxIDs {
			if id == s.highTx.ID() {
				t.Fatalf("candidate %s must keep referencing the old tx, not the replacement %s", c.BlockID, id)
			}
		}
	}
}

// assertPendingLocked 断言轮次未结束时替换被禁止：同发送者同序号的加费交易
// 被 tx-in-proposal 拒绝，被拒交易不可查询，原交易与候选交易列表保持原样。
func (s *endRoundReplaceScene) assertPendingLocked(t *testing.T, n *Node) {
	t.Helper()
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round/height = %d/%d, want 1/0 before the end", n.CurrentRound(), n.Height())
	}
	// 费用更高的另一笔同发送者同序号交易：仍被 tx-in-proposal 拒绝。
	if _, err := n.Submit(s.highTx); reason(err) != ReasonProposalLocked {
		t.Fatalf("replace while a candidate is pending = %v, want %s", err, ReasonProposalLocked)
	}
	// 被拒交易不留任何记录。
	if _, err := n.Tx(s.highTx.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected tx lookup = %v, want %s", err, ReasonUnknownTx)
	}
	// 原交易与候选交易列表保持原样：两候选仍 pending，原交易仍等待投票。
	s.assertRoundOneCandidates(t, n, false, CandidatePending)
	old := n.mustTx(t, s.oldTx.ID())
	if old.Status != StatusProposed || old.ReplacedBy != "" {
		t.Fatalf("old tx after rejection = status %s replacedBy %q, want proposed with no replacement",
			old.Status, old.ReplacedBy)
	}
	assertAccountQueue(t, n, s.sender.pub, 0, map[string]string{s.oldTx.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.other.pub, 0, map[string]string{s.otherTx.ID(): "waiting-vote"})
	p, ok := n.Proposal()
	if !ok || p.BlockID != s.localID || fmt.Sprint(p.TxIDs) != fmt.Sprint(s.localOrder) {
		t.Fatalf("local proposal changed after rejection: %+v ok=%v", p, ok)
	}
}

// assertRepooled 断言 EndRound 进入第 2 轮后、替换发生前的状态：候选交易回到
// 排队（waiting-pack），旧轮为未确认结束记录，新轮尚未产生提议，确认高度与
// 账户已确认序号不因结束轮次而推进。
func (s *endRoundReplaceScene) assertRepooled(t *testing.T, n *Node) {
	t.Helper()
	if n.CurrentRound() != 2 || n.Height() != 0 {
		t.Fatalf("round/height after end = %d/%d, want 2/0 (ending must not confirm anything)",
			n.CurrentRound(), n.Height())
	}
	s.assertRoundOneCandidates(t, n, true, CandidateLost)
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 must not inherit the round 1 proposal: %+v", p)
	}
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("Candidates(2) before any proposal = %v, want %s", err, ReasonUnknownRound)
	}
	old := n.mustTx(t, s.oldTx.ID())
	if old.Status != StatusQueued || old.ReplacedBy != "" {
		t.Fatalf("old tx after end = status %s replacedBy %q, want queued with no replacement",
			old.Status, old.ReplacedBy)
	}
	assertAccountQueue(t, n, s.sender.pub, 0, map[string]string{s.oldTx.ID(): "waiting-pack"})
	assertAccountQueue(t, n, s.other.pub, 0, map[string]string{s.otherTx.ID(): "waiting-pack"})
}

// assertReplaced 断言加费替换成功后的完整状态：旧交易 replaced 且替代关联指向
// 新交易、原内容与签名保留；新交易 queued、说明 waiting-pack（旧候选的历史引用
// 不能让它提前显示等待投票）；账户待处理在该序号上只保留新交易；已确认序号与
// 确认高度不推进；旧轮记录仍引用旧标识；新轮尚未产生提议。
func (s *endRoundReplaceScene) assertReplaced(t *testing.T, n *Node) {
	t.Helper()
	if n.CurrentRound() != 2 || n.Height() != 0 {
		t.Fatalf("round/height after replacement = %d/%d, want 2/0: replacement must not advance the chain",
			n.CurrentRound(), n.Height())
	}

	// 旧交易：replaced，替代关联指向新交易，不带确认块或挤出信息，
	// 原内容、费用、到期轮次与签名完整保留。
	old := n.mustTx(t, s.oldTx.ID())
	if old.Status != StatusReplaced || old.ReplacedBy != s.highTx.ID() {
		t.Fatalf("old tx = status %s replacedBy %q, want replaced/%s", old.Status, old.ReplacedBy, s.highTx.ID())
	}
	if old.BlockHeight != 0 || old.BlockID != "" || old.DropReason != "" || old.DropRound != 0 {
		t.Fatalf("replaced tx must carry no block/drop associations: %+v", old)
	}
	if old.Tx == nil || string(old.Tx.Content) != string(s.oldTx.Content) ||
		old.Tx.Sequence != 1 || old.Tx.Fee != 5 || old.Tx.Expiry != 100 ||
		fmt.Sprintf("%x", old.Tx.Sender) != fmt.Sprintf("%x", s.oldTx.Sender) ||
		fmt.Sprintf("%x", old.Tx.Signature) != fmt.Sprintf("%x", s.oldTx.Signature) {
		t.Fatalf("replaced tx content not preserved in full: %+v", old.Tx)
	}

	// 新交易：queued；旧候选里的历史引用不能让它提前显示等待投票。
	hi := n.mustTx(t, s.highTx.ID())
	if hi.Status != StatusQueued || hi.ReplacedBy != "" {
		t.Fatalf("new tx = status %s replacedBy %q, want queued with no replacement", hi.Status, hi.ReplacedBy)
	}

	// 账户待处理在该序号上只保留新交易，说明 waiting-pack；已确认序号不推进。
	assertAccountQueue(t, n, s.sender.pub, 0, map[string]string{s.highTx.ID(): "waiting-pack"})
	if a := n.Account(s.sender.pub); a.Gap != 0 {
		t.Fatalf("sender gap = %d, want 0", a.Gap)
	}
	assertAccountQueue(t, n, s.other.pub, 0, map[string]string{s.otherTx.ID(): "waiting-pack"})

	// 旧轮记录不变：未确认结束、全部落选，列表继续引用旧交易标识。
	s.assertRoundOneCandidates(t, n, true, CandidateLost)

	// 新轮仍未产生提议或登记候选。
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 must not inherit the round 1 proposal: %+v", p)
	}
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("Candidates(2) before any proposal = %v, want %s", err, ReasonUnknownRound)
	}
}

// runEndRoundReplaceScenario 驱动完整回归序列：未决轮次替换被拒 -> 主动结束 ->
// 等费/低费仍被拒 -> 加费替换成功 -> 持久化 -> 新轮提议只使用新交易。
func runEndRoundReplaceScenario(t *testing.T, shared bool) {
	t.Helper()
	s := buildEndRoundReplaceScene(t, shared)
	s.assertPendingLocked(t, s.n)

	// 用户主动结束未确认轮次：候选交易回到排队，进入第 2 轮。
	next, err := s.n.EndRound()
	if err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	if next != 2 {
		t.Fatalf("EndRound return = %d, want 2", next)
	}
	s.assertRepooled(t, s.n)

	// 解除候选限制并不放宽费用要求：内容不同、费用等于或低于原交易的同序号
	// 有效交易仍以 fee-not-higher 拒绝且不留记录。
	for _, x := range []struct {
		content string
		fee     uint64
	}{
		{"equal-fee", 5},
		{"lower-fee", 2},
	} {
		bad := NewTransaction(s.sender.priv, 1, []byte(x.content), x.fee, 100)
		if _, err := s.n.Submit(bad); reason(err) != ReasonLowFee {
			t.Fatalf("replacement with fee %d = %v, want %s", x.fee, err, ReasonLowFee)
		}
		if _, err := s.n.Tx(bad.ID()); reason(err) != ReasonUnknownTx {
			t.Fatalf("rejected fee %d tx must leave no record: %v, want %s", x.fee, err, ReasonUnknownTx)
		}
	}
	// 拒绝不留痕迹：原交易继续排队、替代关联为空，轮次与候选记录不变。
	s.assertRepooled(t, s.n)

	// 费用严格更高的合法替换不受此前拒绝影响：在新轮尚未产生提议或登记候选时
	// 按普通加费替换接受，结果同时给出新标识与被替换的旧标识，不带被挤出标识。
	res, err := s.n.Submit(s.highTx)
	if err != nil {
		t.Fatalf("higher-fee replacement after EndRound: %v", err)
	}
	if res.TxID != s.highTx.ID() || res.ReplacedID != s.oldTx.ID() || res.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want TxID=%s ReplacedID=%s EvictedID empty",
			res, s.highTx.ID(), s.oldTx.ID())
	}
	s.assertReplaced(t, s.n)

	// 替换结果持久化：重开节点看到完全一致的状态。
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.assertReplaced(t, reopened)

	// 新轮本地提议若打包该账户这一序号，只能使用新交易，绝不能使用已替换的旧交易。
	p, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	foundNew, foundOld := false, false
	for _, id := range p.TxIDs {
		if id == s.highTx.ID() {
			foundNew = true
		}
		if id == s.oldTx.ID() {
			foundOld = true
		}
	}
	if !foundNew || foundOld {
		t.Fatalf("round 2 proposal = %v, must pack the replacement %s and never the replaced %s",
			p.TxIDs, s.highTx.ID(), s.oldTx.ID())
	}
	// 进入新提议后新交易才转为等待投票；旧交易的 replaced 记录与替代关联原样。
	assertAccountQueue(t, reopened, s.sender.pub, 0, map[string]string{s.highTx.ID(): "waiting-vote"})
	old := reopened.mustTx(t, s.oldTx.ID())
	if old.Status != StatusReplaced || old.ReplacedBy != s.highTx.ID() {
		t.Fatalf("old tx after new proposal = status %s replacedBy %q, want replaced/%s",
			old.Status, old.ReplacedBy, s.highTx.ID())
	}
	// 旧轮记录仍引用旧标识，不受新提议影响。
	s.assertRoundOneCandidates(t, reopened, true, CandidateLost)
}

// TestEndRoundReenablesFeeReplacementSharedCandidate 覆盖待替换交易被本地提议
// 与竞争候选共同引用的布局：落选候选（含本地提议）都不能继续阻止加费替换。
func TestEndRoundReenablesFeeReplacementSharedCandidate(t *testing.T) {
	runEndRoundReplaceScenario(t, true)
}

// TestEndRoundReenablesFeeReplacementAltOnlyCandidate 覆盖待替换交易只被竞争
// 候选引用的布局：本地提议从未包含它，竞争候选落选后同样恢复加费替换。
func TestEndRoundReenablesFeeReplacementAltOnlyCandidate(t *testing.T) {
	runEndRoundReplaceScenario(t, false)
}
