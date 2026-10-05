package consensus

import (
	"fmt"
	"testing"
)

// 本文件回归保障：Propose（首次产生与同轮重复请求）与 Proposal（直接读取）
// 返回的都是提议冻结时的独立结果。调用方交换交易次序、把交易标识改成未知标识、
// 增减列表项、改写轮次/区块标识/空块标记，都只能影响自己手中的这一份数据，
// 不能改变本轮已冻结的本地提议、候选集合、投票与确认结果。

const (
	// fakeTxID 是测试中调用方伪造的交易标识：从未提交进交易池。
	fakeTxID = "fake-tx-id-never-submitted"
	// fakeBlockID 是测试中调用方伪造的区块标识：从未登记为候选。
	fakeBlockID = "fake-block-id-never-registered"
)

// assertProposalView 校验提议视图与冻结时的取值完全一致：
// 轮次、区块标识、交易顺序与空块标记。
func assertProposalView(t *testing.T, p ProposalView, round uint64, blockID string, txIDs []string) {
	t.Helper()
	if p.Round != round || p.BlockID != blockID || fmt.Sprint(p.TxIDs) != fmt.Sprint(txIDs) {
		t.Fatalf("proposal = round:%d block:%s empty:%v tx:%v; want round:%d block:%s empty:%v tx:%v",
			p.Round, p.BlockID, p.Empty, p.TxIDs, round, blockID, len(txIDs) == 0, txIDs)
	}
	if p.Empty != (len(txIDs) == 0) {
		t.Fatalf("proposal empty flag = %v, want %v", p.Empty, len(txIDs) == 0)
	}
}

// candidateByBlockID 在一轮候选视图中按区块标识查找候选，找不到返回 nil。
func candidateByBlockID(rc *RoundCandidates, blockID string) *CandidateView {
	for i := range rc.Candidates {
		if rc.Candidates[i].BlockID == blockID {
			return &rc.Candidates[i]
		}
	}
	return nil
}

// tamperProposalView 以调用方身份尽力改写手中的提议视图：
// 交换前两项次序、追加未知交易标识、改写轮次与区块标识、翻转空块标记。
func tamperProposalView(p *ProposalView) {
	if len(p.TxIDs) >= 2 {
		p.TxIDs[0], p.TxIDs[1] = p.TxIDs[1], p.TxIDs[0]
	}
	p.TxIDs = append(p.TxIDs, fakeTxID)
	p.Round += 70000
	p.BlockID = fakeBlockID
	p.Empty = !p.Empty
}

// setupProposalCopyNode 准备一个含三笔交易的节点：甲序号 1 费用 30、
// 甲序号 2 费用 100、乙序号 1 费用 50。按跨账户打包规则，本地提议的
// 固定顺序为 [乙1, 甲1, 甲2]，上限 4 时全部选入。
func setupProposalCopyNode(t *testing.T) (*Node, string, []testKey, []string) {
	t.Helper()
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 4)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 30, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 100, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	for _, tx := range []*Transaction{a1, a2, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	return n, dir, keys, []string{b1.ID(), a1.ID(), a2.ID()}
}

// 三个入口——首次 Propose、同轮再次 Propose、直接读取 Proposal——返回的
// 都是取得时的独立结果：编辑其中一份只能影响这一份数据；本轮已冻结的本地
// 提议的交易集合、次序、区块标识与空块标记保持原样，先前取得未编辑的结果
// 也保持原样，之后再次读取仍得到冻结时的提议。
func TestProposalResultsAreIndependentCopies(t *testing.T) {
	n, dir, keys, want := setupProposalCopyNode(t)
	wantBlockID := BlockID(1, 1, "", want)

	// 三个入口各取得一份结果。
	first, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, first, 1, wantBlockID, want)
	second, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, second, 1, wantBlockID, want)
	third, ok := n.Proposal()
	if !ok {
		t.Fatal("Proposal should exist after Propose")
	}
	assertProposalView(t, third, 1, wantBlockID, want)

	// 尽力改写第一份：交换次序、追加未知标识、改轮次与区块标识、翻转空块标记。
	tamperProposalView(&first)
	// 另外两份已取得但未编辑的结果保持冻结时的原样。
	assertProposalView(t, second, 1, wantBlockID, want)
	assertProposalView(t, third, 1, wantBlockID, want)

	// 再把第二份改成未知标识并缩短长度，第三份与之后新取得的结果仍是原提议。
	second.TxIDs[0] = fakeTxID
	second.TxIDs = second.TxIDs[:1]
	assertProposalView(t, third, 1, wantBlockID, want)
	fourth, ok := n.Proposal()
	if !ok {
		t.Fatal("Proposal should still exist")
	}
	assertProposalView(t, fourth, 1, wantBlockID, want)
	fifth, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, fifth, 1, wantBlockID, want)

	// 节点保存的原始交易仍按原顺序等待投票，伪造标识不在交易池中。
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		want[1]: "waiting-vote", want[2]: "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{
		want[0]: "waiting-vote",
	})
	if _, err := n.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx lookup got %v, want %s", err, ReasonUnknownTx)
	}

	// 登记一个合法竞争候选（甲1、乙1，顺序与本地提议不同）不改变本地提议；
	// 调用方伪造的交易标识不能登记成候选，伪造的区块标识也不是候选。
	alt, err := n.RegisterCandidate(1, []string{want[1], want[0]})
	if err != nil {
		t.Fatal(err)
	}
	if alt.Existing || alt.BlockID == wantBlockID {
		t.Fatalf("competing candidate registration wrong: %+v", alt)
	}
	if _, err := n.RegisterCandidate(1, []string{fakeTxID}); reason(err) != ReasonUnknownTx {
		t.Fatalf("register fake tx id got %v, want %s", err, ReasonUnknownTx)
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	local := candidateByBlockID(rc, wantBlockID)
	if local == nil || !local.Local || local.Result != CandidatePending ||
		fmt.Sprint(local.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("local candidate changed by caller edits: %+v", local)
	}
	if c := candidateByBlockID(rc, alt.BlockID); c == nil || c.Local ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint([]string{want[1], want[0]}) || c.Result != CandidatePending {
		t.Fatalf("competing candidate wrong after caller edits: %+v", c)
	}
	if candidateByBlockID(rc, fakeBlockID) != nil {
		t.Fatal("fake block id must not be a registered candidate")
	}

	// 对原区块标识正常投票：前两票计入但不确认。
	r0, err := n.Vote(keys[0].pub, 1, wantBlockID)
	if err != nil {
		t.Fatal(err)
	}
	if !r0.Counted || r0.Confirmed {
		t.Fatalf("first vote = %+v, want counted without confirm", r0)
	}
	if _, err := n.Vote(keys[1].pub, 1, wantBlockID); err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(mustCandidates(t, n, 1), wantBlockID); len(c.Voters) != 2 {
		t.Fatalf("local candidate votes = %d, want 2", len(c.Voters))
	}

	// 向手中结果里被改写的区块标识投票：按已有规则以 wrong-block-id 拒绝，
	// 不占用该验证者本轮的投票，也不计入真实候选的票数。
	if _, err := n.Vote(keys[2].pub, 1, fakeBlockID); reason(err) != ReasonWrongBlock {
		t.Fatalf("vote for fake block id got %v, want %s", err, ReasonWrongBlock)
	}
	if c := candidateByBlockID(mustCandidates(t, n, 1), wantBlockID); len(c.Voters) != 2 {
		t.Fatalf("rejected vote must not count: local candidate votes = %d, want 2", len(c.Voters))
	}

	// 被拒绝的验证者仍可对真实候选投票，第三票严格超过 2/3 并立即确认。
	res, err := n.Vote(keys[2].pub, 1, wantBlockID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote = %+v, want confirmation", res)
	}

	// 确认块仍使用原来的区块标识与原始交易顺序，接在空的前块标识之后。
	assertBlock(t, *res.Block, want, 1, 1, wantBlockID, "")
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("state after confirm: round=%d height=%d, want round=2 height=1",
			n.CurrentRound(), n.Height())
	}
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, blk, want, 1, 1, wantBlockID, "")
	latest, ok := n.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist")
	}
	assertBlock(t, latest, want, 1, 1, wantBlockID, "")

	// 交易查询关联到实际确认块；账户已确认序号按真实选入的交易推进。
	for _, id := range want {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != wantBlockID {
			t.Fatalf("tx %s confirmation link wrong: %+v", id, info)
		}
	}
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 2 || len(acct.Pending) != 0 {
		t.Fatalf("sender a confirmed seq = %d pending = %d, want 2/0", acct.ConfirmedSequence, len(acct.Pending))
	}
	if acct := n.Account(keys[1].pub); acct.ConfirmedSequence != 1 || len(acct.Pending) != 0 {
		t.Fatalf("sender b confirmed seq = %d pending = %d, want 1/0", acct.ConfirmedSequence, len(acct.Pending))
	}

	// 候选结果：本地提议按原顺序胜出，竞争候选落选，轮次确认为投票确认。
	rcEnd, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(rcEnd, wantBlockID); c == nil || c.Result != CandidateWon || !c.Local ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("winner candidate wrong: %+v", c)
	}
	if c := candidateByBlockID(rcEnd, alt.BlockID); c == nil || c.Result != CandidateLost {
		t.Fatalf("competing candidate should be lost: %+v", c)
	}
	if !rcEnd.Ended || rcEnd.UnconfirmedEnd {
		t.Fatalf("round 1 ending wrong: ended=%v unconfirmed=%v", rcEnd.Ended, rcEnd.UnconfirmedEnd)
	}

	// 早先取得、从未编辑的那份结果始终保持冻结时的原样。
	assertProposalView(t, third, 1, wantBlockID, want)

	// 确认时的保存发生在调用方篡改之后：重开状态目录，篡改的标识与顺序
	// 都不能落盘，确认历史仍是原来的区块标识与交易顺序。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, stored, want, 1, 1, wantBlockID, "")
	rcStored, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(rcStored, wantBlockID); c == nil || c.Result != CandidateWon ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("stored winner wrong after reopen: %+v", c)
	}
	if candidateByBlockID(rcStored, fakeBlockID) != nil {
		t.Fatal("fake block id must not be persisted as a candidate")
	}
	if _, err := reopened.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx id persisted: %v", err)
	}
}

// mustCandidates 查询本轮候选，查询失败时终止测试。
func mustCandidates(t *testing.T, n *Node, round uint64) *RoundCandidates {
	t.Helper()
	rc, err := n.Candidates(round)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// 各类编辑方式分别回归：交换次序、单项改成未知标识、删除/插入/追加列表项、
// 清空列表、翻转空块标记、改写轮次与区块标识。每一种修改后，三个入口再次
// 读取都仍返回冻结的原提议，本地候选保持原交易集合、顺序与区块标识等待投票，
// 伪造标识既不是池内交易，也不能登记为候选，原始交易仍处于等待投票状态。
func TestProposalResultEditVariantsStayLocal(t *testing.T) {
	variants := []struct {
		name string
		edit func(p *ProposalView)
	}{
		{"swap order", func(p *ProposalView) {
			p.TxIDs[0], p.TxIDs[2] = p.TxIDs[2], p.TxIDs[0]
		}},
		{"replace item with unknown id", func(p *ProposalView) {
			p.TxIDs[1] = fakeTxID
		}},
		{"remove item", func(p *ProposalView) {
			p.TxIDs = p.TxIDs[:1]
		}},
		{"append unknown id", func(p *ProposalView) {
			p.TxIDs = append(p.TxIDs, fakeTxID)
		}},
		{"insert unknown id at front", func(p *ProposalView) {
			p.TxIDs = append([]string{fakeTxID}, p.TxIDs...)
		}},
		{"clear list", func(p *ProposalView) {
			p.TxIDs = nil
		}},
		{"flip empty flag", func(p *ProposalView) {
			p.Empty = true
		}},
		{"rewrite round and block id", func(p *ProposalView) {
			p.Round = 999
			p.BlockID = fakeBlockID
		}},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			n, _, _, want := setupProposalCopyNode(t)
			wantBlockID := BlockID(1, 1, "", want)

			p, err := n.Propose()
			if err != nil {
				t.Fatal(err)
			}
			// 另取一份结果保留在手中：它不应被本次编辑波及。
			held, ok := n.Proposal()
			if !ok {
				t.Fatal("Proposal should exist")
			}

			v.edit(&p)

			// 直接读取、同轮再次请求与先取得未编辑的那份，全部仍是原提议。
			again, ok := n.Proposal()
			if !ok {
				t.Fatal("Proposal should still exist")
			}
			assertProposalView(t, again, 1, wantBlockID, want)
			repeated, err := n.Propose()
			if err != nil {
				t.Fatal(err)
			}
			assertProposalView(t, repeated, 1, wantBlockID, want)
			assertProposalView(t, held, 1, wantBlockID, want)

			// 本地候选的交易集合、顺序与区块标识不变，仍处于等待投票状态。
			c := candidateByBlockID(mustCandidates(t, n, 1), wantBlockID)
			if c == nil || !c.Local || c.Result != CandidatePending ||
				fmt.Sprint(c.TxIDs) != fmt.Sprint(want) {
				t.Fatalf("local candidate changed by %q edit: %+v", v.name, c)
			}
			// 伪造标识既不是池内交易，也不能借调用方手中的列表登记为候选。
			if _, err := n.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
				t.Fatalf("fake tx lookup after %q edit got %v, want %s", v.name, err, ReasonUnknownTx)
			}
			if _, err := n.RegisterCandidate(1, []string{fakeTxID}); reason(err) != ReasonUnknownTx {
				t.Fatalf("register fake tx after %q edit got %v, want %s", v.name, err, ReasonUnknownTx)
			}
			// 原始交易仍属于本轮本地提议、等待投票。
			for _, id := range want {
				info, err := n.Tx(id)
				if err != nil {
					t.Fatalf("tx %s lookup after %q edit: %v", id, v.name, err)
				}
				if info.Status != StatusProposed {
					t.Fatalf("tx %s status after %q edit = %s, want %s", id, v.name, info.Status, StatusProposed)
				}
			}
		})
	}
}

// 空块提议同样返回独立结果：调用方向空交易列表追加标识、翻转空块标记、
// 改写区块标识后，再次读取仍显示零交易空块；候选查询中的本地候选仍是
// pending 空块；对原区块标识正常投票确认，得到的确认块仍没有交易，
// 且篡改不会随确认保存落盘。
func TestEmptyProposalResultEditStaysEmpty(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 4)

	first, err := n.Propose() // 空池只能产生空块。
	if err != nil {
		t.Fatal(err)
	}
	wantBlockID := BlockID(1, 1, "", []string{})
	assertProposalView(t, first, 1, wantBlockID, []string{})
	second, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	third, ok := n.Proposal()
	if !ok {
		t.Fatal("empty Proposal should exist")
	}

	// 调用方向返回的空列表追加标识，并翻转空块标记、改写区块标识与轮次。
	first.TxIDs = append(first.TxIDs, fakeTxID)
	first.Empty = false
	first.BlockID = fakeBlockID
	first.Round = 888

	// 其他已取得的结果与之后再次读取的结果仍是零交易空块。
	assertProposalView(t, second, 1, wantBlockID, []string{})
	assertProposalView(t, third, 1, wantBlockID, []string{})
	fourth, ok := n.Proposal()
	if !ok {
		t.Fatal("empty Proposal should still exist")
	}
	assertProposalView(t, fourth, 1, wantBlockID, []string{})
	repeated, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposalView(t, repeated, 1, wantBlockID, []string{})

	// 候选查询中的本地候选仍是零交易空块、等待投票；伪造标识既非交易也非候选。
	c := candidateByBlockID(mustCandidates(t, n, 1), wantBlockID)
	if c == nil || !c.Local || c.Result != CandidatePending || len(c.TxIDs) != 0 {
		t.Fatalf("empty local candidate wrong after edit: %+v", c)
	}
	if _, err := n.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx lookup got %v, want %s", err, ReasonUnknownTx)
	}
	if _, err := n.RegisterCandidate(1, []string{fakeTxID}); reason(err) != ReasonUnknownTx {
		t.Fatalf("register fake tx got %v, want %s", err, ReasonUnknownTx)
	}
	if candidateByBlockID(mustCandidates(t, n, 1), fakeBlockID) != nil {
		t.Fatal("fake block id must not be registered as candidate")
	}

	// 向伪造的区块标识投票按已有规则拒绝，不计票也不锁定验证者。
	if _, err := n.Vote(keys[0].pub, 1, fakeBlockID); reason(err) != ReasonWrongBlock {
		t.Fatalf("vote fake empty-block id got %v, want %s", err, ReasonWrongBlock)
	}

	// 对原空块标识投票达到现有确认条件，确认块仍是零交易的空块。
	confirmByVotes(t, n, keys, wantBlockID)
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.TxIDs) != 0 || blk.ID != wantBlockID || blk.Height != 1 || blk.Round != 1 {
		t.Fatalf("confirmed empty block wrong: %+v", blk)
	}
	if n.Height() != 1 {
		t.Fatalf("height = %d, want 1", n.Height())
	}

	// 重开后历史仍是零交易空块，伪造标识没有落盘。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.TxIDs) != 0 || stored.ID != wantBlockID {
		t.Fatalf("empty block changed after reopen: %+v", stored)
	}
	rc, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(rc, wantBlockID); c == nil || c.Result != CandidateWon || len(c.TxIDs) != 0 {
		t.Fatalf("stored empty candidate wrong: %+v", c)
	}
	if candidateByBlockID(rc, fakeBlockID) != nil {
		t.Fatal("fake block id must not persist as candidate")
	}
	if _, err := reopened.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx id persisted: %v", err)
	}
}

// 尚未产生提议时 Proposal 始终返回不存在；读取本身不能创建候选或轮次记录，
// 不能把交易悄悄标记为已提议，也不能冻结一个空提议。之后首次 Propose
// 仍按当前交易池与既有打包规则正常产生提议。
func TestProposalAbsentBeforeProposeDoesNotCreateCandidate(t *testing.T) {
	t.Run("with queued transactions", func(t *testing.T) {
		keys := genKeys(t, 4)
		n, _ := newTestNode(t, keys, 4)
		tx := NewTransaction(keys[0].priv, 1, []byte("queued-before-propose"), 5, 100)
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}

		// 多次读取都返回不存在。
		for i := 0; i < 2; i++ {
			if p, ok := n.Proposal(); ok {
				t.Fatalf("Proposal must be absent before Propose (read %d): %+v", i+1, p)
			}
		}
		// 读取没有创建轮次候选记录。
		if _, err := n.Candidates(1); reason(err) != ReasonUnknownRound {
			t.Fatalf("Candidates after absent read got %v, want %s", err, ReasonUnknownRound)
		}
		// 尚无提议期间登记候选仍按原规则拒绝。
		if _, err := n.RegisterCandidate(1, []string{tx.ID()}); reason(err) != ReasonNoProposal {
			t.Fatalf("register before Propose got %v, want %s", err, ReasonNoProposal)
		}
		// 交易仍在排队等待打包，没有被读取操作提议。
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued {
			t.Fatalf("status after absent reads = %s, want %s", info.Status, StatusQueued)
		}
		assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{tx.ID(): "waiting-pack"})

		// 之后首次 Propose 正常打包池内真实交易，空提议没有被提前冻结。
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		assertProposalView(t, p, 1, BlockID(1, 1, "", []string{tx.ID()}), []string{tx.ID()})
	})

	t.Run("empty pool", func(t *testing.T) {
		keys := genKeys(t, 4)
		n, _ := newTestNode(t, keys, 4)
		if _, ok := n.Proposal(); ok {
			t.Fatal("Proposal must be absent on a fresh empty node")
		}
		if _, err := n.Candidates(1); reason(err) != ReasonUnknownRound {
			t.Fatalf("Candidates on fresh node got %v, want %s", err, ReasonUnknownRound)
		}
		// 读取之后再 Propose，仍正常产生空块。
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		assertProposalView(t, p, 1, BlockID(1, 1, "", []string{}), []string{})
	})
}

// 提议结果是取得时的快照：第一轮取得结果并保留到进入第二轮后，它仍保留
// 取得时的轮次、区块标识与交易内容，不随节点当前提议变化；在新轮次编辑
// 这份旧结果，既改不了第一轮已确认的历史，也改不了当前轮的提议与候选，
// 当前轮查询与投票继续按已有规则进行。
func TestProposalResultIsSnapshotAcrossRounds(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	// 第一轮：乙1 费用 20 高于甲1 费用 10，提议顺序 [乙1, 甲1]。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 20, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	want1 := []string{b1.ID(), a1.ID()}
	first, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	id1 := BlockID(1, 1, "", want1)
	assertProposalView(t, first, 1, id1, want1)

	confirmByVotes(t, n, keys, id1)
	if n.CurrentRound() != 2 {
		t.Fatalf("round after confirm = %d, want 2", n.CurrentRound())
	}

	// 第二轮提交丙1 并产生新提议，区块接在第一轮确认块之后。
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 5, 100)
	if _, err := n.Submit(c1); err != nil {
		t.Fatal(err)
	}
	want2 := []string{c1.ID()}
	second, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	id2 := BlockID(2, 2, id1, want2)
	assertProposalView(t, second, 2, id2, want2)

	// 第一轮取得后一直未编辑的结果，仍保留取得时的轮次、区块标识与交易顺序。
	assertProposalView(t, first, 1, id1, want1)

	// 进入新轮次后才编辑旧结果：第一轮确认历史与当前轮提议、候选都不受影响。
	tamperProposalView(&first)
	again, ok := n.Proposal()
	if !ok {
		t.Fatal("round 2 Proposal should exist")
	}
	assertProposalView(t, again, 2, id2, want2)

	rc1, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(rc1, id1); c == nil || c.Result != CandidateWon ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint(want1) {
		t.Fatalf("round 1 winner history changed by stale edit: %+v", c)
	}
	rc2, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(rc2, id2); c == nil || !c.Local || c.Result != CandidatePending ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint(want2) {
		t.Fatalf("round 2 local candidate changed by stale edit: %+v", c)
	}
	if candidateByBlockID(rc2, fakeBlockID) != nil {
		t.Fatal("fake block id from stale result must not register in round 2")
	}
	if _, err := n.Tx(fakeTxID); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake tx lookup got %v, want %s", err, ReasonUnknownTx)
	}

	// 旧结果不能作为影响当前轮的入口：其中的轮次与区块标识投票按原规则拒绝。
	if _, err := n.Vote(keys[0].pub, first.Round, first.BlockID); reason(err) != ReasonWrongRound {
		t.Fatalf("vote with stale result got %v, want %s", err, ReasonWrongRound)
	}

	// 当前轮凭真实标识投票确认，产生接在第一块之后的第二块；第一块保持原样。
	confirmByVotes(t, n, keys, id2)
	blk2, err := n.BlockAt(2)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, blk2, want2, 2, 2, id2, id1)
	blk1, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, blk1, want1, 1, 1, id1, "")

	// 确认后进入第三轮，当前轮尚无提议：当前轮查询继续按已有规则返回，
	// 旧轮结果保留在候选历史中，不被当作修改节点状态的入口。
	if n.CurrentRound() != 3 {
		t.Fatalf("round after second confirm = %d, want 3", n.CurrentRound())
	}
	if p, ok := n.Proposal(); ok {
		t.Fatalf("new round must start without proposal, got %+v", p)
	}
	rc2End, err := n.Candidates(2)
	if err != nil {
		t.Fatal(err)
	}
	if c := candidateByBlockID(rc2End, id2); c == nil || c.Result != CandidateWon ||
		fmt.Sprint(c.TxIDs) != fmt.Sprint(want2) {
		t.Fatalf("round 2 winner history wrong after confirm: %+v", c)
	}
}
