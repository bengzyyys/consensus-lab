package consensus

import (
	"fmt"
	"testing"
)

// 本文件钉住竞争候选登记对调用方入参列表的所有权回归保障：
// RegisterCandidate 成功后必须保留登记时刻的独立副本，调用方随后怎样复用、
// 编辑同一个切片（交换位置、改成池内其他有效交易、改成未知标识、制造重复标识）
// 都不能改变已登记候选的区块标识、交易列表、次序与已有票数；这些编辑只要没有
// 再次提交给登记功能，就不影响正常查询、合法投票与最终确认内容。
//
// 场景固定为：四名验证者（确认需严格超过 2/3，即至少 3 票）、单块上限四笔、
// 交易池不限容量，两个发送账户甲（keys[0]）与乙（keys[1]）。
//
//	甲 1 费用 10、甲 2 费用 40、甲 3 费用 1；乙 1 费用 30、乙 2 费用 5。
//
// 本地提议按既有费用与序号门限规则打包为 [乙1 甲1 甲2 乙2]，甲 3 因单块上限
// 留在池中、不被任何候选引用。竞争候选登记为交错列表 [甲1 乙1 甲2]：
// 两账户交易交错（甲、乙、甲），同一账户序号自已确认序号加一起连续递增
// （甲 1、2；乙 1），但次序既不同于本地提议，也不按费用排序（费用反而是
// 10、30、40 递增）。登记必须原样保留这个完整次序，且不推进轮次、确认高度
// 或账户已确认序号。
//
// 竞争候选先拿到一张尚不足以确认的票，随后调用方在原切片上连续做四类编辑。
// 只有把含未知标识、含重复标识的编辑列表再次提交登记时，才分别按既有的
// unknown-transaction 与 duplicate-in-list 拒绝；原候选内容与票数始终不变。
// 剩余名单内验证者继续向原区块标识投票，达到门槛时确认块与按高度查询到的
// 确认历史都采用原始列表；甲只推进到原候选实际包含的最后序号 2，乙推进到 1，
// 被调用方后来写进列表的甲 3 以及落选本地提议独有的乙 2 都不会得到确认。

// assertAltAndRoundIntact 在调用方私自编辑登记入参后，核对节点记录仍是登记时
// 的样子：轮次停在第 1 轮、确认高度为 0；候选集合恰好为本地提议与原候选两个，
// 原候选区块标识、完整交易次序、pending 结果与投票者集合不变；本地提议的
// 标识与交易列表也保持原样。wantOrder 为登记时的原始次序，voterPubs 为
// 原候选此刻应保留的全部投票者（顺序无关，函数内按公钥排序比较）。
func assertAltAndRoundIntact(t *testing.T, n *Node, local ProposalView, altID string, wantOrder []string, voterPubs ...[]byte) {
	t.Helper()
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round=%d height=%d, want 1/0: caller edits or rejected registers must not advance the round",
			n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may appear while the round is undecided")
	}
	p, ok := n.Proposal()
	if !ok || p.BlockID != local.BlockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(local.TxIDs) {
		t.Fatalf("local proposal changed: %+v ok=%v, want %+v", p, ok, local)
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd || !rc.HasRecords || len(rc.Candidates) != 2 {
		t.Fatalf("candidate set must stay exactly the original two: %+v", rc)
	}
	alt := candidateByID(t, rc, altID)
	if alt.Local || alt.Result != CandidatePending || alt.Round != 1 {
		t.Fatalf("original candidate changed: %+v", alt)
	}
	if fmt.Sprint(alt.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("original candidate order = %v, want registered order %v", alt.TxIDs, wantOrder)
	}
	if fmt.Sprint(pubHexList(alt.Voters)) != fmt.Sprint(sortedPubHex(voterPubs...)) {
		t.Fatalf("original candidate voters = %x, want %x", alt.Voters, voterPubs)
	}
}

// TestRegisterCandidateCopiesInputListThroughEditsAndConfirmation 覆盖完整的
// 入参复用回归：登记成功后调用方交换、改写同一个交易标识切片，已登记候选
// 必须保持登记时的内容；编辑不锁定额外交易、不增减候选；含未知或重复标识的
// 编辑列表再次登记仍被既有原因拒绝；未决出时的合法投票与达到门槛后的确认块、
// 确认历史、交易关联与账户序号推进全部以原始候选为准。
func TestRegisterCandidateCopiesInputListThroughEditsAndConfirmation(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	// 甲：序号 1（费 10）、2（费 40）、3（费 1）；乙：序号 1（费 30）、2（费 5）。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 40, 100)
	a3 := NewTransaction(keys[0].priv, 3, []byte("a3"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 30, 100)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 5, 100)
	for _, tx := range []*Transaction{a1, a2, a3, b1, b2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit %q: %v", tx.Content, err)
		}
	}
	idA1, idA2, idA3 := a1.ID(), a2.ID(), a3.ID()
	idB1, idB2 := b1.ID(), b2.ID()

	// 本地提议：首轮各账户下一条可确认交易比较费用，依次选出
	// 乙1(30)、甲1(10)、甲2(40)、乙2(5)，达上限四笔；甲 3 留在池中排队。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantLocal := []string{idB1, idA1, idA2, idB2}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("local proposal = %v, want %v", local.TxIDs, wantLocal)
	}

	// 竞争候选：两账户交错（甲、乙、甲），同账户序号连续（甲 1、2；乙 1），
	// 次序与本地提议不同，也不按费用排序（费用 10、30、40 递增）。
	origList := []string{idA1, idB1, idA2}
	wantID := BlockID(1, 1, "", origList)
	if wantID == local.BlockID {
		t.Fatal("setup: contending candidate must differ from local proposal")
	}
	res, err := n.RegisterCandidate(1, origList)
	if err != nil {
		t.Fatalf("interleaved contending candidate must register: %v", err)
	}
	if res.Existing || res.BlockID != wantID {
		t.Fatalf("register result = %+v, want fresh candidate %s", res, wantID)
	}
	altID := res.BlockID

	// 登记本身不推进轮次、确认高度或账户已确认序号，也不产生确认块。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("after register round=%d height=%d, want 1/0", n.CurrentRound(), n.Height())
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 1: %v, want %s", err, ReasonUnknownBlock)
	}

	// 原候选引用的甲1、甲2、乙1 与本地提议独有的乙2 都等待投票；
	// 甲3 未被任何候选引用，仍等待打包；账户说明与状态一致。
	for _, id := range []string{idA1, idA2, idB1, idB2} {
		if info, _ := n.Tx(id); info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed after register", id, info.Status)
		}
	}
	if info, _ := n.Tx(idA3); info.Status != StatusQueued || info.BlockHeight != 0 {
		t.Fatalf("unreferced a3 = %+v, want queued with no block", info)
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		idA1: "waiting-vote", idA2: "waiting-vote", idA3: "waiting-pack",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{
		idB1: "waiting-vote", idB2: "waiting-vote",
	})

	// 原候选稍后先取得一张尚不足以确认的票（四人名单需 3 票）；但登记之后、
	// 任何后续操作触发状态克隆之前，调用方先直接编辑原切片，此时节点内记录
	// 也必须已经与调用方切片脱钩。

	// —— 编辑一：直接交换原列表中已有项的位置（[甲1 乙1 甲2] -> [乙1 甲1 甲2]）。 ——
	origList[0], origList[1] = origList[1], origList[0]
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2})
	// 被交换的三笔交易仍等待投票，未被引用的甲3 仍等待打包。
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		idA1: "waiting-vote", idA2: "waiting-vote", idA3: "waiting-pack",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{
		idB1: "waiting-vote", idB2: "waiting-vote",
	})

	// —— 编辑二：把某个位置改成另一笔仍在池内、但此前未被任何候选引用的
	// 有效交易甲3（列表变为 [乙1 甲1 甲3]）。甲3 不会因此被锁定或确认。 ——
	origList[2] = idA3
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2})
	if info, _ := n.Tx(idA3); info.Status != StatusQueued {
		t.Fatalf("a3 mentioned only by the caller-edited slice status = %s, want queued", info.Status)
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		idA1: "waiting-vote", idA2: "waiting-vote", idA3: "waiting-pack",
	})
	if rc, _ := n.Candidates(1); len(rc.Candidates) != 2 {
		t.Fatalf("editing the input slice registered a new candidate: %+v", rc.Candidates)
	}

	// 原候选取得一张尚不足以确认的票；这张票在随后的全部编辑中都必须保留。
	if res, err := n.Vote(keys[1].pub, 1, altID); err != nil || !res.Counted || res.Confirmed {
		t.Fatalf("first alt vote must count without confirming: %+v %v", res, err)
	}

	// —— 编辑三：把原列表中的标识改成未知标识（[未知 甲1 甲3]）。
	// 编辑没有再次提交登记：查询原候选、向原区块标识投合法票都不能失败。 ——
	origList[0] = "deadbeef"
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2}, keys[1].pub)
	// 第二票仍合法计入但不足以确认（2 票未严格超过 4 人的 2/3）。
	vote2, err := n.Vote(keys[2].pub, 1, altID)
	if err != nil || !vote2.Counted || vote2.Confirmed {
		t.Fatalf("second legal vote after unknown-id edit: %+v %v", vote2, err)
	}
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2}, keys[1].pub, keys[2].pub)

	// 把含未知标识的编辑列表再次用于登记：仍按既有的 unknown-transaction
	// 拒绝，原候选内容、已有两张票与候选集合保持原样。
	if _, err := n.RegisterCandidate(1, origList); reason(err) != ReasonUnknownTx {
		t.Fatalf("resubmit list with unknown id got %v, want %s", err, ReasonUnknownTx)
	}
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2}, keys[1].pub, keys[2].pub)

	// —— 编辑四：让其中一个有效标识出现两次（列表变为 [甲1 甲1 甲3]）。 ——
	origList[0] = idA1
	origList[1] = idA1
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2}, keys[1].pub, keys[2].pub)
	// 含重复标识的编辑列表再次登记：仍按既有的 duplicate-in-list 拒绝。
	if _, err := n.RegisterCandidate(1, origList); reason(err) != ReasonDuplicateInList {
		t.Fatalf("resubmit duplicated list got %v, want %s", err, ReasonDuplicateInList)
	}
	assertAltAndRoundIntact(t, n, local, altID, []string{idA1, idB1, idA2}, keys[1].pub, keys[2].pub)
	// 甲3 只出现在被拒的编辑列表里，从未被登记，仍等待打包。
	if info, _ := n.Tx(idA3); info.Status != StatusQueued {
		t.Fatalf("a3 must stay queued after rejected registers: %+v", info)
	}

	// —— 门槛：名单内下一名验证者继续向原区块标识投第三票，立即确认。
	// 返回的确认块必须采用原始列表与顺序，而非调用方手中的任何编辑版本。 ——
	vote3, err := n.Vote(keys[3].pub, 1, altID)
	if err != nil || !vote3.Counted || !vote3.Confirmed || vote3.Block == nil {
		t.Fatalf("third legal vote must confirm the original candidate: %+v %v", vote3, err)
	}
	wantOrder := []string{idA1, idB1, idA2}
	blk := *vote3.Block
	if blk.Height != 1 || blk.Round != 1 || blk.ID != altID || blk.PreviousID != "" {
		t.Fatalf("confirming block header wrong: %+v", blk)
	}
	if fmt.Sprint(blk.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("confirming block txs = %v, want registered order %v", blk.TxIDs, wantOrder)
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("after confirm round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}

	// 按高度查询到的确认历史与最新块同样采用原始列表与顺序。
	hist, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if hist.ID != altID || hist.Round != 1 || hist.Height != 1 || hist.PreviousID != "" ||
		fmt.Sprint(hist.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("confirmed history wrong: %+v", hist)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != altID ||
		fmt.Sprint(latest.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("latest block wrong: %+v ok=%v, want %s %v", latest, ok, altID, wantOrder)
	}

	// 原候选实际包含的交易只确认一次并关联胜出块；调用方后来写入列表的甲3
	// 与落选本地提议独有的乙2 都不会得到确认，而是回到/留在排队状态。
	for _, id := range wantOrder {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != altID {
			t.Fatalf("winning tx %s must link to winning block: %+v", id, info)
		}
	}
	for _, id := range []string{idA3, idB2} {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued || info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("non-winning tx %s must stay queued without block link: %+v", id, info)
		}
	}

	// 各账户只推进到原候选实际包含的最后一个序号：甲到 2（不是被编辑列表
	// 提到的 3），乙到 1；剩余交易按序号等待下一轮打包，缺口为 0。
	assertAccountQueue(t, n, keys[0].pub, 2, map[string]string{idA3: "waiting-pack"})
	assertAccountQueue(t, n, keys[1].pub, 1, map[string]string{idB2: "waiting-pack"})

	// 旧轮结果：原候选胜出且保留登记时的列表与三名投票者；本地提议落选，
	// 保留自己原来的四笔交易列表与零票。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("round 1 record wrong after confirm: %+v", rc)
	}
	won := candidateByID(t, rc, altID)
	if won.Local || won.Result != CandidateWon || fmt.Sprint(won.TxIDs) != fmt.Sprint(wantOrder) {
		t.Fatalf("winning candidate record wrong: %+v", won)
	}
	if fmt.Sprint(pubHexList(won.Voters)) != fmt.Sprint(sortedPubHex(keys[1].pub, keys[2].pub, keys[3].pub)) {
		t.Fatalf("winning candidate voters = %x, want keys[1..3]", won.Voters)
	}
	lost := candidateByID(t, rc, local.BlockID)
	if !lost.Local || lost.Result != CandidateLost || fmt.Sprint(lost.TxIDs) != fmt.Sprint(wantLocal) {
		t.Fatalf("losing local candidate record wrong: %+v", lost)
	}
	if len(lost.Voters) != 0 {
		t.Fatalf("local candidate voters = %x, want none", lost.Voters)
	}
}
