package consensus

import (
	"fmt"
	"testing"
)

// 竞争候选只包含账户连续交易的最前面一笔：同一账户序号 3、4、5、6 进入本地提议
// （单块上限 3，故提议含 3、4、5，6 留在池中），随后只含 3 的竞争候选经投票胜出。
// 胜出的候选只确认 3 一笔，4、5 必须回到排队，与 6 一起按序号留在待处理列表；
// 下一轮本地提议应依次包含 4、5、6——费用更高的 5、6 不能越过 4，已确认的 3
// 不能再次入选，也不能因为 4、5 曾属于落选候选而被漏掉。
func TestContenderConfirmsOnlySharedPrefix(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 3) // 四名验证者，单块最多三笔，池容量不限
	sender := genKeys(t, 1)[0]

	// 先让序号 1、2 正常确认：本地提议打包两笔，三票确认本地提议。
	tx1 := NewTransaction(sender.priv, 1, []byte("a1"), 50, 1000)
	tx2 := NewTransaction(sender.priv, 2, []byte("a2"), 50, 1000)
	for _, tx := range []*Transaction{tx1, tx2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local1, _ := n.Propose()
	if fmt.Sprint(local1.TxIDs) != fmt.Sprint([]string{tx1.ID(), tx2.ID()}) {
		t.Fatalf("setup: round 1 proposal = %v, want [tx1 tx2]", local1.TxIDs)
	}
	confirmCandidate(t, n, keys, local1.BlockID)
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	blk1, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if acct := n.Account(sender.pub); acct.ConfirmedSequence != 2 {
		t.Fatalf("setup: confirmed sequence = %d, want 2", acct.ConfirmedSequence)
	}

	// 提交序号 3、4、5、6，费用分别为 10、1、100、200，到期轮次足够远。
	tx3 := NewTransaction(sender.priv, 3, []byte("a3"), 10, 1000)
	tx4 := NewTransaction(sender.priv, 4, []byte("a4"), 1, 1000)
	tx5 := NewTransaction(sender.priv, 5, []byte("a5"), 100, 1000)
	tx6 := NewTransaction(sender.priv, 6, []byte("a6"), 200, 1000)
	for _, tx := range []*Transaction{tx3, tx4, tx5, tx6} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 本地提议依次包含 3、4、5（账户内按序号，费用不改变顺序），6 仍在排队。
	local2, _ := n.Propose()
	wantLocal2 := []string{tx3.ID(), tx4.ID(), tx5.ID()}
	if fmt.Sprint(local2.TxIDs) != fmt.Sprint(wantLocal2) {
		t.Fatalf("round 2 local proposal = %v, want [tx3 tx4 tx5]", local2.TxIDs)
	}
	if info, _ := n.Tx(tx6.ID()); info.Status != StatusQueued {
		t.Fatalf("tx6 status = %s, want queued (block limit 3)", info.Status)
	}

	// 登记只包含 3 的竞争候选。
	contenderID := registerMust(t, n, 2, []string{tx3.ID()})
	if contenderID == local2.BlockID {
		t.Fatal("test setup: contender must differ from local proposal")
	}

	// 两票投给竞争候选：计入但未达门槛（4 名验证者需严格超过 2/3，即 3 票）。
	for i := 0; i < 2; i++ {
		res, err := n.Vote(keys[i].pub, 2, contenderID)
		if err != nil || !res.Counted || res.Confirmed {
			t.Fatalf("contender vote %d must count without confirming: %+v %v", i+1, res, err)
		}
	}
	// 未决出期间：轮次、确认高度、确认历史与账户已确认序号都不变；
	// 3、4、5 继续等待投票，6 继续等待打包。
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("before quorum round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.Height != 1 || latest.ID != blk1.ID {
		t.Fatalf("before quorum latest block = %+v, want height 1 block %s", latest, blk1.ID)
	}
	if acct := n.Account(sender.pub); acct.ConfirmedSequence != 2 {
		t.Fatalf("before quorum confirmed sequence = %d, want 2", acct.ConfirmedSequence)
	}
	for _, id := range []string{tx3.ID(), tx4.ID(), tx5.ID()} {
		if info, _ := n.Tx(id); info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed while pending", id, info.Status)
		}
	}
	if info, _ := n.Tx(tx6.ID()); info.Status != StatusQueued {
		t.Fatalf("tx6 status = %s, want queued while pending", info.Status)
	}
	c := findCandidate(t, n, 2, contenderID)
	if c.Result != CandidatePending || len(c.Voters) != 2 {
		t.Fatalf("contender wrong before quorum: %+v", c)
	}

	// 第三票使竞争候选胜出：本轮只新增该候选的一条确认记录，交易列表仅有 3，
	// 前块标识接在此前的最新确认块之后。
	res, err := n.Vote(keys[2].pub, 2, contenderID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third contender vote should confirm: %+v %v", res, err)
	}
	if res.Block.Height != 2 || res.Block.Round != 2 || res.Block.ID != contenderID {
		t.Fatalf("confirming block wrong: %+v", res.Block)
	}
	if fmt.Sprint(res.Block.TxIDs) != fmt.Sprint([]string{tx3.ID()}) {
		t.Fatalf("confirming block txs = %v, want [tx3]", res.Block.TxIDs)
	}
	if res.Block.PreviousID != blk1.ID {
		t.Fatalf("confirming block previous = %q, want latest confirmed block %q",
			res.Block.PreviousID, blk1.ID)
	}
	if n.CurrentRound() != 3 || n.Height() != 2 {
		t.Fatalf("after confirm round=%d height=%d, want 3/2", n.CurrentRound(), n.Height())
	}
	// 确认历史只有两条：原有序号 1、2 的块与胜出的竞争候选，落选提议不产生记录。
	blk2, err := n.BlockAt(2)
	if err != nil {
		t.Fatal(err)
	}
	if blk2.ID != contenderID || blk2.PreviousID != blk1.ID ||
		fmt.Sprint(blk2.TxIDs) != fmt.Sprint([]string{tx3.ID()}) {
		t.Fatalf("stored block 2 wrong: %+v", blk2)
	}
	if latest, _ := n.LatestBlock(); latest.ID != contenderID || latest.Height != 2 {
		t.Fatalf("latest block = %+v, want contender at height 2", latest)
	}

	// 共享的 3 只确认一次，关联胜出候选的区块标识与高度；账户已确认序号只推进到 3。
	info3, _ := n.Tx(tx3.ID())
	if info3.Status != StatusConfirmed || info3.BlockID != contenderID || info3.BlockHeight != 2 {
		t.Fatalf("tx3 wrong after confirm: %+v", info3)
	}
	acct := n.Account(sender.pub)
	if acct.ConfirmedSequence != 3 {
		t.Fatalf("confirmed sequence = %d, want 3", acct.ConfirmedSequence)
	}

	// 4、5 虽曾进入落选的本地提议，也应回到 queued，与 6 一起按 4、5、6 留在
	// 账户待处理列表，说明均为 waiting-pack，缺口为 0；不带确认块关联，
	// 保留原始交易内容与有效签名。
	if acct.Gap != 0 {
		t.Fatalf("account gap = %d, want 0", acct.Gap)
	}
	if len(acct.Pending) != 3 {
		t.Fatalf("account pending = %d, want 3 (tx4 tx5 tx6)", len(acct.Pending))
	}
	wantPending := map[string]*Transaction{tx4.ID(): tx4, tx5.ID(): tx5, tx6.ID(): tx6}
	for i, id := range []string{tx4.ID(), tx5.ID(), tx6.ID()} {
		p := acct.Pending[i]
		if p.ID != id || p.Status != StatusQueued || p.Note != "waiting-pack" {
			t.Fatalf("pending[%d] = %+v, want %s queued waiting-pack", i, p, id)
		}
		if p.BlockHeight != 0 || p.BlockID != "" {
			t.Fatalf("pending tx %s must not reference a confirmed block: %+v", id, p)
		}
		orig := wantPending[id]
		if p.Tx == nil || !p.Tx.Verify() || p.Tx.Fee != orig.Fee || p.Tx.Sequence != orig.Sequence ||
			fmt.Sprintf("%x", p.Tx.Content) != fmt.Sprintf("%x", orig.Content) ||
			fmt.Sprintf("%x", p.Tx.Signature) != fmt.Sprintf("%x", orig.Signature) {
			t.Fatalf("pending tx %s lost original content or valid signature: %+v", id, p.Tx)
		}
		info, _ := n.Tx(id)
		if info.Status != StatusQueued || info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %s query wrong after losing proposal: %+v", id, info)
		}
	}

	// 旧轮候选查询：本地提议落选、竞争候选胜出，各自保留原来的交易顺序。
	rc, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("round 2 record wrong: %+v", rc)
	}
	localView := findCandidate(t, n, 2, local2.BlockID)
	if !localView.Local || localView.Result != CandidateLost ||
		fmt.Sprint(localView.TxIDs) != fmt.Sprint(wantLocal2) {
		t.Fatalf("losing local proposal view wrong: %+v", localView)
	}
	contenderView := findCandidate(t, n, 2, contenderID)
	if contenderView.Local || contenderView.Result != CandidateWon ||
		fmt.Sprint(contenderView.TxIDs) != fmt.Sprint([]string{tx3.ID()}) ||
		len(contenderView.Voters) != 3 {
		t.Fatalf("winning contender view wrong: %+v", contenderView)
	}
	// 读取历史记录不能使已经回到排队的交易重新等待投票。
	acct = n.Account(sender.pub)
	for _, p := range acct.Pending {
		if p.Status != StatusQueued || p.Note != "waiting-pack" {
			t.Fatalf("reading old round must not re-propose tx %s: %+v", p.ID, p)
		}
	}

	// 下一轮本地提议依次包含 4、5、6：费用更高的 5、6 不能越过 4，
	// 已确认的 3 不能再次入选，4、5 也不能因曾属于落选候选而漏掉。
	local3, _ := n.Propose()
	if local3.Round != 3 {
		t.Fatalf("round 3 proposal round = %d, want 3", local3.Round)
	}
	if fmt.Sprint(local3.TxIDs) != fmt.Sprint([]string{tx4.ID(), tx5.ID(), tx6.ID()}) {
		t.Fatalf("round 3 local proposal = %v, want [tx4 tx5 tx6]", local3.TxIDs)
	}
	// 新提议只让余下交易转为等待投票：已确认序号仍为 3，
	// 已有确认块与旧轮候选结果保持原样。
	acct = n.Account(sender.pub)
	if acct.ConfirmedSequence != 3 || acct.Gap != 0 || len(acct.Pending) != 3 {
		t.Fatalf("account wrong after round 3 proposal: %+v", acct)
	}
	for _, p := range acct.Pending {
		if p.Status != StatusProposed || p.Note != "waiting-vote" {
			t.Fatalf("pending tx %s should wait for vote after new proposal: %+v", p.ID, p)
		}
	}
	if n.CurrentRound() != 3 || n.Height() != 2 {
		t.Fatalf("round=%d height=%d, want 3/2 after new proposal", n.CurrentRound(), n.Height())
	}
	if latest, _ := n.LatestBlock(); latest.ID != contenderID || latest.Height != 2 {
		t.Fatalf("confirmed history changed by new proposal: %+v", latest)
	}
	if info, _ := n.Tx(tx3.ID()); info.Status != StatusConfirmed || info.BlockID != contenderID {
		t.Fatalf("tx3 confirmation changed by new proposal: %+v", info)
	}
	localView = findCandidate(t, n, 2, local2.BlockID)
	contenderView = findCandidate(t, n, 2, contenderID)
	if localView.Result != CandidateLost || contenderView.Result != CandidateWon {
		t.Fatalf("old round results changed: local=%s contender=%s",
			localView.Result, contenderView.Result)
	}
}
