package consensus

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件保障“提议结果独立性”：Propose 首次产生提议、同轮再次请求提议、
// 以及 Proposal 直接读取已有提议，三个入口返回的都是取得时的独立结果。
// 本地程序拿到提议后可能调整交易顺序或保留一份供后续比较；调用方编辑自己
// 手中的结果（交换交易次序、改成未知交易标识、增减列表项、改写区块标识），
// 只能影响这一份数据，不能改变本轮已冻结的本地提议，也不能成为修改节点
// 状态的入口。打包顺序与投票确认规则保持既有行为。

// tamperProposalView 以调用方身份尽力改写手中的提议结果：
// 交换交易次序、把其中一项改成未知交易标识、增删列表项、
// 改写区块标识、轮次与空块标记。
func tamperProposalView(p *ProposalView, forgedTxID, forgedBlockID string) {
	if len(p.TxIDs) >= 2 {
		p.TxIDs[0], p.TxIDs[1] = p.TxIDs[1], p.TxIDs[0]
	}
	if len(p.TxIDs) >= 1 {
		p.TxIDs[0] = forgedTxID
	}
	p.TxIDs = append(p.TxIDs, forgedTxID) // 增项
	p.TxIDs = p.TxIDs[:len(p.TxIDs)-1]    // 减项
	p.BlockID = forgedBlockID
	p.Round += 100
	p.Empty = !p.Empty
}

// assertProposalView 断言一份提议结果与期望的轮次、区块标识、交易顺序和空块标记一致。
func assertProposalView(t *testing.T, p ProposalView, round uint64, blockID string, want []string, empty bool) {
	t.Helper()
	if p.Round != round || p.BlockID != blockID || p.Empty != empty || fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal = {round:%d block:%s txs:%v empty:%v}, want {round:%d block:%s txs:%v empty:%v}",
			p.Round, p.BlockID, p.TxIDs, p.Empty, round, blockID, want, empty)
	}
}

// 多交易提议：三个入口（首次 Propose、同轮再次 Propose、Proposal 读取）返回的
// 结果都可安全编辑。调用方改写后，节点保存的交易集合、次序与区块标识不变；
// 先取得且未编辑的结果保持取得时的轮次与内容；候选查询中本地提议仍按原顺序
// 等待投票；伪造标识不会成为池内交易或已登记候选；对原区块标识正常投票确认后，
// 确认块、交易关联与账户已确认序号都按真实选入的交易推进。
func TestProposalResultIsIndependentCopy(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	txA := NewTransaction(keys[0].priv, 1, []byte("alpha"), 3, 100)
	txB := NewTransaction(keys[1].priv, 1, []byte("beta"), 7, 100)
	txC := NewTransaction(keys[2].priv, 1, []byte("gamma"), 5, 100)
	for _, tx := range []*Transaction{txA, txB, txC} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{txB.ID(), txC.ID(), txA.ID()} // 费用 7、5、3
	wantBlockID := BlockID(1, 1, "", want)

	// 三个入口各取得一份结果：首次产生提议、同轮再次请求、直接读取已有提议。
	first, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	second, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	third, ok := n.Proposal()
	if !ok {
		t.Fatal("proposal should exist after Propose")
	}
	for i, p := range []ProposalView{first, second, third} {
		t.Run(fmt.Sprintf("entry-%d", i), func(t *testing.T) {
			assertProposalView(t, p, 1, wantBlockID, want, false)
		})
	}

	// 调用方尽力改写首次 Propose 与 Proposal 读取取得的两份结果；
	// 同轮再次 Propose 取得的 second 保持不编辑。
	forgedTxID := strings.Repeat("ab", 32)
	forgedBlockID := BlockID(1, 1, "", []string{forgedTxID})
	if forgedBlockID == wantBlockID {
		t.Fatal("setup: forged block id must differ from the real one")
	}
	tamperProposalView(&first, forgedTxID, forgedBlockID)
	tamperProposalView(&third, forgedTxID, forgedBlockID)

	// 未编辑的 second 保留取得时的轮次、区块标识、交易顺序和空块标记。
	assertProposalView(t, second, 1, wantBlockID, want, false)

	// 之后再次读取提议（两个入口），仍得到原来的内容。
	again, ok := n.Proposal()
	if !ok {
		t.Fatal("proposal should still exist")
	}
	assertProposalView(t, again, 1, wantBlockID, want, false)
	repropose, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, repropose, 1, wantBlockID, want, false)

	// 本轮候选查询中，本地提议仍包含原来的交易、按原来的顺序等待投票。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1 (forged ids must not register)", len(rc.Candidates))
	}
	cand := rc.Candidates[0]
	if !cand.Local || cand.BlockID != wantBlockID || fmt.Sprint(cand.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("local candidate changed by caller edit: %+v", cand)
	}
	if cand.Result != CandidatePending || len(cand.Voters) != 0 || len(rc.Unvoted) != 4 {
		t.Fatalf("candidate voting state changed by caller edit: %+v unvoted=%d", cand, len(rc.Unvoted))
	}
	// 调用方伪造的标识没有变成池内交易。
	if _, err := n.Tx(forgedTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("forged tx id got %v, want %s", err, ReasonUnknownTx)
	}

	// 向伪造的区块标识投票：按已有规则以 wrong-block-id 拒绝，且不计入真实候选的票数。
	if _, err := n.Vote(keys[0].pub, 1, forgedBlockID); reason(err) != ReasonWrongBlock {
		t.Fatalf("vote to forged block id got %v, want %s", err, ReasonWrongBlock)
	}
	rc, err = n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates[0].Voters) != 0 || len(rc.Unvoted) != 4 {
		t.Fatalf("rejected vote leaked into real candidate: voters=%v unvoted=%d",
			rc.Candidates[0].Voters, len(rc.Unvoted))
	}
	// 被拒绝的投票不占用该验证者的选择：同一验证者仍可正常投给真实候选。
	res, err := n.Vote(keys[0].pub, 1, wantBlockID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Counted || res.Confirmed {
		t.Fatalf("first real vote = %+v, want counted and not yet confirmed", res)
	}

	// 对原来的区块标识正常投票并达到确认条件。
	var confirmed *VoteResult
	for i := 1; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, wantBlockID)
		if err != nil {
			t.Fatal(err)
		}
		confirmed = res
	}
	if confirmed == nil || !confirmed.Confirmed || confirmed.Block == nil {
		t.Fatalf("third vote should confirm, got %+v", confirmed)
	}
	// 生成的确认块仍使用该标识和原始交易顺序。
	if confirmed.Block.ID != wantBlockID || fmt.Sprint(confirmed.Block.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("confirmed block = %+v, want id %s txs %v", confirmed.Block, wantBlockID, want)
	}
	b, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != wantBlockID || b.Height != 1 || b.Round != 1 || fmt.Sprint(b.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("block at height 1 = %+v, want id %s txs %v", b, wantBlockID, want)
	}
	// 交易查询关联到实际确认块，账户已确认序号按真实选入的交易推进。
	for i, tx := range []*Transaction{txA, txB, txC} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != wantBlockID {
			t.Fatalf("tx %d info = %+v, want confirmed in block %s at height 1", i, info, wantBlockID)
		}
		acct := n.Account(tx.Sender)
		if acct.ConfirmedSequence != 1 || len(acct.Pending) != 0 {
			t.Fatalf("account %d = %+v, want confirmed sequence 1 and no pending", i, acct)
		}
	}

	// 进入下一轮后：先前取得且未编辑的结果仍保留取得时的轮次和交易内容，
	// 不随节点当前提议的变化而变化。
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2", n.CurrentRound())
	}
	assertProposalView(t, second, 1, wantBlockID, want, false)
	// 当前轮的查询继续按已有规则返回：新一轮尚未提议，旧结果不是修改入口。
	if _, ok := n.Proposal(); ok {
		t.Fatal("round 2 should have no proposal yet")
	}
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("candidates of round 2 got %v, want %s", err, ReasonUnknownRound)
	}
	// 历史轮次保留原记录：本地提议已胜出，交易顺序不变。
	hist, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Candidates) != 1 || hist.Candidates[0].Result != CandidateWon ||
		fmt.Sprint(hist.Candidates[0].TxIDs) != fmt.Sprint(want) {
		t.Fatalf("round 1 history = %+v, want won candidate with txs %v", hist.Candidates, want)
	}
	// 新一轮正常产生提议（池中无交易，为空块），与旧结果互不影响。
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, p2, 2, BlockID(2, 2, wantBlockID, []string{}), []string{}, true)
	assertProposalView(t, second, 1, wantBlockID, want, false)
}

// 空块提议同样受保障：调用方向返回的空交易列表追加标识、改写区块标识后，
// 重新读取仍显示空块；伪造标识不进入池也不成为候选；向伪造标识投票被拒绝；
// 正常确认得到的块仍没有交易。尚未产生提议时 Proposal 返回不存在，
// 读取本身不创建候选。
func TestEmptyProposalResultIsIndependentCopy(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 尚未产生提议：Proposal 返回不存在，且读取本身不创建候选。
	if _, ok := n.Proposal(); ok {
		t.Fatal("proposal should not exist before Propose")
	}
	if _, ok := n.Proposal(); ok {
		t.Fatal("repeated read should still report no proposal")
	}
	if _, err := n.Candidates(1); reason(err) != ReasonUnknownRound {
		t.Fatalf("candidates before propose got %v, want %s", err, ReasonUnknownRound)
	}

	// 产生空块提议。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantBlockID := BlockID(1, 1, "", []string{})
	assertProposalView(t, p, 1, wantBlockID, []string{}, true)

	// 调用方向返回的空交易列表追加标识，并改写区块标识与空块标记。
	forgedTxID := strings.Repeat("cd", 32)
	forgedBlockID := BlockID(1, 1, "", []string{forgedTxID})
	p.TxIDs = append(p.TxIDs, forgedTxID)
	p.BlockID = forgedBlockID
	p.Empty = false

	// 重新读取仍显示空块：原轮次、原区块标识、空交易列表、空块标记。
	again, ok := n.Proposal()
	if !ok {
		t.Fatal("proposal should exist")
	}
	assertProposalView(t, again, 1, wantBlockID, []string{}, true)
	repropose, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, repropose, 1, wantBlockID, []string{}, true)

	// 追加的伪造标识没有变成池内交易或已登记候选。
	if _, err := n.Tx(forgedTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("forged tx id got %v, want %s", err, ReasonUnknownTx)
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 || len(rc.Candidates[0].TxIDs) != 0 || rc.Candidates[0].BlockID != wantBlockID {
		t.Fatalf("candidates changed by caller edit: %+v", rc.Candidates)
	}

	// 向伪造的区块标识投票以 wrong-block-id 拒绝；正常确认空块仍没有交易。
	if _, err := n.Vote(keys[0].pub, 1, forgedBlockID); reason(err) != ReasonWrongBlock {
		t.Fatalf("vote to forged block id got %v, want %s", err, ReasonWrongBlock)
	}
	confirmByVotes(t, n, keys, wantBlockID)
	b, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != wantBlockID || len(b.TxIDs) != 0 {
		t.Fatalf("confirmed empty block = %+v, want id %s with no transactions", b, wantBlockID)
	}
}
