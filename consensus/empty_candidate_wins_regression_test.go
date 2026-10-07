package consensus

import (
	"fmt"
	"testing"
)

// 回归场景：交易池已有可打包交易、本地提议也确实包含交易时，另行登记的空竞争
// 候选仍能通过投票胜出。空候选获胜确认一个没有交易的区块；本地提议中的交易
// 依照落选与到期规则处理：到期轮次为 2 的交易在进入第 2 轮时失效，到期轮次为
// 100 的交易回到排队。确认门槛、投票返回结构与公开查询行为均与既有规则一致。
func TestEmptyCandidateWinsOverNonEmptyLocalProposal(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	// 两个不同发送者各提交序号 1、签名有效的交易：
	// a1 到期轮次 2（确认后进入的第 2 轮恰好到期），b1 到期轮次 100。
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

	// 同轮登记空竞争候选：两者作为不同候选并存。
	altID := registerMust(t, n, 1, []string{})
	if altID == local.BlockID {
		t.Fatal("test setup: empty candidate must differ from local proposal")
	}
	// 原本地提议的标识与交易顺序保持原样。
	prop, ok := n.Proposal()
	if !ok || prop.BlockID != local.BlockID ||
		fmt.Sprint(prop.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("local proposal changed after registering empty candidate: %+v", prop)
	}
	if rc, err := n.Candidates(1); err != nil || len(rc.Candidates) != 2 {
		t.Fatalf("setup: round 1 should have 2 candidates: %+v %v", rc, err)
	}
	// 两笔交易都在等待投票，两个账户的已确认序号仍为 0。
	for _, id := range []string{a1.ID(), b1.ID()} {
		if info, _ := n.Tx(id); info.Status != StatusProposed {
			t.Fatalf("setup: tx %s status = %s, want proposed", id, info.Status)
		}
	}
	for i := 0; i < 2; i++ {
		acc := n.Account(keys[i].pub)
		if acc.ConfirmedSequence != 0 || len(acc.Pending) != 1 ||
			acc.Pending[0].Note != "waiting-vote" {
			t.Fatalf("setup: account %d wrong: %+v", i, acc)
		}
	}

	// 一名验证者投给本地提议，另外两名投给空候选：共三人参与投票，
	// 但空候选只有两票（四人名单需严格超过三分之二，即三票），不能确认。
	res, err := n.Vote(keys[0].pub, 1, local.BlockID)
	if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
		t.Fatalf("local vote must count without confirming: %+v %v", res, err)
	}
	for i := 1; i <= 2; i++ {
		res, err := n.Vote(keys[i].pub, 1, altID)
		if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
			t.Fatalf("alt vote %d must count without confirming: %+v %v", i, res, err)
		}
	}
	// 两者继续 pending，轮次和高度不变，到期轮次为 2 的交易也不能提前失效。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 before quorum", n.CurrentRound(), n.Height())
	}
	altView := findCandidate(t, n, 1, altID)
	if altView.Result != CandidatePending || len(altView.Voters) != 2 {
		t.Fatalf("empty candidate wrong before quorum: %+v", altView)
	}
	localView := findCandidate(t, n, 1, local.BlockID)
	if localView.Result != CandidatePending || len(localView.Voters) != 1 {
		t.Fatalf("local candidate wrong before quorum: %+v", localView)
	}
	if info, _ := n.Tx(a1.ID()); info.Status != StatusProposed {
		t.Fatalf("a1 status = %s, want proposed (must not expire before round ends)", info.Status)
	}

	// 剩下的一名验证者投给空候选：第三张票立即使它确认。
	res, err = n.Vote(keys[3].pub, 1, altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third alt vote should confirm: %+v %v", res, err)
	}
	// 投票结果给出该空候选的区块：交易列表为空、高度 1、轮次 1、前块标识为空。
	if res.Block.ID != altID || res.Block.Height != 1 || res.Block.Round != 1 ||
		res.Block.PreviousID != "" || len(res.Block.TxIDs) != 0 {
		t.Fatalf("confirming block wrong: %+v", res.Block)
	}
	// 节点随后进入第 2 轮。
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 after confirm", n.CurrentRound(), n.Height())
	}

	// 按高度查询和最新块查询都得到同一个空块，确认历史只增加这一条。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != altID || blk.Height != 1 || blk.Round != 1 || blk.PreviousID != "" ||
		len(blk.TxIDs) != 0 {
		t.Fatalf("stored block wrong: %+v", blk)
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != altID || len(latest.TxIDs) != 0 {
		t.Fatalf("latest block = %+v ok=%v, want empty winning candidate block", latest, ok)
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 2 got %v, want %s (history has exactly one entry)", err, ReasonUnknownBlock)
	}

	// 原本地提议中的两笔交易均没有被确认，不能关联到这个空块，
	// 也不能推进任何发送者的已确认序号。
	for _, id := range []string{a1.ID(), b1.ID()} {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status == StatusConfirmed || info.BlockID != "" || info.BlockHeight != 0 {
			t.Fatalf("losing-proposal tx %s must not be confirmed: %+v", id, info)
		}
	}
	for i := 0; i < 2; i++ {
		if acc := n.Account(keys[i].pub); acc.ConfirmedSequence != 0 {
			t.Fatalf("account %d confirmed sequence = %d, want 0", i, acc.ConfirmedSequence)
		}
	}

	// 到期轮次为 2 的交易显示 expired 并退出账户待处理列表，
	// 完整交易内容仍可按标识查到。
	info, err := n.Tx(a1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired {
		t.Fatalf("a1 status = %s, want expired", info.Status)
	}
	if info.Tx == nil || info.Tx.Sequence != 1 || info.Tx.Fee != 10 || info.Tx.Expiry != 2 ||
		string(info.Tx.Content) != "a1" || !info.Tx.Verify() {
		t.Fatalf("expired tx must keep full original content: %+v", info.Tx)
	}
	if acc := n.Account(keys[0].pub); len(acc.Pending) != 0 {
		t.Fatalf("expired tx must leave account pending list: %+v", acc.Pending)
	}

	// 到期轮次为 100 的交易回到 queued，账户待处理列表保留它并显示
	// waiting-pack，缺口为 0。
	info, err = n.Tx(b1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("b1 status = %s, want queued", info.Status)
	}
	accB := n.Account(keys[1].pub)
	if accB.Gap != 0 || len(accB.Pending) != 1 || accB.Pending[0].ID != b1.ID() ||
		accB.Pending[0].Status != StatusQueued || accB.Pending[0].Note != "waiting-pack" {
		t.Fatalf("account b pending wrong: %+v", accB)
	}

	// 查询刚结束的第 1 轮：空候选 won、本地提议 lost，各自保留原来的
	// 交易列表与实际投票者；轮次已结束，但不是主动结束的未确认轮次。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Unvoted) != 0 {
		t.Fatalf("round 1 record wrong: %+v", rc)
	}
	altView = findCandidate(t, n, 1, altID)
	if altView.Result != CandidateWon || len(altView.TxIDs) != 0 ||
		len(altView.Voters) != 3 {
		t.Fatalf("empty winner view wrong: %+v", altView)
	}
	wantVoters := map[string]bool{}
	for _, i := range []int{1, 2, 3} {
		wantVoters[fmt.Sprintf("%x", keys[i].pub)] = true
	}
	gotVoters := map[string]bool{}
	for _, v := range altView.Voters {
		gotVoters[fmt.Sprintf("%x", v)] = true
	}
	if fmt.Sprint(gotVoters) != fmt.Sprint(wantVoters) {
		t.Fatalf("empty winner voters = %v, want %v", gotVoters, wantVoters)
	}
	localView = findCandidate(t, n, 1, local.BlockID)
	if localView.Result != CandidateLost || !localView.Local ||
		fmt.Sprint(localView.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) ||
		len(localView.Voters) != 1 ||
		fmt.Sprintf("%x", localView.Voters[0]) != fmt.Sprintf("%x", keys[0].pub) {
		t.Fatalf("losing local view wrong: %+v", localView)
	}
}
