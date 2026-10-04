package consensus

import (
	"fmt"
	"testing"
)

// 本文件为投票确认补充验证者人数变化的回归保障：确认规则是“某候选自己的有效
// 票数严格超过完整验证者名单的三分之二”，分母始终是名单总人数（含未投票者），
// 而非实际参与投票的人数。既有用例以四名验证者为主，这里覆盖 1/3/5/6 人名单。

// candidateView 按区块标识找出某轮候选视图，找不到时测试失败。
func candidateView(t *testing.T, n *Node, round uint64, blockID string) CandidateView {
	t.Helper()
	rc, err := n.Candidates(round)
	if err != nil {
		t.Fatalf("candidates round %d: %v", round, err)
	}
	for _, c := range rc.Candidates {
		if c.BlockID == blockID {
			return c
		}
	}
	t.Fatalf("candidate %s not found in round %d", blockID, round)
	return CandidateView{}
}

// assertPending 全面检查门槛之前的可观察状态：轮次与高度不前进、候选仍未决、
// 候选引用的交易仍处于等待投票状态、未投票名单按完整名单计算。
func assertPendingRound(t *testing.T, n *Node, round uint64, blockID string, wantVoters int, wantUnvoted [][]byte) {
	t.Helper()
	if n.CurrentRound() != round {
		t.Fatalf("current round = %d, want %d (must not advance before quorum)", n.CurrentRound(), round)
	}
	if n.Height() != round-1 {
		t.Fatalf("height = %d, want %d (must not advance before quorum)", n.Height(), round-1)
	}
	if _, ok := n.LatestBlock(); ok != (round > 1) {
		t.Fatalf("latest block presence wrong before quorum in round %d", round)
	}
	rc, err := n.Candidates(round)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended {
		t.Fatalf("round %d must not be ended before quorum", round)
	}
	c := candidateView(t, n, round, blockID)
	if c.Result != CandidatePending {
		t.Fatalf("candidate result = %s, want pending before quorum", c.Result)
	}
	if len(c.Voters) != wantVoters {
		t.Fatalf("candidate voters = %d, want %d", len(c.Voters), wantVoters)
	}
	if len(rc.Unvoted) != len(wantUnvoted) {
		t.Fatalf("unvoted = %d (%x), want %d (%x)", len(rc.Unvoted), rc.Unvoted, len(wantUnvoted), wantUnvoted)
	}
	want := map[string]bool{}
	for _, v := range wantUnvoted {
		want[fmt.Sprintf("%x", v)] = true
	}
	for _, v := range rc.Unvoted {
		if !want[fmt.Sprintf("%x", v)] {
			t.Fatalf("unexpected unvoted validator %x", v)
		}
	}
}

// 三名验证者：门槛为严格超过 3*2/3=2，即需要 3 票。两票必须保持未决，
// 第三票才确认；空块同样需要走完投票，不因没有交易而免于门槛。
func TestQuorumRegressionThreeValidators(t *testing.T) {
	keys := genKeys(t, 3)
	n, _ := newTestNode(t, keys, 10)
	p, err := n.Propose() // 无交易：本地提议是空块
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty {
		t.Fatalf("setup: want empty proposal, got %v", p.TxIDs)
	}

	r1, err := n.Vote(keys[0].pub, 1, p.BlockID)
	if err != nil || !r1.Counted || r1.Confirmed {
		t.Fatalf("vote 1/3: %+v %v", r1, err)
	}
	r2, err := n.Vote(keys[1].pub, 1, p.BlockID)
	if err != nil || !r2.Counted || r2.Confirmed {
		t.Fatalf("2/3 votes must stay pending: %+v %v", r2, err)
	}
	// 门槛之前：轮次 1、高度 0、候选 pending、第三人未投票、高度 1 尚不存在。
	assertPendingRound(t, n, 1, p.BlockID, 2, [][]byte{keys[2].pub})
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block before quorum got %v, want %s", err, ReasonUnknownBlock)
	}

	// 第三票跨过严格超过 2/3 的门槛，立即返回确认块并进入下一轮。
	r3, err := n.Vote(keys[2].pub, 1, p.BlockID)
	if err != nil || !r3.Confirmed || r3.Block == nil {
		t.Fatalf("3/3 votes must confirm: %+v %v", r3, err)
	}
	blk := r3.Block
	if blk.ID != p.BlockID || blk.Height != 1 || blk.Round != 1 || len(blk.TxIDs) != 0 || blk.PreviousID != "" {
		t.Fatalf("confirming block wrong: %+v", blk)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	stored, _ := n.BlockAt(1)
	if stored.ID != p.BlockID || len(stored.TxIDs) != 0 {
		t.Fatalf("stored block wrong: %+v", stored)
	}
	// 旧轮查询：唯一候选胜出且记录实际得票 3。
	rc, _ := n.Candidates(1)
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Candidates) != 1 {
		t.Fatalf("resolved round record wrong: %+v", rc)
	}
	won := rc.Candidates[0]
	if won.Result != CandidateWon || len(won.Voters) != 3 {
		t.Fatalf("winner record wrong: %+v", won)
	}
}

// 六名验证者：门槛为严格超过 6*2/3=4，即需要 5 票。4 票恰好达到三分之二，
// 仍不能确认；第 5 票才确认。胜出候选是非本地的空块，确认块的标识与交易顺序
// 必须来自胜出候选，本地候选引用的交易落选后回到排队。
func TestQuorumRegressionSixValidators(t *testing.T) {
	keys := genKeys(t, 6)
	n, _ := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	if _, err := n.Submit(a1); err != nil {
		t.Fatal(err)
	}
	local, _ := n.Propose() // 本地提议 [a1]
	if len(local.TxIDs) != 1 || local.TxIDs[0] != a1.ID() {
		t.Fatalf("setup local proposal wrong: %v", local.TxIDs)
	}
	// 非本地的空块竞争候选。
	alt, err := n.RegisterCandidate(1, []string{})
	if err != nil || alt.BlockID == local.BlockID {
		t.Fatalf("register alt: %+v %v", alt, err)
	}

	// 前 4 票投给非本地候选：恰好 2/3，票数计入但一律不得表示已确认。
	for i := 0; i < 4; i++ {
		r, err := n.Vote(keys[i].pub, 1, alt.BlockID)
		if err != nil || !r.Counted || r.Confirmed {
			t.Fatalf("vote %d to alt must count but not confirm: %+v %v", i+1, r, err)
		}
	}
	// 门槛之前的完整状态：第 5、6 人未投票；本地候选的交易仍等待投票。
	assertPendingRound(t, n, 1, alt.BlockID, 4, [][]byte{keys[4].pub, keys[5].pub})
	if info, _ := n.Tx(a1.ID()); info.Status != StatusProposed || info.Note != "" {
		t.Fatalf("a1 before quorum: %+v, want proposed/waiting-vote", info)
	}
	if acct := n.Account(keys[0].pub); len(acct.Pending) != 1 || acct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("a1 account before quorum wrong: %+v", acct.Pending)
	}
	if c := candidateView(t, n, 1, local.BlockID); c.Result != CandidatePending {
		t.Fatalf("local candidate must stay pending, got %s", c.Result)
	}

	// 第 5 票才严格超过 2/3，立即确认非本地空块。
	r5, err := n.Vote(keys[4].pub, 1, alt.BlockID)
	if err != nil || !r5.Confirmed || r5.Block == nil {
		t.Fatalf("5/6 votes must confirm: %+v %v", r5, err)
	}
	if r5.Block.ID != alt.BlockID || len(r5.Block.TxIDs) != 0 || r5.Block.Height != 1 {
		t.Fatalf("confirming block must come from alt candidate: %+v", r5.Block)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	// 旧轮结果对应实际得票：非本地候选 5 票胜出，本地候选 0 票落选；
	// 未投票的第 6 人始终计入名单，仍列在未投票名单中。
	rc, _ := n.Candidates(1)
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("resolved round wrong: %+v", rc)
	}
	for _, c := range rc.Candidates {
		switch c.BlockID {
		case alt.BlockID:
			if c.Result != CandidateWon || len(c.Voters) != 5 {
				t.Fatalf("alt result wrong: %+v", c)
			}
		case local.BlockID:
			if c.Result != CandidateLost || len(c.Voters) != 0 {
				t.Fatalf("local result wrong: %+v", c)
			}
		default:
			t.Fatalf("unexpected candidate %s", c.BlockID)
		}
	}
	if len(rc.Unvoted) != 1 || fmt.Sprintf("%x", rc.Unvoted[0]) != fmt.Sprintf("%x", keys[5].pub) {
		t.Fatalf("unvoted after resolution = %x, want keys[5]", rc.Unvoted)
	}
	// 确认块标识与（空）交易顺序来自非本地胜出候选；落选本地候选的交易回到排队。
	stored, _ := n.BlockAt(1)
	if stored.ID != alt.BlockID || len(stored.TxIDs) != 0 {
		t.Fatalf("stored block must be the alt block: %+v", stored)
	}
	if info, _ := n.Tx(a1.ID()); info.Status != StatusQueued {
		t.Fatalf("loser-only tx should return to queue, got %s", info.Status)
	}
}

// 五名验证者：门槛为严格超过 5*2/3=3，即需要 4 票。3 票不足、4 票足够；
// 全程不投票的第五人不会阻止已有 4 票的候选确认（分母是完整名单而非投票人数）。
func TestQuorumRegressionFiveValidators(t *testing.T) {
	keys := genKeys(t, 5)
	n, _ := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	local, _ := n.Propose() // 本地提议 [a1]
	alt, err := n.RegisterCandidate(1, []string{})
	if err != nil || alt.BlockID == local.BlockID {
		t.Fatalf("register alt: %+v %v", alt, err)
	}

	// 3 票投给非本地候选：不足严格超过 2/3（3 不大于 3）。
	for i := 0; i < 3; i++ {
		if r, err := n.Vote(keys[i].pub, 1, alt.BlockID); err != nil || !r.Counted || r.Confirmed {
			t.Fatalf("vote %d to alt must not confirm: %+v %v", i+1, r, err)
		}
	}
	assertPendingRound(t, n, 1, alt.BlockID, 3, [][]byte{keys[3].pub, keys[4].pub})

	// 第 4 票足够：即使 keys[4] 始终不投票，候选也立即确认。
	r4, err := n.Vote(keys[3].pub, 1, alt.BlockID)
	if err != nil || !r4.Confirmed || r4.Block == nil {
		t.Fatalf("4/5 votes must confirm even with one abstention: %+v %v", r4, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	if r4.Block.ID != alt.BlockID || len(r4.Block.TxIDs) != 0 {
		t.Fatalf("confirming block must come from alt: %+v", r4.Block)
	}
	rc, _ := n.Candidates(1)
	for _, c := range rc.Candidates {
		want := CandidateLost
		wantVoters := 0
		if c.BlockID == alt.BlockID {
			want, wantVoters = CandidateWon, 4
		}
		if c.Result != want || len(c.Voters) != wantVoters {
			t.Fatalf("candidate %s = %s/%d voters, want %s/%d", c.BlockID, c.Result, len(c.Voters), want, wantVoters)
		}
	}
	// 从未投票的第五人仍出现在旧轮的未投票名单中。
	if len(rc.Unvoted) != 1 || fmt.Sprintf("%x", rc.Unvoted[0]) != fmt.Sprintf("%x", keys[4].pub) {
		t.Fatalf("unvoted = %x, want keys[4]", rc.Unvoted)
	}
}

// 一名验证者是合法配置：本地提议产生后，该验证者的第一票（1 > 0）就应确认。
func TestQuorumRegressionSingleValidator(t *testing.T) {
	keys := genKeys(t, 1)
	n, _ := newTestNode(t, keys, 1)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	if _, err := n.Submit(a1); err != nil {
		t.Fatal(err)
	}
	// 提议产生之前不能投票。
	if _, err := n.Vote(keys[0].pub, 1, "any"); reason(err) != ReasonNoProposal {
		t.Fatalf("vote before propose got %v, want %s", err, ReasonNoProposal)
	}
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != a1.ID() {
		t.Fatalf("setup proposal wrong: %v", p.TxIDs)
	}
	r, err := n.Vote(keys[0].pub, 1, p.BlockID)
	if err != nil || !r.Counted || !r.Confirmed || r.Block == nil {
		t.Fatalf("sole validator's first vote must confirm: %+v %v", r, err)
	}
	if r.Block.ID != p.BlockID || fmt.Sprint(r.Block.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("confirming block wrong: %+v", r.Block)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	if info, _ := n.Tx(a1.ID()); info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
		t.Fatalf("a1 confirmed state wrong: %+v", info)
	}
}

// 六人名单中的票数归属：一个候选 4 票、另一个候选 1 票时，即使已有 5 人投票，
// 也没有任何候选严格超过 2/3（4 恰好等于 2/3），均不得确认；分母不能按实际
// 参与投票的 5 人计算。最后一人投给已有 4 票的候选后，它才得到足够的 5 票。
// 同时保障一人一轮只能选择一个候选，以及旧轮结果对应实际得票。
func TestQuorumRegressionVoteAttributionSixValidators(t *testing.T) {
	keys := genKeys(t, 6)
	n, _ := newTestNode(t, keys, 1)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 1, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 上限 1、费用更高：本地候选 A=[a1]
	if len(local.TxIDs) != 1 || local.TxIDs[0] != a1.ID() {
		t.Fatalf("setup local proposal wrong: %v", local.TxIDs)
	}
	bID := registerMust(t, n, 1, []string{b1.ID()}) // 竞争候选 B=[b1]

	// A 获得 4 票：恰好达到 2/3，第四票不得确认。
	for i := 0; i < 4; i++ {
		if r, err := n.Vote(keys[i].pub, 1, local.BlockID); err != nil || r.Confirmed {
			t.Fatalf("A vote %d must not confirm: %+v %v", i+1, r, err)
		}
	}
	// keys[4] 改投 B：5 人已投票，但 A 只有 4 票、B 只有 1 票，仍无人确认。
	if r, err := n.Vote(keys[4].pub, 1, bID); err != nil || !r.Counted || r.Confirmed {
		t.Fatalf("B first vote must count but not confirm anyone: %+v %v", r, err)
	}
	assertPendingRound(t, n, 1, local.BlockID, 4, [][]byte{keys[5].pub})
	if c := candidateView(t, n, 1, bID); c.Result != CandidatePending || len(c.Voters) != 1 {
		t.Fatalf("B should be pending with 1 vote: %+v", c)
	}
	// 一人一轮只能选择一个候选：keys[4] 不能改投 A，且 B 的票保留。
	if _, err := n.Vote(keys[4].pub, 1, local.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switch vote got %v, want %s", err, ReasonAlreadyVoted)
	}
	if c := candidateView(t, n, 1, bID); len(c.Voters) != 1 {
		t.Fatalf("B vote must be retained after rejected switch: %d voters", len(c.Voters))
	}

	// 最后一人投给 A：A 达到 5 票，严格超过 2/3，才确认。
	r, err := n.Vote(keys[5].pub, 1, local.BlockID)
	if err != nil || !r.Confirmed || r.Block == nil {
		t.Fatalf("A needs the 6th validator's 5th vote to confirm: %+v %v", r, err)
	}
	if r.Block.ID != local.BlockID || fmt.Sprint(r.Block.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("confirming block must come from A: %+v", r.Block)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	// 旧轮结果对应实际得票：A 5 票胜出，B 1 票落选；无未投票者。
	rc, _ := n.Candidates(1)
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Unvoted) != 0 {
		t.Fatalf("resolved round wrong: %+v", rc)
	}
	for _, c := range rc.Candidates {
		switch c.BlockID {
		case local.BlockID:
			if c.Result != CandidateWon || len(c.Voters) != 5 {
				t.Fatalf("winner A record wrong: %+v", c)
			}
		case bID:
			if c.Result != CandidateLost || len(c.Voters) != 1 {
				t.Fatalf("loser B record wrong: %+v", c)
			}
		}
	}
	// A 的交易确认，B 独有的交易回到排队。
	if info, _ := n.Tx(a1.ID()); info.Status != StatusConfirmed || info.BlockID != local.BlockID {
		t.Fatalf("a1 should confirm in A: %+v", info)
	}
	if info, _ := n.Tx(b1.ID()); info.Status != StatusQueued {
		t.Fatalf("b1 should return to queue, got %s", info.Status)
	}
}

// 门槛之前重复投给原候选不增票：不能借重复请求跨过门槛，候选查询中的投票者
// 也不能重复出现。以三名验证者为例：2 名验证者投票后，重复请求再多也停留在
// 未决，第三名验证者的票才确认。
func TestQuorumRegressionDuplicateVotesCannotCross(t *testing.T) {
	keys := genKeys(t, 3)
	n, _ := newTestNode(t, keys, 10)
	p, _ := n.Propose() // 空块

	if r, _ := n.Vote(keys[0].pub, 1, p.BlockID); !r.Counted || r.Confirmed {
		t.Fatalf("first vote wrong: %+v", r)
	}
	if r, _ := n.Vote(keys[1].pub, 1, p.BlockID); r.Confirmed {
		t.Fatalf("2/3 votes must not confirm: %+v", r)
	}
	// 两名已投票验证者反复重复：均不计数、不确认、不推进轮次与高度。
	for i := 0; i < 3; i++ {
		r0, err := n.Vote(keys[0].pub, 1, p.BlockID)
		if err != nil || r0.Counted || r0.Confirmed {
			t.Fatalf("keys[0] duplicate %d: %+v %v", i, r0, err)
		}
		r1, err := n.Vote(keys[1].pub, 1, p.BlockID)
		if err != nil || r1.Counted || r1.Confirmed {
			t.Fatalf("keys[1] duplicate %d: %+v %v", i, r1, err)
		}
	}
	assertPendingRound(t, n, 1, p.BlockID, 2, [][]byte{keys[2].pub})
	c := candidateView(t, n, 1, p.BlockID)
	seen := map[string]int{}
	for _, v := range c.Voters {
		seen[fmt.Sprintf("%x", v)]++
	}
	if len(c.Voters) != 2 || seen[fmt.Sprintf("%x", keys[0].pub)] != 1 || seen[fmt.Sprintf("%x", keys[1].pub)] != 1 {
		t.Fatalf("voters must appear exactly once: %x", c.Voters)
	}

	// 第三名验证者的有效新票才跨过门槛。
	r, err := n.Vote(keys[2].pub, 1, p.BlockID)
	if err != nil || !r.Confirmed {
		t.Fatalf("third distinct validator must confirm: %+v %v", r, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
}
