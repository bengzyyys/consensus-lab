package consensus

import (
	"fmt"
	"testing"
)

// 本文件为“主动结束未确认轮次后恢复加费替换”这一现有功能补充回归保障。
//
// 现有功能在两个操作各自的边界上已有保障：交易进入任一未决候选（本地提议或
// 竞争候选）后，同发送者同序号的替换被 tx-in-proposal 禁止；主动结束轮次后，
// 未到期交易重新排队。本文件钉住这两个操作衔接之后的行为：已经落选的旧候选
// 不能继续阻止交易被替换。
//
// 场景：四名验证者（确认门槛为票数严格超过 2/3，即 3 票）。第 1 轮已有本地
// 提议与一个竞争候选，各候选均未达到确认门槛（各一票）。待替换交易签名有效、
// 到期轮次（100）晚于结束后进入的新轮次（2），序号 1 是其账户下一条可确认
// 的序号。覆盖两种候选归属：
//   - 共享场景：待替换交易同时被本地提议与竞争候选引用；
//   - 竞争独有场景：待替换交易只被竞争候选引用，本地提议不含它。
//
// 钉住的衔接行为：
//   - 轮次未结束时提交费用更高的同发送者同序号交易，一律以 tx-in-proposal
//     拒绝，被拒交易不可查询，原交易与两个候选的交易列表、票数保持原样；
//   - EndRound 进入新轮后、尚未产生提议或登记候选时，费用不严格更高（等于或
//     低于原交易）的同序号有效交易仍以 fee-not-higher 拒绝且不留记录，原交易
//     继续排队、替代关联为空——解除候选限制并不放宽费用要求；
//   - 上述拒绝不妨碍随后费用严格更高的合法替换：同一笔在旧轮被拒的加费交易
//     在新轮按普通加费替换接受，提交结果同时给出新交易标识与被替换的旧标识，
//     被挤出标识为空；
//   - 旧交易查为 replaced，替代关联指向新交易并保留原内容与签名；新交易为
//     queued；账户待处理列表在该序号上只保留新交易、说明 waiting-pack；已确认
//     序号与确认高度不因结束轮次或替换而推进；
//   - 旧轮仍可查询为未确认结束：候选全部落选，原有投票与交易顺序保留，列表
//     继续引用旧交易标识，不随替换改成新标识；
//   - 新轮本地提议若打包该账户这一序号，只能使用新交易；旧候选里的历史引用
//     不能让新交易提前显示等待投票。
//
// 本组测试沿用现有 Go 库入口（Submit/Propose/RegisterCandidate/Vote/EndRound
// 与各查询）、现有交易状态与拒绝原因，不新增也不改变任何公开行为。

// replaceUnlockScene 是“结束轮次解锁加费替换”场景的全部句柄。
type replaceUnlockScene struct {
	keys []testKey
	dir  string
	n    *Node
	// target 待替换交易：keys[0] seq1 fee5 expiry100，是其账户下一条可确认
	// 的序号，到期轮次远晚于结束后进入的新轮次。
	target *Transaction
	// other 另一账户（keys[1]）的seq1 交易，用于控制本地提议是否引用 target。
	other *Transaction
	// bump 同发送者同序号、费用 30 的加费交易：先在旧轮被 tx-in-proposal
	// 拒绝，EndRound 后同一笔再被接受为合法替换。
	bump *Transaction
	// local 第 1 轮本地提议。
	local ProposalView
	// altID/altList 第 1 轮竞争候选的区块标识与交易列表（只含 target）。
	altID   string
	altList []string
	// shared 为 true 时 target 同时被本地提议与竞争候选引用；
	// 为 false 时只被竞争候选引用。
	shared bool
}

// buildReplaceUnlockScene 构造第 1 轮未决场景：本地提议与竞争候选各一票、
// 均未达确认门槛。
//
// 共享场景（shared=true，单块上限 2）：target(费5) 与 other(费3) 都被本地
// 提议打包，竞争候选只含 target，同一笔交易被两个候选共同引用。
//
// 竞争独有场景（shared=false，单块上限 1）：本地提议只打包费用更高的
// other(费20)，target(费5) 留在池内，只能经竞争候选进入未决状态。
func buildReplaceUnlockScene(t *testing.T, shared bool) *replaceUnlockScene {
	t.Helper()
	keys := genKeys(t, 4)
	maxTxs := uint64(2)
	otherFee := uint64(3)
	if !shared {
		maxTxs = 1
		otherFee = 20
	}
	n, dir := newTestNode(t, keys, maxTxs)

	s := &replaceUnlockScene{
		keys:   keys,
		dir:    dir,
		n:      n,
		target: NewTransaction(keys[0].priv, 1, []byte("unlock-target"), 5, 100),
		other:  NewTransaction(keys[1].priv, 1, []byte("unlock-other"), otherFee, 100),
		bump:   NewTransaction(keys[0].priv, 1, []byte("unlock-bump"), 30, 100),
		shared: shared,
	}
	if s.bump.ID() == s.target.ID() {
		t.Fatal("setup: replacement tx must have a distinct id")
	}
	for _, tx := range []*Transaction{s.target, s.other} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit %q: %v", tx.Content, err)
		}
	}

	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 共享场景按费用打包 target(5)、other(3) 两笔；竞争独有场景上限 1，
	// 只打包费用更高的 other(20)，target 不在本地提议中。
	wantLocal := []string{s.target.ID(), s.other.ID()}
	if !shared {
		wantLocal = []string{s.other.ID()}
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("setup: local proposal = %v, want %v", local.TxIDs, wantLocal)
	}

	// 竞争候选只含 target：与本地提议列表不同，必然是不同的候选。
	s.altList = []string{s.target.ID()}
	alt, err := n.RegisterCandidate(1, s.altList)
	if err != nil {
		t.Fatal(err)
	}
	if alt.Existing || alt.BlockID == local.BlockID {
		t.Fatalf("setup: competitor must be a fresh candidate distinct from local: %+v", alt)
	}

	// 验证者0 投本地提议、验证者1 投竞争候选，各一票；四验证者门槛为 3 票，
	// 两个候选都未决出。
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("setup: local vote: %+v %v", res, err)
	}
	if res, err := n.Vote(keys[1].pub, 1, alt.BlockID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("setup: competitor vote: %+v %v", res, err)
	}

	s.local = local
	s.altID = alt.BlockID
	return s
}

// assertPendingRoundOne 断言第 1 轮仍处于完整未决状态（结束前、以及
// tx-in-proposal 拒绝之后同样成立）：轮次未前进、确认高度为 0；两个候选
// 均 pending、各自的交易顺序与那一票原样；target 与 other 都被未决候选
// 引用而等待投票；两个账户的已确认序号都为 0。
func (s *replaceUnlockScene) assertPendingRoundOne(t *testing.T, n *Node) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	p, ok := n.Proposal()
	if !ok || p.BlockID != s.local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(s.local.TxIDs) {
		t.Fatalf("local proposal changed: %+v ok=%v, want id=%s txs=%v", p, ok, s.local.BlockID, s.local.TxIDs)
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 1 must stay pending with records: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(rc.Candidates))
	}
	wantUnvoted := [][]byte{s.keys[2].pub, s.keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}
	for _, c := range rc.Candidates {
		switch c.BlockID {
		case s.local.BlockID:
			if !c.Local || c.Result != CandidatePending ||
				fmt.Sprint(c.TxIDs) != fmt.Sprint(s.local.TxIDs) ||
				len(c.Voters) != 1 || fmt.Sprintf("%x", c.Voters[0]) != fmt.Sprintf("%x", s.keys[0].pub) {
				t.Fatalf("local candidate must stay pending with its order and single vote: %+v", c)
			}
		case s.altID:
			if c.Local || c.Result != CandidatePending ||
				fmt.Sprint(c.TxIDs) != fmt.Sprint(s.altList) ||
				len(c.Voters) != 1 || fmt.Sprintf("%x", c.Voters[0]) != fmt.Sprintf("%x", s.keys[1].pub) {
				t.Fatalf("competitor must stay pending with its order and single vote: %+v", c)
			}
		default:
			t.Fatalf("unexpected candidate %s", c.BlockID)
		}
	}

	// 两笔交易都被未决候选引用：等待投票，且禁止替换正由此而来。
	for _, tx := range []*Transaction{s.target, s.other} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusProposed {
			t.Fatalf("tx %q status = %s, want proposed while round 1 is pending", tx.Content, info.Status)
		}
	}
	assertAccountQueue(t, n, s.keys[0].pub, 0, map[string]string{s.target.ID(): "waiting-vote"})
	assertAccountQueue(t, n, s.keys[1].pub, 0, map[string]string{s.other.ID(): "waiting-vote"})
}

// assertEndedRoundOne 断言第 1 轮在结束后（以及替换完成之后）的历史记录：
// 未确认结束、两个候选全部落选、原有投票与交易顺序保留；候选列表继续引用
// 旧交易标识，绝不随替换改成新标识。
func (s *replaceUnlockScene) assertEndedRoundOne(t *testing.T, n *Node) {
	t.Helper()
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || !rc.UnconfirmedEnd || !rc.HasRecords {
		t.Fatalf("round 1 must be an unconfirmed ended round with records: %+v", rc)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(rc.Candidates))
	}
	wantUnvoted := [][]byte{s.keys[2].pub, s.keys[3].pub}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(hexKeys(wantUnvoted)) {
		t.Fatalf("unvoted validators = %x, want %x", rc.Unvoted, wantUnvoted)
	}
	for _, c := range rc.Candidates {
		if c.Result != CandidateLost {
			t.Fatalf("candidate %s result = %s, want lost", c.BlockID, c.Result)
		}
		var wantVoter []byte
		var wantOrder []string
		switch c.BlockID {
		case s.local.BlockID:
			if !c.Local {
				t.Fatalf("local candidate flag must be retained: %+v", c)
			}
			wantVoter = s.keys[0].pub
			wantOrder = s.local.TxIDs
		case s.altID:
			wantVoter = s.keys[1].pub
			wantOrder = s.altList
		default:
			t.Fatalf("unexpected candidate %s", c.BlockID)
		}
		if len(c.Voters) != 1 || fmt.Sprintf("%x", c.Voters[0]) != fmt.Sprintf("%x", wantVoter) {
			t.Fatalf("candidate %s original vote not retained: %x", c.BlockID, c.Voters)
		}
		if fmt.Sprint(c.TxIDs) != fmt.Sprint(wantOrder) {
			t.Fatalf("candidate %s tx order changed: %v, want %v", c.BlockID, c.TxIDs, wantOrder)
		}
		// 历史列表继续引用旧交易标识：凡原本引用了 target 的候选，替换后仍必须
		// 引用旧标识；替换后的新标识绝不能出现在已结束的旧轮记录里。
		wantOld := false
		for _, id := range wantOrder {
			if id == s.target.ID() {
				wantOld = true
			}
		}
		foundOld := false
		for _, id := range c.TxIDs {
			if id == s.target.ID() {
				foundOld = true
			}
			if id == s.bump.ID() {
				t.Fatalf("ended round 1 candidate %s must not reference the replacement tx %s", c.BlockID, s.bump.ID())
			}
		}
		if foundOld != wantOld {
			t.Fatalf("candidate %s old-tx reference = %v, want %v (list %v)", c.BlockID, foundOld, wantOld, c.TxIDs)
		}
	}
}

// assertNoProgress 断言确认高度与两个账户的已确认序号都没有被结束轮次或
// 替换推进。
func (s *replaceUnlockScene) assertNoProgress(t *testing.T, n *Node) {
	t.Helper()
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0 (ending a round and replacing confirm nothing)", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may exist")
	}
	for _, k := range s.keys[:2] {
		if a := n.Account(k.pub); a.ConfirmedSequence != 0 {
			t.Fatalf("account %x confirmed seq = %d, want 0", k.pub, a.ConfirmedSequence)
		}
	}
}

// runReplaceUnlockScenario 是两种候选归属共用的完整回归序列。
func runReplaceUnlockScenario(t *testing.T, shared bool) {
	s := buildReplaceUnlockScene(t, shared)
	s.assertPendingRoundOne(t, s.n)

	// —— 轮次尚未结束：费用更高的同发送者同序号交易被 tx-in-proposal 拒绝 ——
	res, err := s.n.Submit(s.bump)
	if reason(err) != ReasonProposalLocked {
		t.Fatalf("in-round replacement reason = %v, want %s", err, ReasonProposalLocked)
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}
	// 被拒交易不可查询、不留记录。
	if _, err := s.n.Tx(s.bump.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected bump lookup = %v, want %s", err, ReasonUnknownTx)
	}
	// 原交易与候选交易列表、票数保持原样。
	s.assertPendingRoundOne(t, s.n)

	// —— 用户主动结束该轮：进入第 2 轮，未到期交易重新排队 ——
	nextRound, err := s.n.EndRound()
	if err != nil {
		t.Fatalf("EndRound: %v", err)
	}
	if nextRound != 2 || s.n.CurrentRound() != 2 {
		t.Fatalf("round after end = %d (current %d), want 2", nextRound, s.n.CurrentRound())
	}
	s.assertEndedRoundOne(t, s.n)
	s.assertNoProgress(t, s.n)
	// 新轮尚未产生提议或登记候选。
	if p, ok := s.n.Proposal(); ok {
		t.Fatalf("round 2 must not inherit a proposal: %+v", p)
	}
	if _, err := s.n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("Candidates(2) before any proposal = %v, want %s", err, ReasonUnknownRound)
	}
	// 两笔交易回到排队、等待打包；旧交易替代关联为空。
	for _, tx := range []*Transaction{s.target, s.other} {
		info := s.n.mustTx(t, tx.ID())
		if info.Status != StatusQueued || info.ReplacedBy != "" {
			t.Fatalf("tx %q after end = status %s replacedBy %s, want queued with no link",
				tx.Content, info.Status, info.ReplacedBy)
		}
	}
	assertAccountQueue(t, s.n, s.keys[0].pub, 0, map[string]string{s.target.ID(): "waiting-pack"})
	assertAccountQueue(t, s.n, s.keys[1].pub, 0, map[string]string{s.other.ID(): "waiting-pack"})

	// —— 解除候选限制并不放宽费用要求：等于或低于原交易的同序号交易仍被拒绝 ——
	entriesBefore := len(s.n.st.Entries)
	equalFee := NewTransaction(s.keys[0].priv, 1, []byte("unlock-equal"), 5, 100)
	if equalFee.ID() == s.target.ID() {
		t.Fatal("setup: equal-fee tx must have a distinct id")
	}
	if res, err := s.n.Submit(equalFee); reason(err) != ReasonLowFee || res != nil {
		t.Fatalf("equal-fee resubmit = %+v, %v, want %s with nil result", res, err, ReasonLowFee)
	}
	if _, err := s.n.Tx(equalFee.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("equal-fee tx lookup = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}
	lowerFee := NewTransaction(s.keys[0].priv, 1, []byte("unlock-lower"), 2, 100)
	if res, err := s.n.Submit(lowerFee); reason(err) != ReasonLowFee || res != nil {
		t.Fatalf("lower-fee resubmit = %+v, %v, want %s with nil result", res, err, ReasonLowFee)
	}
	if _, err := s.n.Tx(lowerFee.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("lower-fee tx lookup = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}
	// 拒绝不留记录：原交易继续排队、替代关联为空。
	if got := len(s.n.st.Entries); got != entriesBefore {
		t.Fatalf("entries changed %d -> %d after fee rejections", entriesBefore, got)
	}
	targetInfo := s.n.mustTx(t, s.target.ID())
	if targetInfo.Status != StatusQueued || targetInfo.ReplacedBy != "" {
		t.Fatalf("target after fee rejections = %+v, want queued with empty replaced-by", targetInfo)
	}

	// —— 费用严格更高的合法替换不受上述拒绝妨碍：旧轮被拒的同一笔加费交易
	// 在新轮按普通加费替换接受 ——
	res, err = s.n.Submit(s.bump)
	if err != nil {
		t.Fatalf("higher-fee replacement after EndRound must succeed: %v", err)
	}
	if res.TxID != s.bump.ID() || res.ReplacedID != s.target.ID() || res.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want accept %s replacing %s with empty evicted id",
			res, s.bump.ID(), s.target.ID())
	}

	// 旧交易：replaced，替代关联指向新交易，原内容与签名保留，不带确认块信息。
	targetInfo = s.n.mustTx(t, s.target.ID())
	if targetInfo.Status != StatusReplaced || targetInfo.ReplacedBy != s.bump.ID() {
		t.Fatalf("old tx = status %s replacedBy %s, want replaced/%s",
			targetInfo.Status, targetInfo.ReplacedBy, s.bump.ID())
	}
	if targetInfo.BlockHeight != 0 || targetInfo.BlockID != "" {
		t.Fatalf("replaced history must not carry block info: %+v", targetInfo)
	}
	assertTxMatches(t, targetInfo.Tx, s.target)

	// 新交易：queued，不带任何关联；旧候选里的历史引用不能让它提前显示
	// 等待投票——账户待处理列表在该序号上只保留新交易，说明 waiting-pack。
	bumpInfo := s.n.mustTx(t, s.bump.ID())
	if bumpInfo.Status != StatusQueued || bumpInfo.ReplacedBy != "" ||
		bumpInfo.BlockHeight != 0 || bumpInfo.BlockID != "" {
		t.Fatalf("new tx = %+v, want queued with no links", bumpInfo)
	}
	assertTxMatches(t, bumpInfo.Tx, s.bump)
	assertAccountQueue(t, s.n, s.keys[0].pub, 0, map[string]string{s.bump.ID(): "waiting-pack"})
	assertAccountQueue(t, s.n, s.keys[1].pub, 0, map[string]string{s.other.ID(): "waiting-pack"})
	if a := s.n.Account(s.keys[0].pub); a.Gap != 0 {
		t.Fatalf("account 0 gap = %d, want 0", a.Gap)
	}
	// 结束轮次与替换都不推进已确认序号与确认高度。
	s.assertNoProgress(t, s.n)
	// 旧轮记录继续引用旧交易标识，不随替换改成新标识。
	s.assertEndedRoundOne(t, s.n)

	// —— 新轮本地提议若打包该账户这一序号，只能使用新交易 ——
	// bump(费30) 高于 other，必然入选；共享场景上限 2 时 other 一同打包。
	p2, err := s.n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{s.bump.ID(), s.other.ID()}
	if !shared {
		wantOrder = []string{s.bump.ID()}
	}
	if fmt.Sprint(p2.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("round 2 proposal = %v, want %v (only the replacement tx may fill that sequence)", p2.TxIDs, wantOrder)
	}
	for _, id := range p2.TxIDs {
		if id == s.target.ID() {
			t.Fatalf("round 2 proposal must not pack the replaced old tx %s", s.target.ID())
		}
	}
	bumpInfo = s.n.mustTx(t, s.bump.ID())
	if bumpInfo.Status != StatusProposed {
		t.Fatalf("new tx status = %s, want proposed after round 2 proposal", bumpInfo.Status)
	}
	assertAccountQueue(t, s.n, s.keys[0].pub, 0, map[string]string{s.bump.ID(): "waiting-vote"})
	// 旧交易的 replaced 记录与替代关联保持原样。
	targetInfo = s.n.mustTx(t, s.target.ID())
	if targetInfo.Status != StatusReplaced || targetInfo.ReplacedBy != s.bump.ID() {
		t.Fatalf("old tx after round 2 proposal = %+v, want replaced/%s", targetInfo, s.bump.ID())
	}
	s.assertNoProgress(t, s.n)

	// 持久化：重开后旧轮落选记录、替换关联与新交易状态完全一致。
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CurrentRound() != 2 {
		t.Fatalf("reopened round = %d, want 2", reopened.CurrentRound())
	}
	s.assertEndedRoundOne(t, reopened)
	targetInfo = reopened.mustTx(t, s.target.ID())
	if targetInfo.Status != StatusReplaced || targetInfo.ReplacedBy != s.bump.ID() {
		t.Fatalf("reopened old tx = %+v, want replaced/%s", targetInfo, s.bump.ID())
	}
	assertTxMatches(t, targetInfo.Tx, s.target)
	bumpInfo = reopened.mustTx(t, s.bump.ID())
	if bumpInfo.Status != StatusProposed {
		t.Fatalf("reopened new tx status = %s, want proposed", bumpInfo.Status)
	}
	assertAccountQueue(t, reopened, s.keys[0].pub, 0, map[string]string{s.bump.ID(): "waiting-vote"})
	s.assertNoProgress(t, reopened)
}

// TestEndRoundUnlocksReplacementSharedByBothCandidates 覆盖待替换交易被本地
// 提议与竞争候选共同引用的场景：轮次未结束时加费替换被 tx-in-proposal 冻结，
// 主动结束轮次后同一笔加费交易按普通规则完成替换，旧轮历史记录不受改写。
func TestEndRoundUnlocksReplacementSharedByBothCandidates(t *testing.T) {
	runReplaceUnlockScenario(t, true)
}

// TestEndRoundUnlocksReplacementCompetitorOnly 覆盖待替换交易只被竞争候选
// 引用（本地提议不含它）的场景：落选竞争候选同样不能继续阻止替换，
// 衔接行为与共享场景完全一致。
func TestEndRoundUnlocksReplacementCompetitorOnly(t *testing.T) {
	runReplaceUnlockScenario(t, false)
}
