package consensus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 候选登记的各类失败原因，失败时节点状态不变。
func TestRegisterCandidateValidation(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 尚未产生本地提议。
	if _, err := n.RegisterCandidate(1, []string{a1.ID()}); reason(err) != ReasonNoProposal {
		t.Fatalf("got %v, want %s", err, ReasonNoProposal)
	}

	p, _ := n.Propose() // 上限 2：本地提议按费用选出 a1(10)、b1(5)，a2 因缺口留在池中。

	// 非当前轮次（轮次优先于其他校验）。
	if _, err := n.RegisterCandidate(2, []string{a1.ID()}); reason(err) != ReasonWrongRound {
		t.Fatalf("got %v, want %s", err, ReasonWrongRound)
	}
	// 未知交易。
	if _, err := n.RegisterCandidate(1, []string{"deadbeef"}); reason(err) != ReasonUnknownTx {
		t.Fatalf("got %v, want %s", err, ReasonUnknownTx)
	}
	// 列表重复。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), a1.ID()}); reason(err) != ReasonDuplicateInList {
		t.Fatalf("got %v, want %s", err, ReasonDuplicateInList)
	}
	// 超过单块上限。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), a2.ID(), b1.ID()}); reason(err) != ReasonTooManyTxs {
		t.Fatalf("got %v, want %s", err, ReasonTooManyTxs)
	}
	// 序号不连续：a2 之前缺少该账户序号 1。
	if _, err := n.RegisterCandidate(1, []string{a2.ID()}); reason(err) != ReasonSequenceGap {
		t.Fatalf("got %v, want %s", err, ReasonSequenceGap)
	}
	// 序号不连续：跳号。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), a2.ID(), b1.ID()}); reason(err) != ReasonTooManyTxs {
		t.Fatalf("limit check must precede sequence check, got %v", err)
	}

	// 所有失败登记后状态不变：仍只有本地提议一个候选，引用之外的交易仍排队。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 || rc.Candidates[0].BlockID != p.BlockID || !rc.Candidates[0].Local {
		t.Fatalf("failed registrations changed candidates: %+v", rc.Candidates)
	}
	if info, _ := n.Tx(a2.ID()); info.Status != StatusQueued || info.Note != "" {
		t.Fatalf("a2 should stay queued, got %+v", info)
	}

	// 合法登记：账户间顺序由调用者决定，账户内序号连续即可。
	if _, err := n.RegisterCandidate(1, []string{a1.ID(), b1.ID()}); err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}
	// 允许空块候选。
	if _, err := n.RegisterCandidate(1, nil); err != nil {
		t.Fatalf("empty candidate rejected: %v", err)
	}
}

// 已退出池的交易（被替换）不能进入候选。
func TestRegisterCandidateRejectsReplacedTx(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	p, _ := n.Propose() // a1 进入本地提议
	// 另一个账户的排队交易
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 1, 100)
	n.Submit(b1)

	// 先确认本地提议，进入第 2 轮。
	confirmByVotes(t, n, keys, p.BlockID)

	// 第 2 轮：提交 c1，再用更高费替换；旧交易退出池。
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 1, 100)
	n.Submit(c1)
	c1hi := NewTransaction(keys[2].priv, 1, []byte("c1hi"), 9, 100)
	n.Submit(c1hi)

	n.Propose()
	if _, err := n.RegisterCandidate(2, []string{c1.ID()}); reason(err) != ReasonTxNotInPool {
		t.Fatalf("replaced tx got %v, want %s", err, ReasonTxNotInPool)
	}
	// 已确认交易同样退出池。
	if _, err := n.RegisterCandidate(2, []string{a1.ID()}); reason(err) != ReasonTxNotInPool {
		t.Fatalf("confirmed tx got %v, want %s", err, ReasonTxNotInPool)
	}
}

// 相同交易列表重复登记返回同一候选并保留已有票数；登记其他候选不动本地提议。
func TestRegisterCandidateIdempotentAndLocalStable(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	n.Submit(a1)
	n.Submit(b1)
	p1, _ := n.Propose() // 打包顺序：b1 先、a1 后。

	list := []string{a1.ID(), b1.ID()} // 顺序与本地提议相反
	r1, err := n.RegisterCandidate(1, list)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Existing || r1.BlockID == p1.BlockID {
		t.Fatalf("new candidate wrong: %+v local=%s", r1, p1.BlockID)
	}
	// 预期区块标识可用同一编码复算。
	wantID := BlockID(1, 1, "", list)
	if r1.BlockID != wantID {
		t.Fatalf("block id %s != recomputed %s", r1.BlockID, wantID)
	}

	// 投一票给竞争候选，再用相同列表重复登记。
	if _, err := n.Vote(keys[0].pub, 1, r1.BlockID); err != nil {
		t.Fatal(err)
	}
	r2, err := n.RegisterCandidate(1, append([]string{}, list...))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Existing || r2.BlockID != r1.BlockID {
		t.Fatalf("same list should return same candidate: %+v vs %+v", r1, r2)
	}
	rc, _ := n.Candidates(1)
	var alt *CandidateView
	for i := range rc.Candidates {
		if rc.Candidates[i].BlockID == r1.BlockID {
			alt = &rc.Candidates[i]
		}
	}
	if alt == nil || len(alt.Voters) != 1 {
		t.Fatalf("votes not preserved on duplicate register: %+v", alt)
	}

	// Propose/Proposal 始终返回不变的本地提议。
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	p3, ok := n.Proposal()
	if !ok || p2.BlockID != p1.BlockID || p3.BlockID != p1.BlockID {
		t.Fatalf("local proposal changed: %s %s %s", p1.BlockID, p2.BlockID, p3.BlockID)
	}
	if fmt.Sprint(p3.TxIDs) != fmt.Sprint(p1.TxIDs) {
		t.Fatal("local proposal tx order changed")
	}
}

// 被任一未决候选引用的交易都显示等待投票，且禁止被更高费用交易替换。
func TestCandidateTxsLocked(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 1)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 1, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 上限 1、费用更高：本地提议只含 a1。
	if len(local.TxIDs) != 1 || local.TxIDs[0] != a1.ID() {
		t.Fatalf("setup: local should only contain a1, got %v", local.TxIDs)
	}

	// b1 仅在竞争候选中出现。
	alt, err := n.RegisterCandidate(1, []string{b1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := n.Tx(b1.ID())
	if info.Status != StatusProposed {
		t.Fatalf("alt-candidate tx status = %s, want proposed", info.Status)
	}
	acct := n.Account(keys[1].pub)
	if len(acct.Pending) != 1 || acct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("alt-candidate tx note = %+v, want waiting-vote", acct.Pending)
	}
	replace := NewTransaction(keys[1].priv, 1, []byte("b1hi"), 99, 100)
	if _, err := n.Submit(replace); reason(err) != ReasonProposalLocked {
		t.Fatalf("replacing alt-candidate tx got %v, want %s", err, ReasonProposalLocked)
	}

	// 仅在池中等待（未被任何候选引用）的交易仍可替换。
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 1, 100)
	n.Submit(c1)
	c1hi := NewTransaction(keys[2].priv, 1, []byte("c1hi"), 2, 100)
	if _, err := n.Submit(c1hi); err != nil {
		t.Fatalf("queued-only tx should remain replaceable: %v", err)
	}
	_ = alt
}

// 多候选投票：每人单选、重复不增票、改投拒绝且原票保留；非本地候选也能胜出确认。
func TestMultiCandidateVoting(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 打包顺序：b1(5) 先、a1(1) 后。
	// 竞争候选：与本地提议相反的顺序。
	list := []string{a1.ID(), b1.ID()}
	alt, err := n.RegisterCandidate(1, list)
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID || fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{b1.ID(), a1.ID()}) {
		t.Fatalf("setup wrong: local=%v alt=%s", local.TxIDs, alt.BlockID)
	}

	// 未知区块标识拒绝。
	if _, err := n.Vote(keys[0].pub, 1, "deadbeef"); reason(err) != ReasonWrongBlock {
		t.Fatalf("got %v, want %s", err, ReasonWrongBlock)
	}

	// keys[0] 投本地；重复不增票；改投明确拒绝且原票保留。
	if res, err := n.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted {
		t.Fatalf("first vote: %+v %v", res, err)
	}
	if res, _ := n.Vote(keys[0].pub, 1, local.BlockID); res.Counted {
		t.Fatal("duplicate vote must not count")
	}
	if _, err := n.Vote(keys[0].pub, 1, alt.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch vote got %v, want %s", err, ReasonAlreadyVoted)
	}
	rc, _ := n.Candidates(1)
	for _, c := range rc.Candidates {
		if c.BlockID == local.BlockID && len(c.Voters) != 1 {
			t.Fatalf("original vote lost after rejected switch: %d voters", len(c.Voters))
		}
		if c.BlockID == alt.BlockID && len(c.Voters) != 0 {
			t.Fatal("rejected switch must not add vote")
		}
	}

	// 其余三人投竞争候选，第三票使其 >2/3 立即确认。
	r1, err := n.Vote(keys[1].pub, 1, alt.BlockID)
	if err != nil || r1.Confirmed {
		t.Fatalf("alt vote 1: %+v %v", r1, err)
	}
	r2, _ := n.Vote(keys[2].pub, 1, alt.BlockID)
	if r2.Confirmed {
		t.Fatal("2 votes for alt plus locked local vote: alt has 2, must not confirm yet")
	}
	r3, err := n.Vote(keys[3].pub, 1, alt.BlockID)
	if err != nil || !r3.Confirmed || r3.Block == nil {
		t.Fatalf("alt should confirm on third vote: %+v %v", r3, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	// 确认块是竞争候选的标识与顺序。
	blk, _ := n.BlockAt(1)
	if blk.ID != alt.BlockID || fmt.Sprint(blk.TxIDs) != fmt.Sprint(list) {
		t.Fatalf("confirmed block wrong: %+v", blk)
	}
	if blk.PreviousID != "" {
		t.Fatalf("genesis prev = %q, want empty", blk.PreviousID)
	}

	// 结果记录：本地候选落选，竞争候选胜出。
	rc, _ = n.Candidates(1)
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("round record wrong: %+v", rc)
	}
	for _, c := range rc.Candidates {
		want := CandidateLost
		if c.BlockID == alt.BlockID {
			want = CandidateWon
		}
		if c.Result != want {
			t.Fatalf("candidate %s result = %s, want %s", c.BlockID, c.Result, want)
		}
	}

	// 胜出交易均确认一次，账户序号按实际确认结果推进。
	for _, id := range list {
		info, _ := n.Tx(id)
		if info.Status != StatusConfirmed || info.BlockID != alt.BlockID || info.BlockHeight != 1 {
			t.Fatalf("winner tx %s wrong: %+v", id, info)
		}
	}
	if n.Account(keys[0].pub).ConfirmedSequence != 1 || n.Account(keys[1].pub).ConfirmedSequence != 1 {
		t.Fatal("confirmed sequences not advanced")
	}
	// 确认后旧轮投票一律按轮次拒绝。
	if _, err := n.Vote(keys[0].pub, 1, local.BlockID); reason(err) != ReasonWrongRound {
		t.Fatalf("stale vote got %v, want wrong-round", err)
	}
}

// 落选候选独有的交易回到排队；共享交易确认一次、落选候选不能改其状态；
// 进入新轮次时已到期的交易显示过期。
func TestLoserTransactionsReturnAndExpire(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	shared := NewTransaction(keys[0].priv, 1, []byte("shared"), 1, 100)
	loserTx := NewTransaction(keys[1].priv, 1, []byte("loser-only"), 1, 100)
	n.Submit(shared)
	n.Submit(loserTx)
	// 上限 10：本地提议同时包含两笔（顺序按打包规则，不影响本测试结论）。
	local, _ := n.Propose()
	if len(local.TxIDs) != 2 {
		t.Fatalf("local proposal should pack both txs, got %v", local.TxIDs)
	}
	// 竞争候选只含 shared：它将胜出，loserTx 仅被落选的本地候选引用。
	alt, err := n.RegisterCandidate(1, []string{shared.ID()})
	if err != nil {
		t.Fatal(err)
	}
	confirmCandidate(t, n, keys, alt.BlockID)

	// 共享交易只确认一次，关联胜出块；落选本地候选不能改变它。
	info, _ := n.Tx(shared.ID())
	if info.Status != StatusConfirmed || info.BlockID != alt.BlockID || info.BlockHeight != 1 {
		t.Fatalf("shared tx wrong: %+v", info)
	}
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 1 {
		t.Fatalf("winner account seq = %d, want 1", a.ConfirmedSequence)
	}
	// 落选候选独有的交易回到排队，账户序号不因落选推进。
	loser, _ := n.Tx(loserTx.ID())
	if loser.Status != StatusQueued {
		t.Fatalf("loser-only tx status = %s, want queued", loser.Status)
	}
	if a := n.Account(keys[1].pub); a.ConfirmedSequence != 0 || len(a.Pending) != 1 {
		t.Fatalf("loser account wrong: %+v", a)
	}

	// 第 2 轮：再提交一笔进入轮次 3 即到期的交易；不确认直接结束。
	expiring := NewTransaction(keys[3].priv, 1, []byte("expiring"), 1, 3)
	n.Submit(expiring)
	p2, _ := n.Propose()
	if len(p2.TxIDs) != 2 {
		t.Fatalf("round 2 proposal should pack loserTx and expiring, got %v", p2.TxIDs)
	}
	if r, err := n.EndRound(); err != nil || r != 3 {
		t.Fatalf("end round = %d, %v", r, err)
	}
	if info, _ := n.Tx(expiring.ID()); info.Status != StatusExpired {
		t.Fatalf("expiring tx status = %s, want expired", info.Status)
	}
	// 落选交易未到期则回到排队；已确认历史不变。
	if info, _ := n.Tx(loserTx.ID()); info.Status != StatusQueued {
		t.Fatalf("loser tx after EndRound = %s, want queued", info.Status)
	}
	if info, _ := n.Tx(shared.ID()); info.Status != StatusConfirmed {
		t.Fatalf("confirmed tx changed after round end: %s", info.Status)
	}
	if n.Height() != 1 {
		t.Fatal("EndRound must not create a block")
	}
	// 账户缺口与序号按实际确认结果：key1 的序号 1 仍在等待，无缺口。
	if a := n.Account(keys[1].pub); a.Gap != 0 || a.ConfirmedSequence != 0 {
		t.Fatalf("loser account after round end wrong: %+v", a)
	}
}

// EndRound：所有候选落选不留块，交易回池；查询明确显示未确认结束。
func TestEndRoundAllCandidatesLost(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	local, _ := n.Propose()
	alt, err := n.RegisterCandidate(1, []string{}) // 空块竞争候选
	if err != nil {
		t.Fatal(err)
	}
	n.Vote(keys[0].pub, 1, local.BlockID)
	n.Vote(keys[1].pub, 1, alt.BlockID)

	r, err := n.EndRound()
	if err != nil || r != 2 {
		t.Fatalf("EndRound = %d, %v", r, err)
	}
	rc, _ := n.Candidates(1)
	if !rc.Ended || !rc.UnconfirmedEnd {
		t.Fatalf("ended round should be marked unconfirmed-end: %+v", rc)
	}
	for _, c := range rc.Candidates {
		if c.Result != CandidateLost {
			t.Fatalf("candidate %s result = %s, want lost", c.BlockID, c.Result)
		}
	}
	// 旧票不影响新轮次：同一名验证者可在新轮次重新选择。
	p2, _ := n.Propose()
	if res, err := n.Vote(keys[0].pub, 2, p2.BlockID); err != nil || !res.Counted {
		t.Fatalf("vote in new round: %+v %v", res, err)
	}
	if info, _ := n.Tx(a1.ID()); info.Status != StatusProposed {
		t.Fatalf("a1 should be proposed again in round 2: %s", info.Status)
	}
}

// 候选查询排序、未投票名单与防御性拷贝。
func TestCandidatesQuery(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 打包顺序：b1 先、a1 后。
	altID := registerMust(t, n, 1, []string{a1.ID(), b1.ID()})
	emptyID := registerMust(t, n, 1, []string{})
	if altID == local.BlockID {
		t.Fatal("test setup: alt candidate must differ from local")
	}

	// 未知轮次。
	if _, err := n.Candidates(0); reason(err) != ReasonUnknownRound {
		t.Fatalf("round 0 got %v, want %s", err, ReasonUnknownRound)
	}
	if _, err := n.Candidates(9); reason(err) != ReasonUnknownRound {
		t.Fatalf("future round got %v, want %s", err, ReasonUnknownRound)
	}

	n.Vote(keys[3].pub, 1, altID)
	n.Vote(keys[0].pub, 1, local.BlockID)
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	// 候选按区块标识排序。
	var ids []string
	for _, c := range rc.Candidates {
		ids = append(ids, c.BlockID)
	}
	sorted := append([]string{}, ids...)
	sortStrings(sorted)
	if fmt.Sprint(ids) != fmt.Sprint(sorted) {
		t.Fatalf("candidates not sorted: %v", ids)
	}
	// 投票者按公钥排序。
	for _, c := range rc.Candidates {
		prev := ""
		for _, v := range c.Voters {
			h := fmt.Sprintf("%x", v)
			if h < prev {
				t.Fatal("voters not sorted by public key")
			}
			prev = h
		}
	}
	// 未投票验证者：keys[1]、keys[2]，按公钥排序。
	if len(rc.Unvoted) != 2 {
		t.Fatalf("unvoted = %d, want 2", len(rc.Unvoted))
	}
	unvotedSet := map[string]bool{}
	for _, v := range rc.Unvoted {
		unvotedSet[fmt.Sprintf("%x", v)] = true
	}
	if !unvotedSet[fmt.Sprintf("%x", keys[1].pub)] || !unvotedSet[fmt.Sprintf("%x", keys[2].pub)] {
		t.Fatalf("unvoted set wrong: %x", rc.Unvoted)
	}
	if fmt.Sprintf("%x", rc.Unvoted[0]) > fmt.Sprintf("%x", rc.Unvoted[1]) {
		t.Fatal("unvoted validators must be sorted by public key")
	}
	// 未决出时结果为 pending。
	for _, c := range rc.Candidates {
		if c.Result != CandidatePending {
			t.Fatalf("candidate %s result = %s, want pending", c.BlockID, c.Result)
		}
	}

	// 防御性拷贝：调用方修改返回结果不影响节点。
	var altView *CandidateView
	for i := range rc.Candidates {
		if rc.Candidates[i].BlockID == altID {
			altView = &rc.Candidates[i]
		}
	}
	if altView == nil || len(altView.TxIDs) == 0 || len(altView.Voters) == 0 {
		t.Fatalf("alt view missing data: %+v", altView)
	}
	originalFirstTx := altView.TxIDs[0]
	altView.TxIDs[0] = "tampered"
	altView.Voters[0][0] ^= 0xff
	rc.Unvoted[0][0] ^= 0xff
	rc2, _ := n.Candidates(1)
	var alt2 *CandidateView
	for i := range rc2.Candidates {
		if rc2.Candidates[i].BlockID == altID {
			alt2 = &rc2.Candidates[i]
		}
	}
	if alt2.TxIDs[0] != originalFirstTx {
		t.Fatal("mutation of candidate tx ids leaked into node state")
	}
	// 原节点内的投票仍有效（重复投票不计数即说明身份比对未被破坏）。
	if res, err := n.Vote(keys[3].pub, 1, altID); err != nil || res.Counted {
		t.Fatalf("node state corrupted by caller mutation: %+v %v", res, err)
	}
	_ = emptyID
}

// 停止后恢复候选、每人已投的选择与历史结果。
func TestCandidatesPersistence(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("persist-cand"), Validators: vals, MaxTxsPerBlock: 5})
	if err != nil {
		t.Fatal(err)
	}
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 打包顺序：b1 先、a1 后。
	altID := registerMust(t, n, 1, []string{a1.ID(), b1.ID()})
	if altID == local.BlockID {
		t.Fatal("test setup: alt candidate must differ from local")
	}
	n.Vote(keys[0].pub, 1, local.BlockID)
	n.Vote(keys[1].pub, 1, altID)
	n.Vote(keys[2].pub, 1, altID)

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := n2.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidates restored = %d, want 2", len(rc.Candidates))
	}
	byID := map[string]CandidateView{}
	for _, c := range rc.Candidates {
		byID[c.BlockID] = c
	}
	if c := byID[local.BlockID]; !c.Local || len(c.Voters) != 1 {
		t.Fatalf("local candidate restored wrong: %+v", c)
	}
	if c := byID[altID]; c.Local || len(c.Voters) != 2 {
		t.Fatalf("alt candidate restored wrong: %+v", c)
	}
	if p, ok := n2.Proposal(); !ok || p.BlockID != local.BlockID {
		t.Fatalf("local proposal not restored: %+v", p)
	}
	// 每人的选择恢复：重复不计数、改投拒绝。
	if res, _ := n2.Vote(keys[1].pub, 1, altID); res.Counted {
		t.Fatal("restored vote should dedup")
	}
	if _, err := n2.Vote(keys[1].pub, 1, local.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch after reopen got %v, want %s", err, ReasonAlreadyVoted)
	}
	// 再补一票，竞争候选确认。
	res, err := n2.Vote(keys[3].pub, 1, altID)
	if err != nil || !res.Confirmed {
		t.Fatalf("alt should confirm after reopen: %+v %v", res, err)
	}
	// 历史结果保留，新轮无候选记录。
	rc, _ = n2.Candidates(1)
	for _, c := range rc.Candidates {
		want := CandidateWon
		if c.BlockID != altID {
			want = CandidateLost
		}
		if c.Result != want {
			t.Fatalf("history result = %s, want %s", c.Result, want)
		}
	}
	if _, err := n2.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("fresh round should have no records, got %v", err)
	}
}

// 版本 1 的旧状态目录仍可打开：未决提议与票数作为本地候选恢复，
// 过去没有保存的候选详情明确显示无记录，确认块不变。
func TestOpenV1State(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("v1-seed"), Validators: vals, MaxTxsPerBlock: 5})
	if err != nil {
		t.Fatal(err)
	}
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	p, _ := n.Propose()
	n.Vote(keys[0].pub, 1, p.BlockID)

	// 把当前状态改写为版本 1 的文件形态：仅 Proposal 字段，没有 Rounds。
	st := n.st.clone()
	rs := st.Rounds[1]
	local := rs.Candidates[rs.LocalBlockID]
	st.Version = 1
	st.Rounds = nil
	st.Proposal = &proposalState{
		Round:   rs.Round,
		TxIDs:   append([]string{}, local.TxIDs...),
		BlockID: local.BlockID,
		Votes:   append([]string{}, local.Votes...),
	}
	writeRawState(t, dir, st)

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1", n2.CurrentRound())
	}
	p2, ok := n2.Proposal()
	if !ok || p2.BlockID != p.BlockID || fmt.Sprint(p2.TxIDs) != fmt.Sprint(p.TxIDs) {
		t.Fatalf("v1 proposal not restored as local candidate: %+v", p2)
	}
	rc, err := n2.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.HasRecords || len(rc.Candidates) != 1 || !rc.Candidates[0].Local {
		t.Fatalf("v1 pending proposal should restore with details: %+v", rc)
	}
	if len(rc.Candidates[0].Voters) != 1 {
		t.Fatal("v1 votes not restored as local candidate votes")
	}
	if res, _ := n2.Vote(keys[0].pub, 1, p.BlockID); res.Counted {
		t.Fatal("restored v1 vote should dedup")
	}
	res, _ := n2.Vote(keys[1].pub, 1, p.BlockID)
	res, _ = n2.Vote(keys[2].pub, 1, p.BlockID)
	if !res.Confirmed {
		t.Fatal("v1 node should confirm after reopening")
	}
}

// 版本 1 的历史轮次：确认轮与主动结束轮都没有候选详情，确认块本身不变。
func TestOpenV1HistoryRounds(t *testing.T) {
	keys := genKeys(t, 4)
	dir := t.TempDir()
	vals := make([][]byte, len(keys))
	for i, k := range keys {
		vals[i] = k.pub
	}
	n, err := New(dir, Config{Seed: []byte("v1-hist"), Validators: vals, MaxTxsPerBlock: 5})
	if err != nil {
		t.Fatal(err)
	}
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	p1, _ := n.Propose()
	confirmCandidate(t, n, keys, p1.BlockID) // 轮次 1 确认，当前轮次 2
	if _, err := n.EndRound(); err != nil {  // 轮次 2 主动结束，当前轮次 3
		t.Fatal(err)
	}
	block1, _ := n.BlockAt(1)

	// 改写为版本 1：删除 Rounds。
	st := n.st.clone()
	st.Version = 1
	st.Rounds = nil
	writeRawState(t, dir, st)

	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CurrentRound() != 3 || n2.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 3/1", n2.CurrentRound(), n2.Height())
	}
	b, err := n2.BlockAt(1)
	if err != nil || b.ID != block1.ID {
		t.Fatalf("confirmed block not preserved: %+v %v", b, err)
	}
	rc1, err := n2.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc1.HasRecords || !rc1.Ended || rc1.UnconfirmedEnd || len(rc1.Candidates) != 0 {
		t.Fatalf("v1 confirmed round should show no records: %+v", rc1)
	}
	rc2, err := n2.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if rc2.HasRecords || !rc2.Ended || !rc2.UnconfirmedEnd {
		t.Fatalf("v1 ended round should show unconfirmed-end with no records: %+v", rc2)
	}
	// 当前轮次尚未提议，查询按未知轮次处理。
	if _, err := n2.Candidates(3); reason(err) != ReasonUnknownRound {
		t.Fatalf("current round without proposal got %v, want %s", err, ReasonUnknownRound)
	}
	// 迁移后的节点可以继续正常工作。
	p3, err := n2.Propose()
	if err != nil || p3.Round != 3 {
		t.Fatalf("propose after v1 migration: %+v %v", p3, err)
	}
}

// 登记失败、投票失败、结束轮次失败时保存错误必须保留操作前状态。
func TestCandidateOpsAtomicity(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	local, _ := n.Propose()

	n.injectSaveErr = errors.New("disk full (simulated)")
	_, err := n.RegisterCandidate(1, []string{})
	if err == nil {
		t.Fatal("register must fail when save fails")
	}
	rc, _ := n.Candidates(1)
	if len(rc.Candidates) != 1 {
		t.Fatalf("failed register changed state: %d candidates", len(rc.Candidates))
	}
	// 投票触发确认时保存失败：轮次不得前进。
	_, err = n.Vote(keys[0].pub, 1, local.BlockID)
	if err == nil {
		t.Fatal("vote must fail when save fails")
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("failed vote changed state: round=%d height=%d", n.CurrentRound(), n.Height())
	}
	// 结束轮次保存失败：轮次不得前进。
	_, err = n.EndRound()
	if err == nil {
		t.Fatal("end round must fail when save fails")
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("failed EndRound changed round: %d", n.CurrentRound())
	}
	n.injectSaveErr = nil

	// 磁盘文件仍是操作前状态。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ = reopened.Candidates(1)
	if len(rc.Candidates) != 1 {
		t.Fatalf("disk state changed despite failed saves: %d candidates", len(rc.Candidates))
	}
	if res, err := reopened.Vote(keys[0].pub, 1, local.BlockID); err != nil || !res.Counted {
		t.Fatalf("vote after reopen: %+v %v", res, err)
	}
}

func registerMust(t *testing.T, n *Node, round uint64, ids []string) string {
	t.Helper()
	r, err := n.RegisterCandidate(round, ids)
	if err != nil {
		t.Fatalf("register %v: %v", ids, err)
	}
	return r.BlockID
}

func confirmCandidate(t *testing.T, n *Node, keys []testKey, blockID string) {
	t.Helper()
	round := n.CurrentRound()
	count := len(keys)*2/3 + 1
	for i := 0; i < count; i++ {
		res, err := n.Vote(keys[i].pub, round, blockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
		if i == count-1 && !res.Confirmed {
			t.Fatalf("vote %d should confirm %s", i+1, blockID)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func writeRawState(t *testing.T, dir string, st *state) {
	t.Helper()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
