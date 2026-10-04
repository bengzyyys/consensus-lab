package consensus

import (
	"fmt"
	"testing"
)

// 回归场景：交易在第 1 轮仍有效，但确认后进入的第 2 轮恰好等于它的到期轮次。
// 同一次确认要固定三个结果的先后关系：胜出交易只确认一次、保留竞争候选的
// 确认块标识与高度，不因进入到期轮次而变成过期；落选候选独有的交易先回到
// 排队、再按到期规则失效；未到期交易继续排队，且不能越过序号缺口被打包。
func TestConfirmIntoExpiryRoundKeepsWinnerConfirmed(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	// 两个发送者各提交序号 1：费用 10 与费用 5，到期轮次都为 2；
	// 费用 5 的发送者另有序号 2、费用 100、到期轮次 100 的交易。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 2)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 2)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 100, 100)
	for _, tx := range []*Transaction{a1, b1, b2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 本地提议包含两笔序号 1 交易（费用高者在前），序号 2 仍在排队。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("setup: local proposal = %v, want [a1 b1]", local.TxIDs)
	}
	if info, _ := n.Tx(b2.ID()); info.Status != StatusQueued {
		t.Fatalf("setup: b2 status = %s, want queued", info.Status)
	}

	// 竞争候选只包含费用 10 的那笔交易。
	altID := registerMust(t, n, 1, []string{a1.ID()})
	if altID == local.BlockID {
		t.Fatal("test setup: competing candidate must differ from local proposal")
	}

	// 两票计入但不确认：四人名单需严格超过三分之二，即三票。
	for i := 0; i < 2; i++ {
		res, err := n.Vote(keys[i].pub, 1, altID)
		if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
			t.Fatalf("vote %d must count without confirming: %+v %v", i+1, res, err)
		}
	}
	// 轮次与高度不前进，到期轮次为 2 的两笔交易仍有效，已确认序号不提前变化。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0 before quorum", n.CurrentRound(), n.Height())
	}
	for _, id := range []string{a1.ID(), b1.ID()} {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed (still valid in round 1)", id, info.Status)
		}
	}
	for _, k := range keys[:2] {
		if acc := n.Account(k.pub); acc.ConfirmedSequence != 0 {
			t.Fatalf("confirmed sequence = %d before quorum, want 0", acc.ConfirmedSequence)
		}
	}

	// 第三票使竞争候选胜出，并进入第 2 轮。
	res, err := n.Vote(keys[2].pub, 1, altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote should confirm: %+v %v", res, err)
	}
	if res.Block.ID != altID || res.Block.Height != 1 || res.Block.Round != 1 ||
		fmt.Sprint(res.Block.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("confirming block wrong: %+v", res.Block)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1 after confirm", n.CurrentRound(), n.Height())
	}

	// 胜出交易虽也在落选的本地提议中，仍只确认一次：保留竞争候选对应的
	// 确认块标识与高度，不能因进入其到期轮次（第 2 轮）而变成过期。
	info, err := n.Tx(a1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockID != altID || info.BlockHeight != 1 {
		t.Fatalf("winner tx wrong: %+v", info)
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != altID || blk.Height != 1 || blk.Round != 1 || blk.PreviousID != "" ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("stored block wrong: %+v", blk)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != altID {
		t.Fatalf("latest block = %+v ok=%v, want competing candidate block", latest, ok)
	}
	// 胜出账户：已确认序号推进到 1，待处理列表移除该交易。
	if accA := n.Account(keys[0].pub); accA.ConfirmedSequence != 1 || len(accA.Pending) != 0 {
		t.Fatalf("winner account wrong: %+v", accA)
	}

	// 另一笔序号 1 交易只属于落选候选：进入第 2 轮后显示 expired，
	// 保留原交易内容，不留确认块关联，也不推进该账户的已确认序号。
	info, err = n.Tx(b1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired || info.BlockID != "" || info.BlockHeight != 0 {
		t.Fatalf("losing-only tx wrong: %+v", info)
	}
	if info.Tx == nil || info.Tx.Sequence != 1 || info.Tx.Fee != 5 || info.Tx.Expiry != 2 ||
		string(info.Tx.Content) != "b1" {
		t.Fatalf("expired tx must keep original content: %+v", info.Tx)
	}
	accB := n.Account(keys[1].pub)
	if accB.ConfirmedSequence != 0 {
		t.Fatalf("loser confirmed sequence = %d, want 0", accB.ConfirmedSequence)
	}

	// 序号 2 交易尚未到期，继续以 queued 留在待处理列表，最早缺口为 1。
	if accB.Gap != 1 {
		t.Fatalf("earliest gap = %d, want 1", accB.Gap)
	}
	if len(accB.Pending) != 1 || accB.Pending[0].ID != b2.ID() ||
		accB.Pending[0].Status != StatusQueued || accB.Pending[0].Note != "waiting-pack" {
		t.Fatalf("loser pending wrong: %+v", accB.Pending)
	}
	if info, _ := n.Tx(b2.ID()); info.Status != StatusQueued {
		t.Fatalf("b2 status = %s, want queued", info.Status)
	}

	// 即使费用最高，序号 2 交易也不能越过缺口进入第 2 轮提议：空块。
	prop2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if prop2.Round != 2 || !prop2.Empty || len(prop2.TxIDs) != 0 {
		t.Fatalf("round 2 proposal = %+v, want empty block", prop2)
	}

	// 候选历史：竞争候选胜出、本地提议落选，各自保留原来的交易顺序。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd || len(rc.Unvoted) != 1 {
		t.Fatalf("round 1 record wrong: %+v", rc)
	}
	altView := findCandidate(t, n, 1, altID)
	if altView.Result != CandidateWon || len(altView.Voters) != 3 ||
		fmt.Sprint(altView.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("winner candidate view wrong: %+v", altView)
	}
	localView := findCandidate(t, n, 1, local.BlockID)
	if localView.Result != CandidateLost || !localView.Local ||
		fmt.Sprint(localView.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("losing local candidate view wrong: %+v", localView)
	}

	// 历史引用不能把过期交易重新变成等待投票，也不改变待处理列表。
	if info, _ := n.Tx(b1.ID()); info.Status != StatusExpired {
		t.Fatalf("expired tx revived by history query: %s", info.Status)
	}
	if accB := n.Account(keys[1].pub); len(accB.Pending) != 1 || accB.Pending[0].ID != b2.ID() {
		t.Fatalf("history query changed pending list: %+v", accB.Pending)
	}
}
