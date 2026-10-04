package consensus

import (
	"fmt"
	"testing"
)

// findCandidate 在指定轮次中按区块标识找出候选视图，找不到即失败。
func findCandidate(t *testing.T, n *Node, round uint64, blockID string) CandidateView {
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

// assertRoundStillOpen 断言第 1 轮在达到门槛之前一切照旧：
// 投票已计入但结果仍为未决，轮次与确认高度不前进，候选引用的交易仍等待投票。
// candidateVoters 为该候选当前得票，totalVoted 为本轮已投票的不同验证者人数
// （票数分散到多个候选时二者不同）。
func assertRoundStillOpen(t *testing.T, n *Node, blockID string, totalValidators, candidateVoters, totalVoted int, txID string) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, must stay 1 before quorum", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, must stay 0 before quorum", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may exist before quorum")
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended {
		t.Fatalf("round must not be decided before quorum: %+v", rc)
	}
	c := findCandidate(t, n, 1, blockID)
	if c.Result != CandidatePending {
		t.Fatalf("candidate result = %s, want pending", c.Result)
	}
	if len(c.Voters) != candidateVoters {
		t.Fatalf("candidate voters = %d, want %d", len(c.Voters), candidateVoters)
	}
	if len(rc.Unvoted) != totalValidators-totalVoted {
		t.Fatalf("unvoted = %d, want %d (full validator set always counts)",
			len(rc.Unvoted), totalValidators-totalVoted)
	}
	if txID != "" {
		info, err := n.Tx(txID)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusProposed {
			t.Fatalf("referenced tx status = %s, want proposed while pending", info.Status)
		}
	}
}

// 确认规则是“某候选自己的有效票数严格超过完整验证者名单的三分之二”。
// 逐一名单规模保护确认时机：1 人第一票即确认；3 人两票未决、第三票确认；
// 5 人三票不足、四票足够（未投票的第五人不阻止）；6 人四票恰好三分之二仍不足，
// 第五票才确认。门槛只取决于完整名单人数，与实际参与投票的人数无关。
func TestQuorumAcrossValidatorSetSizes(t *testing.T) {
	cases := []struct {
		name         string
		size         int
		pendingVotes int // 达到门槛前逐票计入但保持未决的票数
	}{
		{"single-validator-first-vote-confirms", 1, 0},
		{"three-validators-need-all-three", 3, 2},
		{"five-validators-four-suffice-with-one-silent", 5, 3},
		{"six-validators-exact-two-thirds-insufficient", 6, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := genKeys(t, tc.size)
			n, _ := newTestNode(t, keys, 10)
			a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
			if _, err := n.Submit(a1); err != nil {
				t.Fatal(err)
			}
			local, _ := n.Propose()
			if len(local.TxIDs) != 1 || local.TxIDs[0] != a1.ID() {
				t.Fatalf("setup: local proposal = %v, want [a1]", local.TxIDs)
			}

			// 门槛之前：每一票都正常计入，但不得表示已确认，轮次/高度/查询结果均不前进。
			for i := 0; i < tc.pendingVotes; i++ {
				res, err := n.Vote(keys[i].pub, 1, local.BlockID)
				if err != nil || !res.Counted {
					t.Fatalf("vote %d should be counted: %+v %v", i+1, res, err)
				}
				if res.Confirmed || res.Block != nil {
					t.Fatalf("vote %d must not confirm (strictly more than 2/3 required): %+v", i+1, res)
				}
				assertRoundStillOpen(t, n, local.BlockID, tc.size, i+1, i+1, a1.ID())
			}

			// 达到门槛的那一票立即返回胜出候选的确认块。
			res, err := n.Vote(keys[tc.pendingVotes].pub, 1, local.BlockID)
			if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
				t.Fatalf("vote %d should confirm immediately: %+v %v", tc.pendingVotes+1, res, err)
			}
			wantVoters := tc.pendingVotes + 1
			if res.Block.Height != 1 || res.Block.Round != 1 || res.Block.ID != local.BlockID {
				t.Fatalf("confirming vote returned wrong block: %+v", res.Block)
			}
			if fmt.Sprint(res.Block.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
				t.Fatalf("confirming block tx order = %v, want [a1]", res.Block.TxIDs)
			}
			if res.Block.PreviousID != "" {
				t.Fatalf("genesis previous id = %q, want empty", res.Block.PreviousID)
			}

			// 立即进入下一轮，确认高度前进；未投票者不阻止确认。
			if n.CurrentRound() != 2 || n.Height() != 1 {
				t.Fatalf("after confirm round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
			}

			// 查询旧轮：候选胜出、得票与实际一致，未投票者仍按完整名单列出。
			rc, err := n.Candidates(1)
			if err != nil {
				t.Fatal(err)
			}
			if !rc.Ended || rc.UnconfirmedEnd {
				t.Fatalf("old round record wrong: %+v", rc)
			}
			c := findCandidate(t, n, 1, local.BlockID)
			if c.Result != CandidateWon {
				t.Fatalf("candidate result = %s, want won", c.Result)
			}
			if len(c.Voters) != wantVoters {
				t.Fatalf("winner voters = %d, want %d", len(c.Voters), wantVoters)
			}
			if len(rc.Unvoted) != tc.size-wantVoters {
				t.Fatalf("unvoted after confirm = %d, want %d", len(rc.Unvoted), tc.size-wantVoters)
			}

			// 确认块标识与交易顺序来自胜出候选，交易已确认在高度 1。
			blk, err := n.BlockAt(1)
			if err != nil {
				t.Fatal(err)
			}
			if blk.ID != local.BlockID || fmt.Sprint(blk.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
				t.Fatalf("stored block wrong: %+v", blk)
			}
			info, _ := n.Tx(a1.ID())
			if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != local.BlockID {
				t.Fatalf("winner tx wrong after confirm: %+v", info)
			}
		})
	}
}

// 空块沿用同一票数规则：没有交易不免除投票，也不改变门槛。
func TestEmptyBlockUsesSameQuorum(t *testing.T) {
	cases := []struct {
		name         string
		size         int
		pendingVotes int
	}{
		{"single-empty-block", 1, 0},
		{"three-validators-empty", 3, 2},
		{"six-validators-empty", 6, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := genKeys(t, tc.size)
			n, _ := newTestNode(t, keys, 10)
			local, _ := n.Propose() // 池中无交易，产生空块。
			if !local.Empty || len(local.TxIDs) != 0 {
				t.Fatalf("setup: expected empty proposal, got %v", local.TxIDs)
			}

			for i := 0; i < tc.pendingVotes; i++ {
				res, err := n.Vote(keys[i].pub, 1, local.BlockID)
				if err != nil || !res.Counted || res.Confirmed {
					t.Fatalf("empty-block vote %d must count without confirming: %+v %v", i+1, res, err)
				}
			}
			res, err := n.Vote(keys[tc.pendingVotes].pub, 1, local.BlockID)
			if err != nil || !res.Confirmed || res.Block == nil {
				t.Fatalf("empty block should confirm on vote %d: %+v %v", tc.pendingVotes+1, res, err)
			}
			if len(res.Block.TxIDs) != 0 || res.Block.ID != local.BlockID {
				t.Fatalf("confirmed empty block wrong: %+v", res.Block)
			}
			if n.CurrentRound() != 2 || n.Height() != 1 {
				t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
			}
			blk, _ := n.BlockAt(1)
			if blk.ID != local.BlockID || len(blk.TxIDs) != 0 {
				t.Fatalf("stored empty block wrong: %+v", blk)
			}
		})
	}
}

// 票数归属与完整名单计数：六人名单中一个候选四票、另一个候选一票，
// 五个人已投票仍不能确认任何候选——未投票者始终计入名单人数，比例不能拿
// 实际参与人数计算。最后一人投给已有四票的候选后，它才得到足够的五票。
// 胜出者不是本地提议时，确认块的标识与交易顺序仍来自胜出候选。
func TestQuorumVoteOwnershipAndNonLocalWinner(t *testing.T) {
	keys := genKeys(t, 6)
	n, _ := newTestNode(t, keys, 10)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 打包按费用：b1 先、a1 后。
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{b1.ID(), a1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [b1 a1]", local.TxIDs)
	}
	// 竞争候选使用相反的交易顺序，将作为非本地候选胜出。
	altList := []string{a1.ID(), b1.ID()}
	altID := registerMust(t, n, 1, altList)
	if altID == local.BlockID {
		t.Fatal("test setup: alt candidate must differ from local")
	}

	// 四人投竞争候选：4 票恰好等于六人的三分之二，仍不足。
	for i := 0; i < 4; i++ {
		res, err := n.Vote(keys[i].pub, 1, altID)
		if err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("alt vote %d must count without confirming: %+v %v", i+1, res, err)
		}
	}
	// 第五人投本地候选：两个候选分别为 4 票与 1 票，均不能确认。
	res, err := n.Vote(keys[4].pub, 1, local.BlockID)
	if err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("local vote must count without confirming: %+v %v", res, err)
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, split 4/1 votes must not decide", n.CurrentRound(), n.Height())
	}
	altView := findCandidate(t, n, 1, altID)
	localView := findCandidate(t, n, 1, local.BlockID)
	if altView.Result != CandidatePending || len(altView.Voters) != 4 {
		t.Fatalf("alt candidate wrong before quorum: %+v", altView)
	}
	if localView.Result != CandidatePending || len(localView.Voters) != 1 {
		t.Fatalf("local candidate wrong before quorum: %+v", localView)
	}
	rc, _ := n.Candidates(1)
	if len(rc.Unvoted) != 1 || fmt.Sprintf("%x", rc.Unvoted[0]) != fmt.Sprintf("%x", keys[5].pub) {
		t.Fatalf("only keys[5] should be unvoted: %x", rc.Unvoted)
	}
	// 两笔交易仍处于等待投票状态。
	for _, id := range []string{a1.ID(), b1.ID()} {
		if info, _ := n.Tx(id); info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed", id, info.Status)
		}
	}

	// 重复投原候选不增票，不能借重复请求跨过门槛；改投仍被一人一轮一票规则拒绝。
	dup, err := n.Vote(keys[0].pub, 1, altID)
	if err != nil || dup.Counted || dup.Confirmed {
		t.Fatalf("duplicate vote must not count or confirm: %+v %v", dup, err)
	}
	if _, err := n.Vote(keys[0].pub, 1, local.BlockID); reason(err) != ReasonAlreadyVoted {
		t.Fatalf("switching candidate got %v, want %s", err, ReasonAlreadyVoted)
	}
	assertRoundStillOpen(t, n, altID, 6, 4, 5, a1.ID())

	// 最后一名未投票者投给已有四票的候选：第五票严格超过三分之二，立即确认。
	res, err = n.Vote(keys[5].pub, 1, altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("fifth distinct alt vote should confirm: %+v %v", res, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	// 确认块标识与交易顺序来自非本地的胜出候选。
	if res.Block.ID != altID || fmt.Sprint(res.Block.TxIDs) != fmt.Sprint(altList) {
		t.Fatalf("confirming block must come from non-local winner: %+v", res.Block)
	}
	blk, _ := n.BlockAt(1)
	if blk.ID != altID || fmt.Sprint(blk.TxIDs) != fmt.Sprint(altList) || blk.PreviousID != "" {
		t.Fatalf("stored block must match non-local winner: %+v", blk)
	}

	// 查询旧轮：胜出者与落选者的结果和实际得票一一对应，投票者不重复。
	rc, _ = n.Candidates(1)
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Unvoted) != 0 {
		t.Fatalf("decided round record wrong: %+v", rc)
	}
	altView = findCandidate(t, n, 1, altID)
	localView = findCandidate(t, n, 1, local.BlockID)
	if altView.Result != CandidateWon || len(altView.Voters) != 5 {
		t.Fatalf("winner view wrong: %+v", altView)
	}
	wantAltVoters := map[string]bool{}
	for _, i := range []int{0, 1, 2, 3, 5} {
		wantAltVoters[fmt.Sprintf("%x", keys[i].pub)] = true
	}
	gotAltVoters := map[string]bool{}
	for _, v := range altView.Voters {
		k := fmt.Sprintf("%x", v)
		if gotAltVoters[k] {
			t.Fatalf("voter %s appears more than once", k)
		}
		gotAltVoters[k] = true
	}
	if fmt.Sprint(gotAltVoters) != fmt.Sprint(wantAltVoters) {
		t.Fatalf("winner voters = %v, want %v", gotAltVoters, wantAltVoters)
	}
	if localView.Result != CandidateLost || len(localView.Voters) != 1 ||
		fmt.Sprintf("%x", localView.Voters[0]) != fmt.Sprintf("%x", keys[4].pub) {
		t.Fatalf("loser view must keep its single real vote: %+v", localView)
	}

	// 胜出候选中的交易按其顺序确认一次。
	for _, id := range altList {
		info, _ := n.Tx(id)
		if info.Status != StatusConfirmed || info.BlockID != altID || info.BlockHeight != 1 {
			t.Fatalf("winner tx %s wrong: %+v", id, info)
		}
	}
}
