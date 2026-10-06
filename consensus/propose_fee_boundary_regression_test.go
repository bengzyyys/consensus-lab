package consensus

import (
	"fmt"
	"math"
	"testing"
)

// 费用接近无符号 64 位整数边界时的打包回归保障。
// 现有打包用例多用小费用，极大数值的覆盖集中在编码；这里保护实际打包结果：
// 费用必须按真实大小排序，跨过最高位（2^63）或差值很大时不能倒置，
// 0 费用仍是可接收、可打包的合法交易。

// 四个账户各提供下一笔可确认交易，费用分别为
// 0、2^63-1、2^63、2^64-1：全部具备入选条件时，提议必须按真实费用从大到小排列，
// 0 费用交易照常入选，不能被当成“没有候选”。
func TestProposeFeeUint64BoundaryOrder(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	zero := NewTransaction(keys[0].priv, 1, []byte("zero"), 0, 100)
	maxI64 := NewTransaction(keys[1].priv, 1, []byte("max-int64"), math.MaxInt64, 100)
	overI64 := NewTransaction(keys[2].priv, 1, []byte("over-int64"), math.MaxInt64+1, 100)
	maxU64 := NewTransaction(keys[3].priv, 1, []byte("max-uint64"), math.MaxUint64, 100)
	// 故意打乱提交顺序，且让 0 费用最先提交：接收与排序都不受提交次序影响。
	for _, tx := range []*Transaction{zero, maxU64, maxI64, overI64} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{maxU64.ID(), overI64.ID(), maxI64.ID(), zero.ID()}
	p := assertProposalOrder(t, n, want)
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}
	if p.Empty {
		t.Fatal("proposal must not be empty")
	}
	// 生成提议不推进已确认序号、当前轮次与确认高度。
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1 (propose must not advance the round)", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0 (propose must not confirm anything)", n.Height())
	}
	for i, tx := range []*Transaction{zero, maxI64, overI64, maxU64} {
		assertAccountQueue(t, n, keys[i].pub, 0, map[string]string{tx.ID(): "waiting-vote"})
	}
}

// 0 费用是池中唯一候选时也必须被打包：不能因费用为 0 而误判为无可选交易、产生空块。
func TestProposeZeroFeeOnlyStillPacks(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	zero := NewTransaction(keys[0].priv, 1, []byte("zero"), 0, 100)
	if _, err := n.Submit(zero); err != nil {
		t.Fatal(err)
	}
	p := assertProposalOrder(t, n, []string{zero.ID()})
	if p.Empty {
		t.Fatal("zero-fee transaction is a valid candidate; proposal must not be empty")
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{zero.ID(): "waiting-vote"})
}

// 费用比较与账户序号的关系：同一账户下一笔费用为 0、后续一笔费用达 2^64-1，
// 后续交易也只能在前一笔选入后才参与其他账户的竞争；开始参与后应立即凭费用
// 排到剩余可选交易的最前。顺序必须为 乙1(2^63)、丙1(2^63-1)、甲1(0)、甲2(2^64-1)。
func TestProposeFeeBoundaryUnlocksSuccessor(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 0, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), math.MaxUint64, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), math.MaxInt64+1, 100)
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), math.MaxInt64, 100)
	for _, tx := range []*Transaction{a1, a2, b1, c1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 甲2 费用最高却不能越过甲1；甲1 入选后甲2 立即凭最大费用取得下一位置。
	want := []string{b1.ID(), c1.ID(), a1.ID(), a2.ID()}
	p := assertProposalOrder(t, n, want)
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1 (propose must not advance the round)", n.CurrentRound())
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		a1.ID(): "waiting-vote", a2.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{c1.ID(): "waiting-vote"})
}

// 两笔可选交易费用相同且都处在高数值范围（2^64-1）时，按完整交易标识的字典序
// 决定先后：标识较小者先入选，与提交顺序、账户公钥顺序无关。
func TestProposeHighFeeTieBreakByID(t *testing.T) {
	keys := genKeys(t, 4)

	// 甲1 标识较小、乙1 标识较大，费用同为 2^64-1：无论谁先提交，甲1 都在前。
	t.Run("smaller id first regardless of submission order", func(t *testing.T) {
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), math.MaxUint64, 100)
		a1 := craftTxWithID(t, keys[0].priv, 1, math.MaxUint64, b1.ID(), true)
		set := []*Transaction{a1, b1}
		want := []string{a1.ID(), b1.ID()}

		var prev *ProposalView
		for _, order := range [][]*Transaction{set, {b1, a1}} {
			n, _ := newTestNode(t, keys, 4)
			for _, tx := range order {
				if _, err := n.Submit(tx); err != nil {
					t.Fatal(err)
				}
			}
			p := assertProposalOrder(t, n, want)
			if prev != nil && p.BlockID != prev.BlockID {
				t.Fatalf("submission order changed block id:\n%s\n%s", prev.BlockID, p.BlockID)
			}
			pp := p
			prev = &pp
		}
	})

	// 对称情形：甲1 标识较大时乙1 在前，同费比较只认标识字典序。
	t.Run("larger id yields to smaller id", func(t *testing.T) {
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), math.MaxUint64, 100)
		a1 := craftTxWithID(t, keys[0].priv, 1, math.MaxUint64, b1.ID(), false)
		n, _ := newTestNode(t, keys, 4)
		for _, tx := range []*Transaction{a1, b1} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		assertProposalOrder(t, n, []string{b1.ID(), a1.ID()})
	})

	// 同费比较同样适用于跨最高位的高数值（2^63）：标识较小者先入选。
	t.Run("tie at 2^63 decided by id", func(t *testing.T) {
		b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), math.MaxInt64+1, 100)
		a1 := craftTxWithID(t, keys[0].priv, 1, math.MaxInt64+1, b1.ID(), true)
		n, _ := newTestNode(t, keys, 4)
		for _, tx := range []*Transaction{b1, a1} {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		assertProposalOrder(t, n, []string{a1.ID(), b1.ID()})
	})
}

// 可连续选入的交易多于单块上限时，提议正好停在上限：
// 选入的高费用交易等待投票，未选入的（含 0 费用）留在池中等待打包，
// 已确认序号、当前轮次与确认高度都不被提议推进。
func TestProposeFeeBoundaryLimitedLeavesRestQueued(t *testing.T) {
	keys := genKeys(t, 5)
	n, _ := newTestNode(t, keys, 3)

	fees := []uint64{math.MaxUint64, math.MaxInt64 + 1, math.MaxInt64, 1, 0}
	txs := make([]*Transaction, len(fees))
	for i, fee := range fees {
		txs[i] = NewTransaction(keys[i].priv, 1, []byte(fmt.Sprintf("tx-%d", i)), fee, 100)
	}
	// 打乱提交顺序：低费用先提交也不能先入选。
	for _, tx := range []*Transaction{txs[4], txs[2], txs[0], txs[3], txs[1]} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{txs[0].ID(), txs[1].ID(), txs[2].ID()}
	p := assertProposalOrder(t, n, want)
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1 (propose must not advance the round)", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0 (propose must not confirm anything)", n.Height())
	}
	// 选入者等待投票，未选入者仍在池中等待打包；已确认序号保持 0。
	for i := 0; i < 3; i++ {
		assertAccountQueue(t, n, keys[i].pub, 0, map[string]string{txs[i].ID(): "waiting-vote"})
	}
	for i := 3; i < 5; i++ {
		assertAccountQueue(t, n, keys[i].pub, 0, map[string]string{txs[i].ID(): "waiting-pack"})
		info, err := n.Tx(txs[i].ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued {
			t.Fatalf("tx %d status = %s, want queued", i, info.Status)
		}
	}
}
