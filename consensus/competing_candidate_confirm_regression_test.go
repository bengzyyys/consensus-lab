package consensus

import (
	"fmt"
	"testing"
)

// 竞争候选只包含账户连续交易的最前面一笔并胜出时：
// 只有该笔随胜出候选确认，落选本地提议中的其余交易回到排队，
// 与从未打包的交易一起按账户序号在下一轮正常打包。
//
// 场景固定为：四名验证者、单块最多三笔、交易池不限容量。
// 账户序号 1、2 先正常确认；随后提交序号 3、4、5、6（费用 10、1、100、200，
// 到期轮次足够远）。本地提议为 [3 4 5]，6 留在池中；再登记只含 3 的竞争候选。
// 竞争候选两票时一切不变，第三票胜出后仅 3 确认一次，4、5 回到 queued，
// 与 6 一起按 4、5、6 等待打包；下一轮本地提议依次为 [4 5 6]。
func TestCompetingCandidateConfirmRequeuesRemaining(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 3) // 单块上限 3，池容量 0 表示不限制
	sender := keys[0]               // 发送者同时是验证者不影响本场景

	// 前两轮：序号 1、2 各自随本地提议正常确认，确认历史推进到高度 2。
	tx1 := NewTransaction(sender.priv, 1, []byte("tx-1"), 5, 1000)
	tx2 := NewTransaction(sender.priv, 2, []byte("tx-2"), 5, 1000)
	for _, tx := range []*Transaction{tx1, tx2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit seq %d: %v", tx.Sequence, err)
		}
		local, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{tx.ID()}) {
			t.Fatalf("setup round %d: local proposal = %v, want [%s]", n.CurrentRound(), local.TxIDs, tx.ID())
		}
		confirmCandidate(t, n, keys, local.BlockID)
	}
	if n.CurrentRound() != 3 || n.Height() != 2 {
		t.Fatalf("setup: round=%d height=%d, want 3/2", n.CurrentRound(), n.Height())
	}
	block1, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	block2, err := n.BlockAt(2)
	if err != nil {
		t.Fatal(err)
	}
	if block2.PreviousID != block1.ID {
		t.Fatalf("setup: block2 previous = %q, want %q", block2.PreviousID, block1.ID)
	}

	// 提交序号 3、4、5、6：费用 10、1、100、200，到期轮次足够远。
	tx3 := NewTransaction(sender.priv, 3, []byte("tx-3"), 10, 1000)
	tx4 := NewTransaction(sender.priv, 4, []byte("tx-4"), 1, 1000)
	tx5 := NewTransaction(sender.priv, 5, []byte("tx-5"), 100, 1000)
	tx6 := NewTransaction(sender.priv, 6, []byte("tx-6"), 200, 1000)
	for _, tx := range []*Transaction{tx3, tx4, tx5, tx6} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit seq %d: %v", tx.Sequence, err)
		}
	}
	id3, id4, id5, id6 := tx3.ID(), tx4.ID(), tx5.ID(), tx6.ID()

	// 本地提议按账户序号依次包含 3、4、5；费用更高的 6 因单块上限留在池中排队。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if local.Round != 3 || local.Empty {
		t.Fatalf("local proposal view wrong: %+v", local)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{id3, id4, id5}) {
		t.Fatalf("local proposal = %v, want [3 4 5] by account sequence", local.TxIDs)
	}
	for _, id := range []string{id3, id4, id5} {
		if info, _ := n.Tx(id); info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed", id, info.Status)
		}
	}
	if info, _ := n.Tx(id6); info.Status != StatusQueued {
		t.Fatalf("tx 6 status = %s, want queued (block limit 3)", info.Status)
	}

	// 登记只含 3 的竞争候选。
	altID := registerMust(t, n, 3, []string{id3})
	if altID == local.BlockID {
		t.Fatal("test setup: competing candidate must differ from local proposal")
	}

	// 竞争候选取得两票（4 人名单需严格超过 2 票）时仍未确认：
	// 轮次、确认高度、已有确认历史与账户已确认序号都不改变，
	// 3、4、5 继续等待投票，6 继续等待打包。
	for i := 0; i < 2; i++ {
		res, err := n.Vote(keys[i].pub, 3, altID)
		if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
			t.Fatalf("alt vote %d must count without confirming: %+v %v", i+1, res, err)
		}
	}
	if n.CurrentRound() != 3 || n.Height() != 2 {
		t.Fatalf("before quorum round=%d height=%d, want 3/2", n.CurrentRound(), n.Height())
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != block2.ID {
		t.Fatalf("latest block before quorum = %+v, want height-2 block %s", latest, block2.ID)
	}
	for h, want := range map[uint64]string{1: block1.ID, 2: block2.ID} {
		blk, err := n.BlockAt(h)
		if err != nil || blk.ID != want {
			t.Fatalf("confirmed history at height %d changed before quorum: %+v", h, blk)
		}
	}
	acct := n.Account(sender.pub)
	if acct.ConfirmedSequence != 2 {
		t.Fatalf("confirmed sequence = %d, want 2 before quorum", acct.ConfirmedSequence)
	}
	assertPending(t, acct, []string{id3, id4, id5, id6}, []string{"waiting-vote", "waiting-vote", "waiting-vote", "waiting-pack"})
	altView := findCandidate(t, n, 3, altID)
	localView := findCandidate(t, n, 3, local.BlockID)
	if altView.Result != CandidatePending || len(altView.Voters) != 2 {
		t.Fatalf("alt candidate wrong before quorum: %+v", altView)
	}
	if localView.Result != CandidatePending {
		t.Fatalf("local candidate must stay pending before quorum: %+v", localView)
	}

	// 第三票使竞争候选胜出：本轮只新增该候选的一条确认记录，交易列表仅有 3，
	// 前块标识接在此前的最新确认块（高度 2）之后。
	res, err := n.Vote(keys[2].pub, 3, altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third alt vote should confirm: %+v %v", res, err)
	}
	if res.Block.Height != 3 || res.Block.Round != 3 || res.Block.ID != altID {
		t.Fatalf("confirming block wrong: %+v", res.Block)
	}
	if fmt.Sprint(res.Block.TxIDs) != fmt.Sprint([]string{id3}) {
		t.Fatalf("confirming block txs = %v, want [3]", res.Block.TxIDs)
	}
	if res.Block.PreviousID != block2.ID {
		t.Fatalf("confirming block previous = %q, want latest confirmed %q", res.Block.PreviousID, block2.ID)
	}
	if n.CurrentRound() != 4 || n.Height() != 3 {
		t.Fatalf("after confirm round=%d height=%d, want 4/3", n.CurrentRound(), n.Height())
	}
	if latest, _ := n.LatestBlock(); latest.ID != altID {
		t.Fatalf("latest block = %q, want winning candidate %q", latest.ID, altID)
	}

	// 共享的 3 只确认一次：关联胜出候选的区块标识与高度，账户已确认序号只推进到 3。
	info3, err := n.Tx(id3)
	if err != nil {
		t.Fatal(err)
	}
	if info3.Status != StatusConfirmed || info3.BlockHeight != 3 || info3.BlockID != altID {
		t.Fatalf("shared tx 3 must confirm exactly once with winner block: %+v", info3)
	}
	acct = n.Account(sender.pub)
	if acct.ConfirmedSequence != 3 {
		t.Fatalf("confirmed sequence = %d, want exactly 3", acct.ConfirmedSequence)
	}

	// 4、5 虽曾进入落选的本地提议，也应回到 queued，与原先未打包的 6 一起
	// 按 4、5、6 留在账户待处理列表，说明均为 waiting-pack，缺口为 0。
	assertPending(t, acct, []string{id4, id5, id6}, []string{"waiting-pack", "waiting-pack", "waiting-pack"})

	// 回到排队的交易不带确认块关联，保留原始交易内容与有效签名。
	for _, tx := range []*Transaction{tx4, tx5, tx6} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued || info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("requeued tx %d must not carry block association: %+v", tx.Sequence, info)
		}
		if info.Tx == nil || !info.Tx.Verify() {
			t.Fatalf("requeued tx %d must keep a valid signature: %+v", tx.Sequence, info.Tx)
		}
		if fmt.Sprintf("%x", info.Tx.Signature) != fmt.Sprintf("%x", tx.Signature) ||
			fmt.Sprintf("%x", info.Tx.Content) != fmt.Sprintf("%x", tx.Content) ||
			info.Tx.Fee != tx.Fee || info.Tx.Expiry != tx.Expiry {
			t.Fatalf("requeued tx %d content changed: %+v, want original %+v", tx.Sequence, info.Tx, tx)
		}
	}

	// 旧轮候选查询：本地提议落选、竞争候选胜出，各自保留原来的交易顺序。
	rc, err := n.Candidates(3)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("round 3 record wrong after confirm: %+v", rc)
	}
	localView = findCandidate(t, n, 3, local.BlockID)
	altView = findCandidate(t, n, 3, altID)
	if localView.Result != CandidateLost || !localView.Local ||
		fmt.Sprint(localView.TxIDs) != fmt.Sprint([]string{id3, id4, id5}) {
		t.Fatalf("local candidate must be lost with original order [3 4 5]: %+v", localView)
	}
	if altView.Result != CandidateWon || altView.Local ||
		fmt.Sprint(altView.TxIDs) != fmt.Sprint([]string{id3}) {
		t.Fatalf("alt candidate must be won with original order [3]: %+v", altView)
	}

	// 读取历史记录不能使已经回到排队的交易重新等待投票。
	for _, id := range []string{id4, id5, id6} {
		if info, _ := n.Tx(id); info.Status != StatusQueued {
			t.Fatalf("reading history re-proposed tx %s: status = %s, want queued", id, info.Status)
		}
	}

	// 下一轮：本地提议依次包含 4、5、6。费用更高的 5、6 不能越过 4 排在前面，
	// 已确认的 3 不能再次入选，4、5 也不因曾属于落选候选而被漏掉。
	next, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if next.Round != 4 || fmt.Sprint(next.TxIDs) != fmt.Sprint([]string{id4, id5, id6}) {
		t.Fatalf("next round proposal = %v (round %d), want [4 5 6] in round 4", next.TxIDs, next.Round)
	}

	// 产生新提议只让余下交易转为等待投票：账户已确认序号仍为 3，
	// 已有确认块与旧轮候选结果保持原样。
	acct = n.Account(sender.pub)
	if acct.ConfirmedSequence != 3 {
		t.Fatalf("confirmed sequence = %d after new proposal, want 3", acct.ConfirmedSequence)
	}
	assertPending(t, acct, []string{id4, id5, id6}, []string{"waiting-vote", "waiting-vote", "waiting-vote"})
	if n.CurrentRound() != 4 || n.Height() != 3 {
		t.Fatalf("after new proposal round=%d height=%d, want 4/3", n.CurrentRound(), n.Height())
	}
	for h, want := range map[uint64]string{1: block1.ID, 2: block2.ID, 3: altID} {
		blk, err := n.BlockAt(h)
		if err != nil || blk.ID != want {
			t.Fatalf("confirmed history at height %d changed after new proposal: %+v", h, blk)
		}
	}
	localView = findCandidate(t, n, 3, local.BlockID)
	altView = findCandidate(t, n, 3, altID)
	if localView.Result != CandidateLost || altView.Result != CandidateWon {
		t.Fatalf("round 3 results changed after new proposal: local=%s alt=%s", localView.Result, altView.Result)
	}
}

// assertPending 断言账户待处理列表恰好为给定交易标识顺序，
// 每笔说明与 wantNotes 一一对应，且序号缺口为 0。
func assertPending(t *testing.T, acct *AccountInfo, wantIDs, wantNotes []string) {
	t.Helper()
	if len(acct.Pending) != len(wantIDs) {
		t.Fatalf("pending = %d entries, want %d: %+v", len(acct.Pending), len(wantIDs), acct.Pending)
	}
	for i, info := range acct.Pending {
		if info.ID != wantIDs[i] || info.Note != wantNotes[i] {
			t.Fatalf("pending[%d] = (%s, %s), want (%s, %s)", i, info.ID, info.Note, wantIDs[i], wantNotes[i])
		}
	}
	if acct.Gap != 0 {
		t.Fatalf("gap = %d, want 0 (sequences consecutive)", acct.Gap)
	}
}
