package consensus

import (
	"crypto/ed25519"
	"fmt"
	"testing"
)

// 提议顺序的完整断言辅助：提议的交易标识序列必须与 want 完全一致。
func assertProposalOrder(t *testing.T, n *Node, want []string) ProposalView {
	t.Helper()
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal order = %v, want %v", p.TxIDs, want)
	}
	return p
}

// 断言账户的已确认序号与每笔待处理交易的等待说明（waiting-vote / waiting-pack）。
func assertAccountQueue(t *testing.T, n *Node, pub ed25519.PublicKey, confirmed uint64, notes map[string]string) {
	t.Helper()
	acct := n.Account(pub)
	if acct.ConfirmedSequence != confirmed {
		t.Fatalf("confirmed sequence = %d, want %d", acct.ConfirmedSequence, confirmed)
	}
	if len(acct.Pending) != len(notes) {
		t.Fatalf("pending count = %d, want %d (%+v)", len(acct.Pending), len(notes), acct.Pending)
	}
	for _, info := range acct.Pending {
		want, ok := notes[info.ID]
		if !ok {
			t.Fatalf("unexpected pending tx %s", info.ID)
		}
		if info.Note != want {
			t.Fatalf("tx %s note = %q, want %q", info.ID, info.Note, want)
		}
	}
}

// 跨账户打包：后续序号必须等前一笔选入后才参与竞争，选入后凭费用进入比较。
// 账户甲序号 1、2 费用 30、100，账户乙序号 1、2 费用 50、20，上限 4 时
// 完整顺序为 乙1、甲1、甲2、乙2：甲2 开始不能越过甲1，甲1 入选后甲2 凭高费用排在乙2 前。
func TestProposeOrderUnlocksNextSequence(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 30, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 100, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 20, 100)
	for _, tx := range []*Transaction{a1, a2, b1, b2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{b1.ID(), a1.ID(), a2.ID(), b2.ID()}
	p := assertProposalOrder(t, n, want)
	// 区块标识按公开规则由轮次、高度、前块标识与交易顺序唯一确定。
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}
	// 提议生成不推进任何账户的已确认序号，也不推进轮次。
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1 (propose must not advance the round)", n.CurrentRound())
	}
	// 选入的交易全部等待投票。
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		a1.ID(): "waiting-vote", a2.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{
		b1.ID(): "waiting-vote", b2.ID(): "waiting-vote",
	})
}

// 同一交易集合、上限 3：提议只含前三笔，乙2 留在池中等待打包，其余等待投票。
func TestProposeOrderLimitedLeavesRestQueued(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 3)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 30, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 100, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 20, 100)
	for _, tx := range []*Transaction{a1, a2, b1, b2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{b1.ID(), a1.ID(), a2.ID()}
	p := assertProposalOrder(t, n, want)
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}
	// 未选入的乙2 保持等待打包，已选入的等待投票；已确认序号不受影响。
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		a1.ID(): "waiting-vote", a2.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{
		b1.ID(): "waiting-vote", b2.ID(): "waiting-pack",
	})
	info, err := n.Tx(b2.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("b2 status = %s, want queued", info.Status)
	}
}

// craftTxWithID 通过变换内容构造一笔交易，使其标识相对 ref 满足字典序要求
// （less 为 true 时要求标识更小）。用于确定性构造同费用比较场景。
func craftTxWithID(t *testing.T, priv ed25519.PrivateKey, seq, fee uint64, ref string, less bool) *Transaction {
	t.Helper()
	for i := 0; ; i++ {
		tx := NewTransaction(priv, seq, []byte(fmt.Sprintf("tie-%d", i)), fee, 100)
		if (tx.ID() < ref) == less {
			return tx
		}
		if i > 10000 {
			t.Fatal("cannot craft transaction with desired id ordering")
		}
	}
}

// 同费用比较只发生在交易实际具备入选条件时：
// 刚成为可选的后续交易与另一账户的下一笔同费用，按标识字典序取较小者；
// 尚未具备条件的交易不能凭较小标识提前占位。
func TestProposeTieBreakOnlyWhenEligible(t *testing.T) {
	keys := genKeys(t, 4)

	// 甲1 费用最高先入选，甲2 随即具备条件并与乙1 同费用：
	// 甲2 标识更小时必须凭字典序排在乙1 前，不能因刚具备条件而固定靠后。
	t.Run("newly eligible wins tie by smaller id", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 4)
		a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 100, 100)
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
		a2 := craftTxWithID(t, keys[0].priv, 2, 50, b1.ID(), true)
		for _, tx := range []*Transaction{a1, a2, b1} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		assertProposalOrder(t, n, []string{a1.ID(), a2.ID(), b1.ID()})
	})

	// 对称情形：刚具备条件的甲2 标识更大时排在乙1 后，也不能固定靠前。
	t.Run("newly eligible loses tie by larger id", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 4)
		a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 100, 100)
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
		a2 := craftTxWithID(t, keys[0].priv, 2, 50, b1.ID(), false)
		for _, tx := range []*Transaction{a1, a2, b1} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		assertProposalOrder(t, n, []string{a1.ID(), b1.ID(), a2.ID()})
	})

	// 甲2 与甲1 同费用且标识更小，但甲1 未选入前甲2 不具备条件：
	// 不能凭较小标识提前占位，顺序必须是 乙1、甲1、甲2。
	t.Run("not yet eligible cannot preempt by smaller id", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 4)
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 60, 100)
		a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 50, 100)
		a2 := craftTxWithID(t, keys[0].priv, 2, 50, a1.ID(), true)
		for _, tx := range []*Transaction{a1, a2, b1} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		assertProposalOrder(t, n, []string{b1.ID(), a1.ID(), a2.ID()})
	})
}

// 相同有效交易集合、相同轮次与配置：提议产生前调整提交顺序，
// 最终交易顺序与区块标识不变（无替换、无容量淘汰）。
func TestProposeIndependentOfSubmissionOrder(t *testing.T) {
	keys := genKeys(t, 4)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 30, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 100, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 20, 100)
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), 50, 100) // 与 b1 同费用，考验字典序
	set := []*Transaction{a1, a2, b1, b2, c1}

	build := func(order []*Transaction) (ProposalView, Block) {
		n, _ := newTestNode(t, keys, 4)
		for _, tx := range order {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		confirmByVotes(t, n, keys, p.BlockID)
		b, err := n.BlockAt(1)
		if err != nil {
			t.Fatal(err)
		}
		return p, b
	}

	pFwd, bFwd := build(set)
	pRev, bRev := build([]*Transaction{c1, b2, b1, a2, a1})
	if fmt.Sprint(pFwd.TxIDs) != fmt.Sprint(pRev.TxIDs) {
		t.Fatalf("submission order changed proposal order:\n%v\n%v", pFwd.TxIDs, pRev.TxIDs)
	}
	if pFwd.BlockID != pRev.BlockID {
		t.Fatalf("submission order changed block id:\n%s\n%s", pFwd.BlockID, pRev.BlockID)
	}
	if len(pFwd.TxIDs) != 4 {
		t.Fatalf("proposal should pack 4 transactions, got %v", pFwd.TxIDs)
	}
	// 确认后的历史同样一致。
	if bFwd.ID != bRev.ID || fmt.Sprint(bFwd.TxIDs) != fmt.Sprint(bRev.TxIDs) {
		t.Fatal("confirmed history differs between submission orders")
	}
}

// 序号缺口边界：缺少下一条应确认交易时，更高序号无论费用多高都不能入选，
// 其他账户照常竞争；没有任何账户提供可选交易时产生空块。
func TestProposeSequenceGapBoundary(t *testing.T) {
	keys := genKeys(t, 4)

	// 甲缺序号 1，序号 2 费用再高也不能入选；乙的序号 1 照常竞争入选。
	t.Run("gap blocks high fee successor", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 4)
		a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 1000, 100)
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 1, 100)
		for _, tx := range []*Transaction{a2, b1} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		assertProposalOrder(t, n, []string{b1.ID()})
		// 甲2 仍在池中等待缺口，账户查询指出最早缺口序号 1。
		acct := n.Account(keys[0].pub)
		if acct.Gap != 1 || len(acct.Pending) != 1 || acct.Pending[0].ID != a2.ID() {
			t.Fatalf("gap account state wrong: %+v", acct)
		}
		if acct.Pending[0].Note != "waiting-pack" {
			t.Fatalf("a2 note = %q, want waiting-pack", acct.Pending[0].Note)
		}
	})

	// 所有账户都缺下一条：即使池中还有等待缺口的交易，也产生空块。
	t.Run("no eligible tx yields empty block", func(t *testing.T) {
		n, _ := newTestNode(t, keys, 4)
		a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 1000, 100)
		b3 := NewTransaction(keys[1].priv, 3, []byte("b3"), 999, 100)
		for _, tx := range []*Transaction{a2, b3} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		p := assertProposalOrder(t, n, []string{})
		if !p.Empty {
			t.Fatalf("proposal should be empty, got %v", p.TxIDs)
		}
		if wantID := BlockID(1, 1, "", []string{}); p.BlockID != wantID {
			t.Fatalf("empty block id = %s, want %s", p.BlockID, wantID)
		}
		// 等待缺口的交易保持排队，已确认序号不被提议推进。
		assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{a2.ID(): "waiting-pack"})
		assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b3.ID(): "waiting-pack"})
	})
}

// 已确认序号不为 0 的账户从其下一序号开始参与打包，不能重新从 1 取交易。
func TestProposeStartsAfterConfirmedSequence(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	// 第一轮：确认甲的序号 1、2，使其已确认序号变为 2。
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 10, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 20, 100)
	for _, tx := range []*Transaction{a1, a2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p1 := assertProposalOrder(t, n, []string{a1.ID(), a2.ID()})
	confirmByVotes(t, n, keys, p1.BlockID)
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 2 {
		t.Fatalf("confirmed sequence = %d, want 2", acct.ConfirmedSequence)
	}

	// 第二轮：甲的序号 3、4 与乙的序号 1 竞争。甲3 费用 10、甲4 费用 100、乙1 费用 50。
	// 甲必须从序号 3 开始：顺序为 乙1(50)、甲3(10)、甲4(100 待甲3 入选后才可参与)。
	a3 := NewTransaction(keys[0].priv, 3, []byte("a3"), 10, 100)
	a4 := NewTransaction(keys[0].priv, 4, []byte("a4"), 100, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	for _, tx := range []*Transaction{a3, a4, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{b1.ID(), a3.ID(), a4.ID()}
	p2 := assertProposalOrder(t, n, want)
	// 第二轮的区块标识接在第一个确认块之后。
	if wantID := BlockID(2, 2, p1.BlockID, want); p2.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p2.BlockID, wantID)
	}
	// 提议不推进已确认序号：甲仍为 2，乙仍为 0。
	assertAccountQueue(t, n, keys[0].pub, 2, map[string]string{
		a3.ID(): "waiting-vote", a4.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{
		b1.ID(): "waiting-vote",
	})
}
