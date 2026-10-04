package consensus

import (
	"fmt"
	"testing"
)

// TestConfirmAtExpiryRoundBoundary 固定“同一次确认”内各效果的先后顺序：
// 胜出候选的交易先确认（移出交易池、推进账户序号、关联确认块），
// 落选候选独有的交易随后回到排队，最后才进入下一轮并按新轮次处理到期。
//
// 场景：第 1 轮、四名验证者、单块上限 2。a 的序号 1 交易费用 10、到期轮次 2；
// b 的序号 1 交易费用 5、到期轮次 2，另有一笔序号 2、费用 100、到期轮次 100。
// 只含 a1 的竞争候选在第三票胜出并进入第 2 轮——恰好是两笔序号 1 交易的到期轮次。
// 此时必须保证：a1 已确认而不是过期，b1 过期且不再占池，b2 受序号缺口阻挡、
// 第 2 轮只能产生空块；候选历史仍保留胜出/落选结果与各自原交易顺序。
func TestConfirmAtExpiryRoundBoundary(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 2)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1-fee10-exp2"), 10, 2)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1-fee5-exp2"), 5, 2)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2-fee100-exp100"), 100, 100)
	for _, tx := range []*Transaction{a1, b1, b2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 本地提议按费用包含两笔序号 1 交易：a1(10)、b1(5)；b2 因 b1 尚未选入
	// 造成序号缺口，即使费用最高也只能留在池内排队。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("local proposal = %v, want [a1 b1]", local.TxIDs)
	}
	if b2Info, _ := n.Tx(b2.ID()); b2Info.Status != StatusQueued {
		t.Fatalf("b2 should stay queued while round 1 is open, got %s", b2Info.Status)
	}

	// 竞争候选只含费用 10 的 a1；它与本地提议共享 a1，b1 仅被本地提议引用。
	altID := registerMust(t, n, 1, []string{a1.ID()})
	if altID == local.BlockID {
		t.Fatal("test setup: competing candidate must differ from the local proposal")
	}

	// 前两票：票已计入，但两票恰好等于四人名单的三分之二，严格超过才能确认，
	// 故既不能确认也不能进入下一轮。
	for _, i := range []int{0, 1} {
		res, err := n.Vote(keys[i].pub, 1, altID)
		if err != nil || !res.Counted || res.Confirmed || res.Block != nil {
			t.Fatalf("vote %d must count without confirming: %+v %v", i+1, res, err)
		}
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, two votes must keep round 1 open", n.CurrentRound(), n.Height())
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended {
		t.Fatalf("round must stay pending before quorum: %+v", rc)
	}
	altView := findCandidate(t, n, 1, altID)
	localView := findCandidate(t, n, 1, local.BlockID)
	if altView.Result != CandidatePending || len(altView.Voters) != 2 {
		t.Fatalf("competing candidate wrong before quorum: %+v", altView)
	}
	if localView.Result != CandidatePending || len(localView.Voters) != 0 {
		t.Fatalf("local candidate wrong before quorum: %+v", localView)
	}
	if len(rc.Unvoted) != 2 {
		t.Fatalf("unvoted = %d, want 2", len(rc.Unvoted))
	}

	// 仍在第 1 轮：到期轮次为 2 的两笔交易都还有效，处于等待投票状态；
	// 账户已确认序号一律不得提前变化，b 账户待处理列表保持 [b1, b2] 且无缺口。
	for _, id := range []string{a1.ID(), b1.ID()} {
		info, _ := n.Tx(id)
		if info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed while round 1 is open", id, info.Status)
		}
	}
	if a := n.Account(keys[0].pub); a.ConfirmedSequence != 0 || len(a.Pending) != 1 ||
		a.Pending[0].ID != a1.ID() || a.Pending[0].Note != "waiting-vote" {
		t.Fatalf("account a before confirm wrong: %+v", a)
	}
	bAcct := n.Account(keys[1].pub)
	if bAcct.ConfirmedSequence != 0 || bAcct.Gap != 0 || len(bAcct.Pending) != 2 {
		t.Fatalf("account b before confirm wrong: %+v", bAcct)
	}
	if bAcct.Pending[0].ID != b1.ID() || bAcct.Pending[0].Status != StatusProposed ||
		bAcct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("b1 pending wrong before confirm: %+v", bAcct.Pending[0])
	}
	if bAcct.Pending[1].ID != b2.ID() || bAcct.Pending[1].Status != StatusQueued ||
		bAcct.Pending[1].Note != "waiting-pack" {
		t.Fatalf("b2 pending wrong before confirm: %+v", bAcct.Pending[1])
	}

	// 第三票：竞争候选胜出，立即确认并进入第 2 轮。
	res, err := n.Vote(keys[2].pub, 1, altID)
	if err != nil || !res.Counted || !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote should confirm the competing candidate: %+v %v", res, err)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 确认块来自胜出的竞争候选：它的区块标识、高度与交易顺序 [a1]。
	wantBlock := BlockID(1, 1, "", []string{a1.ID()})
	if wantBlock != altID {
		t.Fatalf("competing block id = %s, recomputed %s", altID, wantBlock)
	}
	if res.Block.ID != altID || res.Block.Height != 1 || res.Block.Round != 1 ||
		res.Block.PreviousID != "" || fmt.Sprint(res.Block.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("confirming vote returned wrong block: %+v", res.Block)
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != altID || blk.Height != 1 || blk.Round != 1 || blk.PreviousID != "" ||
		fmt.Sprint(blk.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("stored block wrong: %+v", blk)
	}

	// a1 虽同时出现在落选的本地提议中，但只确认一次：保留竞争候选对应的
	// 确认块标识与高度 1，进入到期轮次 2 也不能被改成过期；原交易内容保留。
	a1Info, err := n.Tx(a1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if a1Info.Status != StatusConfirmed || a1Info.BlockID != altID || a1Info.BlockHeight != 1 {
		t.Fatalf("winner tx a1 wrong at expiry boundary: %+v", a1Info)
	}
	if a1Info.Tx == nil || a1Info.Tx.ID() != a1.ID() || !a1Info.Tx.Verify() ||
		a1Info.Tx.Sequence != 1 || a1Info.Tx.Fee != 10 || a1Info.Tx.Expiry != 2 {
		t.Fatalf("winner tx content not preserved: %+v", a1Info.Tx)
	}
	aAcct := n.Account(keys[0].pub)
	if aAcct.ConfirmedSequence != 1 || aAcct.Gap != 0 || len(aAcct.Pending) != 0 {
		t.Fatalf("winner account wrong after confirm: %+v", aAcct)
	}

	// b1 只属于落选候选：先回池、再在同一次确认中因到期轮次 2 失效。
	// 必须显示 expired、保留原交易内容，不留任何确认块关联，也不推进账户序号。
	b1Info, err := n.Tx(b1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if b1Info.Status != StatusExpired {
		t.Fatalf("loser-only b1 status = %s, want expired after entering round 2", b1Info.Status)
	}
	if b1Info.BlockID != "" || b1Info.BlockHeight != 0 {
		t.Fatalf("expired b1 must keep no block association: %+v", b1Info)
	}
	if b1Info.Tx == nil || b1Info.Tx.ID() != b1.ID() || !b1Info.Tx.Verify() ||
		b1Info.Tx.Sequence != 1 || b1Info.Tx.Fee != 5 || b1Info.Tx.Expiry != 2 {
		t.Fatalf("expired b1 content not preserved: %+v", b1Info.Tx)
	}

	// b2 尚未到期：仍以 queued 留在待处理列表；b1 过期留下序号 1 的最早缺口。
	bAcct = n.Account(keys[1].pub)
	if bAcct.ConfirmedSequence != 0 || bAcct.Gap != 1 || len(bAcct.Pending) != 1 {
		t.Fatalf("account b after confirm wrong: %+v", bAcct)
	}
	if bAcct.Pending[0].ID != b2.ID() || bAcct.Pending[0].Status != StatusQueued ||
		bAcct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("b2 should remain queued waiting-pack: %+v", bAcct.Pending[0])
	}
	b2Info, _ := n.Tx(b2.ID())
	if b2Info.Status != StatusQueued || b2Info.BlockID != "" || b2Info.BlockHeight != 0 {
		t.Fatalf("b2 wrong after round advanced: %+v", b2Info)
	}

	// 候选历史：竞争候选胜出、本地提议落选，各自保留原来的交易顺序；
	// 历史中对已过期 b1 的引用不能把它重新变成等待投票。
	rc, _ = n.Candidates(1)
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("decided round record wrong: %+v", rc)
	}
	altView = findCandidate(t, n, 1, altID)
	localView = findCandidate(t, n, 1, local.BlockID)
	if altView.Local || altView.Result != CandidateWon || len(altView.Voters) != 3 ||
		fmt.Sprint(altView.TxIDs) != fmt.Sprint([]string{a1.ID()}) {
		t.Fatalf("winner history wrong: %+v", altView)
	}
	if !localView.Local || localView.Result != CandidateLost || len(localView.Voters) != 0 ||
		fmt.Sprint(localView.TxIDs) != fmt.Sprint([]string{a1.ID(), b1.ID()}) {
		t.Fatalf("loser history must keep original order and zero votes: %+v", localView)
	}
	if len(rc.Unvoted) != 1 || fmt.Sprintf("%x", rc.Unvoted[0]) != fmt.Sprintf("%x", keys[3].pub) {
		t.Fatalf("only keys[3] should remain unvoted: %x", rc.Unvoted)
	}
	// 历史查询只读：查完之后过期交易仍是过期，待处理列表仍只有 b2。
	if info, _ := n.Tx(b1.ID()); info.Status != StatusExpired {
		t.Fatalf("history reference revived expired b1: %s", info.Status)
	}
	if a := n.Account(keys[1].pub); len(a.Pending) != 1 || a.Pending[0].ID != b2.ID() {
		t.Fatalf("history reference put expired b1 back into the pool: %+v", a.Pending)
	}

	// b2 费用再高也不能越过序号 1 的缺口进入第 2 轮提议：本轮只能是空块，
	// 空块接在高度 1 的确认块之后、使用高度 2。
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Empty || len(p2.TxIDs) != 0 || p2.Round != 2 {
		t.Fatalf("round 2 proposal must be empty due to sequence gap, got %+v", p2)
	}
	if p2.BlockID != BlockID(2, 2, altID, []string{}) {
		t.Fatalf("round 2 empty block id = %s, want recomputed id over height 2", p2.BlockID)
	}
	if info, _ := n.Tx(b2.ID()); info.Status != StatusQueued {
		t.Fatalf("b2 must stay queued after empty round-2 proposal: %s", info.Status)
	}
	// 新轮次同样无法借候选登记把过期的 b1 拉回，或让 b2 跳过缺口。
	if _, err := n.RegisterCandidate(2, []string{b1.ID()}); reason(err) != ReasonTxNotInPool {
		t.Fatalf("registering expired b1 got %v, want %s", err, ReasonTxNotInPool)
	}
	if _, err := n.RegisterCandidate(2, []string{b2.ID()}); reason(err) != ReasonSequenceGap {
		t.Fatalf("registering gapped b2 got %v, want %s", err, ReasonSequenceGap)
	}

	// 落盘恢复后公开结果完全一致：一次确认内的先后顺序不依赖内存状态。
	n2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n2.CurrentRound() != 2 || n2.Height() != 1 {
		t.Fatalf("reopened round=%d height=%d, want 2/1", n2.CurrentRound(), n2.Height())
	}
	if info, _ := n2.Tx(a1.ID()); info.Status != StatusConfirmed ||
		info.BlockID != altID || info.BlockHeight != 1 {
		t.Fatalf("reopened winner tx wrong: %+v", info)
	}
	if info, _ := n2.Tx(b1.ID()); info.Status != StatusExpired ||
		info.BlockID != "" || info.BlockHeight != 0 {
		t.Fatalf("reopened expired b1 wrong: %+v", info)
	}
	if a := n2.Account(keys[0].pub); a.ConfirmedSequence != 1 || len(a.Pending) != 0 {
		t.Fatalf("reopened winner account wrong: %+v", a)
	}
	if a := n2.Account(keys[1].pub); a.ConfirmedSequence != 0 || a.Gap != 1 ||
		len(a.Pending) != 1 || a.Pending[0].ID != b2.ID() || a.Pending[0].Status != StatusQueued {
		t.Fatalf("reopened loser account wrong: %+v", a)
	}
	if p2restored, ok := n2.Proposal(); !ok || p2restored.BlockID != p2.BlockID || !p2restored.Empty {
		t.Fatalf("reopened round-2 empty proposal wrong: %+v ok=%v", p2restored, ok)
	}
	rc2, err := n2.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rc2.Candidates {
		want := CandidateLost
		if c.BlockID == altID {
			want = CandidateWon
		}
		if c.Result != want {
			t.Fatalf("reopened candidate %s result = %s, want %s", c.BlockID, c.Result, want)
		}
	}
}
