package consensus

import (
	"fmt"
	"testing"
)

// 回归场景：交易池里已有可打包交易、本地提议也确实包含交易时，另行登记的
// 空竞争候选仍能通过投票胜出。空候选确认一个没有交易的区块；本地提议中的
// 交易按落选与到期规则处理——到期轮次为 2 的失效，到期轮次为 100 的回到排队。
// 确认门槛、投票返回结构与公开查询行为均沿用现有规则。
func TestEmptyContendingCandidateWinsOverLocalProposal(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	// 两个不同发送者各提交序号 1、签名有效的交易：
	// 一笔到期轮次为 2，另一笔到期轮次为 100。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 2)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 本地提议包含两笔交易（费用高者在前）。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [a1 b1]", local.TxIDs)
	}

	// 同轮登记空竞争候选：新登记，与本地提议是不同的候选。
	reg, err := n.RegisterCandidate(1, []string{})
	if err != nil {
		t.Fatal(err)
	}
	emptyID := reg.BlockID
	if reg.Existing {
		t.Fatal("setup: empty candidate must be newly registered")
	}
	if emptyID == local.BlockID {
		t.Fatal("setup: empty candidate must differ from local proposal")
	}

	// 登记后两者并存：本地提议的标识与交易顺序保持原样，
	// 两笔交易都在等待投票，两个账户的已确认序号仍为 0。
	prop, ok := n.Proposal()
	if !ok || prop.BlockID != local.BlockID ||
		fmt.Sprint(prop.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("local proposal changed after registering competitor: %+v ok=%v", prop, ok)
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidates after register = %d, want 2 (local + empty)", len(rc.Candidates))
	}
	emptyView := findCandidate(t, n, 1, emptyID)
	if emptyView.Local || len(emptyView.TxIDs) != 0 {
		t.Fatalf("empty candidate view wrong: %+v", emptyView)
	}
	for _, id := range []string{a1.ID(), b1.ID()} {
		if info, _ := n.Tx(id); info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed (waiting-vote)", id, info.Status)
		}
	}
	for i, id := range []string{a1.ID(), b1.ID()} {
		acc := n.Account(keys[i].pub)
		if acc.ConfirmedSequence != 0 {
			t.Fatalf("account %d confirmed sequence = %d, want 0", i, acc.ConfirmedSequence)
		}
		if len(acc.Pending) != 1 || acc.Pending[0].ID != id ||
			acc.Pending[0].Status != StatusProposed || acc.Pending[0].Note != "waiting-vote" {
			t.Fatalf("account %d pending wrong while waiting for votes: %+v", i, acc.Pending)
		}
	}

	// 一名验证者投给本地提议，另外两名投给空候选：共三人参与投票，
	// 但空候选只有两票（四人名单需严格超过三分之二，即三票），不能确认。
	res, err := n.Vote(keys[0].pub, 1, local.BlockID)
	if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
		t.Fatalf("local vote must count without confirming: %+v %v", res, err)
	}
	for i := 1; i <= 2; i++ {
		res, err := n.Vote(keys[i].pub, 1, emptyID)
		if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
			t.Fatalf("empty vote %d must count without confirming: %+v %v", i, res, err)
		}
	}

	// 两者继续 pending，轮次与高度不变；到期轮次为 2 的交易也不能提前失效。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 before quorum", n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may exist before quorum")
	}
	emptyView = findCandidate(t, n, 1, emptyID)
	if emptyView.Result != CandidatePending || len(emptyView.Voters) != 2 {
		t.Fatalf("empty candidate wrong before quorum: %+v", emptyView)
	}
	localView := findCandidate(t, n, 1, local.BlockID)
	if localView.Result != CandidatePending || len(localView.Voters) != 1 {
		t.Fatalf("local candidate wrong before quorum: %+v", localView)
	}
	if rc, _ := n.Candidates(1); rc.Ended || len(rc.Unvoted) != 1 {
		t.Fatalf("round 1 must stay open with one unvoted validator: %+v", rc)
	}
	if info, _ := n.Tx(a1.ID()); info.Status != StatusProposed {
		t.Fatalf("expiry-2 tx status = %s in round 1, want proposed (must not expire early)", info.Status)
	}

	// 剩下的一名验证者投给空候选：第三票立即使它确认。
	// 投票结果给出该空候选的区块：交易列表为空、高度 1、轮次 1、前块标识为空。
	res, err = n.Vote(keys[3].pub, 1, emptyID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third empty-candidate vote should confirm: %+v %v", res, err)
	}
	if res.Block.ID != emptyID || res.Block.Height != 1 || res.Block.Round != 1 ||
		res.Block.PreviousID != "" || len(res.Block.TxIDs) != 0 {
		t.Fatalf("confirming block wrong: %+v", res.Block)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 after confirm", n.CurrentRound(), n.Height())
	}

	// 按高度查询与最新块查询得到同一个空块；确认历史只有这一条。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != emptyID || blk.Height != 1 || blk.Round != 1 || blk.PreviousID != "" ||
		len(blk.TxIDs) != 0 {
		t.Fatalf("stored block wrong: %+v", blk)
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != emptyID || len(latest.TxIDs) != 0 {
		t.Fatalf("latest block = %+v ok=%v, want the confirmed empty block", latest, ok)
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 2 got %v, want %s (history has exactly one entry)", err, ReasonUnknownBlock)
	}

	// 原本地提议中的两笔交易均未被确认：不关联空块，也不推进任何发送者的
	// 已确认序号。到期轮次为 2 的交易显示 expired、退出账户待处理列表，
	// 完整交易内容仍可按标识查到。
	info, err := n.Tx(a1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired || info.BlockID != "" || info.BlockHeight != 0 {
		t.Fatalf("expiry-2 tx wrong after confirm: %+v", info)
	}
	if info.Tx == nil || info.Tx.Sequence != 1 || info.Tx.Fee != 10 || info.Tx.Expiry != 2 ||
		string(info.Tx.Content) != "a1" {
		t.Fatalf("expired tx must keep original content: %+v", info.Tx)
	}
	if accA := n.Account(keys[0].pub); accA.ConfirmedSequence != 0 || len(accA.Pending) != 0 {
		t.Fatalf("expired-tx account wrong: %+v", accA)
	}

	// 到期轮次为 100 的交易回到 queued：账户待处理列表保留它并显示
	// waiting-pack，缺口为 0，已确认序号仍为 0。
	info, err = n.Tx(b1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued || info.BlockID != "" || info.BlockHeight != 0 {
		t.Fatalf("expiry-100 tx wrong after confirm: %+v", info)
	}
	accB := n.Account(keys[1].pub)
	if accB.ConfirmedSequence != 0 || accB.Gap != 0 {
		t.Fatalf("expiry-100 account wrong: %+v", accB)
	}
	if len(accB.Pending) != 1 || accB.Pending[0].ID != b1.ID() ||
		accB.Pending[0].Status != StatusQueued || accB.Pending[0].Note != "waiting-pack" {
		t.Fatalf("expiry-100 pending wrong: %+v", accB.Pending)
	}

	// 查询刚结束的第 1 轮：空候选 won、本地提议 lost，各自保留原来的
	// 交易列表与实际投票者；轮次已结束，但不是主动结束的未确认轮次。
	rc, err = n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Unvoted) != 0 {
		t.Fatalf("decided round record wrong: %+v", rc)
	}
	emptyView = findCandidate(t, n, 1, emptyID)
	if emptyView.Result != CandidateWon || emptyView.Local || len(emptyView.TxIDs) != 0 {
		t.Fatalf("winning empty candidate view wrong: %+v", emptyView)
	}
	if len(emptyView.Voters) != 3 {
		t.Fatalf("winner voters = %d, want 3", len(emptyView.Voters))
	}
	wantVoters := map[string]bool{}
	for _, i := range []int{1, 2, 3} {
		wantVoters[fmt.Sprintf("%x", keys[i].pub)] = true
	}
	gotVoters := map[string]bool{}
	for _, v := range emptyView.Voters {
		gotVoters[fmt.Sprintf("%x", v)] = true
	}
	if fmt.Sprint(gotVoters) != fmt.Sprint(wantVoters) {
		t.Fatalf("winner voters = %v, want %v", gotVoters, wantVoters)
	}
	localView = findCandidate(t, n, 1, local.BlockID)
	if localView.Result != CandidateLost || !localView.Local ||
		fmt.Sprint(localView.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("losing local candidate view wrong: %+v", localView)
	}
	if len(localView.Voters) != 1 ||
		fmt.Sprintf("%x", localView.Voters[0]) != fmt.Sprintf("%x", keys[0].pub) {
		t.Fatalf("loser must keep its single real vote: %+v", localView)
	}
}
