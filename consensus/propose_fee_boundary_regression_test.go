package consensus

import (
	"fmt"
	"sort"
	"testing"
)

// 本文件为“本地提议按费用排序”补充回归保障：现有打包用例只使用较小费用，
// 极大费用的覆盖集中在稳定编码（encoding_regression_test.go）。这里钉住费用
// 跨越无符号 64 位整数边界时的实际打包结果——费用按 uint64 真实大小比较，
// 0 费用是合法候选，序号门控、同费按交易标识定序与单块上限规则都不得被破坏。
//
// 回归针对的典型错误实现：把费用当 int64 比较（2^63 会被视为负数而倒置）、
// 有符号差值判大小（相差很大的两笔费用可能反转或溢出为 0 被判相同）、
// 把 0 费用当成“没有候选”、以及让后续序号交易越过账户下一笔交易提前占位。
const (
	feeZero    uint64 = 0
	feeIntMax  uint64 = 9223372036854775807  // 2^63-1：有符号 64 位整数最大值
	feeHighBit uint64 = 9223372036854775808  // 2^63：跨过最高符号位
	feeUintMax uint64 = 18446744073709551615 // 2^64-1：无符号 64 位整数最大值
)

// 四个不同账户的下一笔可确认交易同时存在，费用分别为 0、2^63-1、2^63、2^64-1。
// 完整提议顺序必须严格按费用真实大小排列：跨过最高位（2^63 必须在 2^63-1 前）、
// 两笔费用相差极大（最大值与 0）都不能倒置；0 费用交易合法入选且排在最后，
// 提议非空。选入者全部等待投票，提议生成不推进序号、轮次与确认高度。
func TestProposeSortsFeesAcrossUint64Boundaries(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	txMax := NewTransaction(keys[0].priv, 1, []byte("fee-max"), feeUintMax, 100)
	txHighBit := NewTransaction(keys[1].priv, 1, []byte("fee-high-bit"), feeHighBit, 100)
	txIntMax := NewTransaction(keys[2].priv, 1, []byte("fee-int-max"), feeIntMax, 100)
	txZero := NewTransaction(keys[3].priv, 1, []byte("fee-zero"), feeZero, 100)

	// 故意打乱费用次序提交：打包结果只取决于费用与交易标识。
	for _, tx := range []*Transaction{txZero, txIntMax, txMax, txHighBit} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{txMax.ID(), txHighBit.ID(), txIntMax.ID(), txZero.ID()}
	p := assertProposalOrder(t, n, want)
	if p.Empty {
		t.Fatalf("proposal with eligible txs must not be empty: %v", p.TxIDs)
	}
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}

	// 费用必须按无符号整数原样保留并参与比较（防止任何符号转换渗入打包路径）。
	pairs := []struct {
		fee uint64
		tx  *Transaction
	}{
		{feeUintMax, txMax},
		{feeHighBit, txHighBit},
		{feeIntMax, txIntMax},
		{feeZero, txZero},
	}
	for _, pair := range pairs {
		info, err := n.Tx(pair.tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Tx.Fee != pair.fee {
			t.Fatalf("tx %s stored fee = %d, want %d", pair.tx.ID(), info.Tx.Fee, pair.fee)
		}
	}

	// 四笔全部选入、等待投票；每个账户的已确认序号仍为 0。
	for i, pair := range pairs {
		assertAccountQueue(t, n, keys[i].pub, 0, map[string]string{
			pair.tx.ID(): "waiting-vote",
		})
	}
	// 提议生成不推进当前轮次与确认高度。
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0 (propose must not confirm a block)", n.Height())
	}
}

// 池中只有 0 费用交易时，它们仍是合法候选而不是“没有候选”：
// 提议必须非空，两笔 0 费用交易按交易标识字典序打包并等待投票。
func TestProposePacksZeroFeeTxs(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	a1 := NewTransaction(keys[0].priv, 1, []byte("zero-a"), feeZero, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("zero-b"), feeZero, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{a1.ID(), b1.ID()}
	sort.Strings(want)
	p := assertProposalOrder(t, n, want)
	if p.Empty {
		t.Fatal("zero-fee txs must be packable candidates, not an empty block")
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{a1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-vote"})
	if n.Height() != 0 || n.CurrentRound() != 1 {
		t.Fatalf("propose must not advance height/round: height=%d round=%d", n.Height(), n.CurrentRound())
	}
}

// 费用比较与账户序号的关系：同一账户的下一笔费用很低，后续一笔即使费用达到
// uint64 最大值，也只能在前一笔选入后才参与竞争；一旦开始参与，必须立即凭
// 真实费用取得位置（在最大值与 0 的极端差距下排在仅存的 0 费用候选之前）。
//
// 账户布局（均为序号 1，甲另有一笔序号 2）：
//
//	甲1 费用 1、甲2 费用 2^64-1（锁定到甲1 入选）
//	乙1 费用 2^63、丙1 费用 2^63-1、丁1 费用 0
//
// 完整顺序必须是 乙1、丙1、甲1、甲2、丁1：
// 甲2 不能凭最大费用越过甲1；甲1 入选后甲2 立即压过丁1，而不是被固定排在最后。
func TestProposeSequenceGatingAtFeeBoundaries(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 5)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2-max"), feeUintMax, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), feeHighBit, 100)
	c1 := NewTransaction(keys[2].priv, 1, []byte("c1"), feeIntMax, 100)
	d1 := NewTransaction(keys[3].priv, 1, []byte("d1-zero"), feeZero, 100)
	for _, tx := range []*Transaction{a2, d1, a1, c1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	assertProposalOrder(t, n, []string{b1.ID(), c1.ID(), a1.ID(), a2.ID(), d1.ID()})

	// 甲的两笔按序号先后都已选入；丁的 0 费用交易最后入选，均等待投票。
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{
		a1.ID(): "waiting-vote", a2.ID(): "waiting-vote",
	})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{b1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{c1.ID(): "waiting-vote"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{d1.ID(): "waiting-vote"})
}

// 两笔可选交易费用相同且都处在高数值范围时，按完整交易标识字典序决定先后；
// 提交顺序与账户公钥顺序都不得影响结果。为同时证伪“公钥顺序定序”，刻意让
// 标识更小的交易来自公钥十六进制更大的账户。2^63 与 2^64-1 两个高值各验一次，
// 防止同费比较在最高位附近被误判成大小关系。
func TestProposeHighFeeTieBreakByTxID(t *testing.T) {
	for _, fee := range []uint64{feeHighBit, feeUintMax} {
		t.Run(fmt.Sprintf("fee-%d", fee), func(t *testing.T) {
			keys := genKeys(t, 6)

			// 选出公钥十六进制最小与最大的两个账户。
			ordered := append([]testKey(nil), keys...)
			sort.Slice(ordered, func(i, j int) bool {
				return fmt.Sprintf("%x", ordered[i].pub) < fmt.Sprintf("%x", ordered[j].pub)
			})
			lowHexKey := ordered[0]
			highHexKey := ordered[len(ordered)-1]

			// 标识更大的交易放在公钥字典序更小的账户，标识更小的放在公钥更大的账户。
			bigIDTx := NewTransaction(lowHexKey.priv, 1, []byte("tie-big-id"), fee, 100)
			smallIDTx := craftTxWithID(t, highHexKey.priv, 1, fee, bigIDTx.ID(), true)
			if smallIDTx.ID() >= bigIDTx.ID() {
				t.Fatalf("setup: crafted id %s must be smaller than %s", smallIDTx.ID(), bigIDTx.ID())
			}
			if fmt.Sprintf("%x", smallIDTx.Sender) <= fmt.Sprintf("%x", bigIDTx.Sender) {
				t.Fatal("setup: smaller-id tx must come from the lexicographically larger sender key")
			}

			want := []string{smallIDTx.ID(), bigIDTx.ID()}
			build := func(order []*Transaction) ProposalView {
				n, _ := newTestNode(t, keys, 2)
				for _, tx := range order {
					if _, err := n.Submit(tx); err != nil {
						t.Fatal(err)
					}
				}
				return assertProposalOrder(t, n, want)
			}

			// 两种提交顺序都在提议产生前完成，最终提议顺序与区块标识必须一致。
			pFwd := build([]*Transaction{smallIDTx, bigIDTx})
			pRev := build([]*Transaction{bigIDTx, smallIDTx})
			if pFwd.BlockID != pRev.BlockID {
				t.Fatalf("submission order changed block id:\n%s\n%s", pFwd.BlockID, pRev.BlockID)
			}
		})
	}
}

// 可连续选入的交易多于单块上限时，提议正好停在上限：
// 五个账户各一笔可确认交易，费用为 2^64-1、2^63、2^63-1、1、0，上限为 4。
// 选入前四笔，0 费用交易按规则尚未选入、留在池中等待打包；选入者等待投票，
// 已确认序号、当前轮次与确认高度都不被提议推进。
func TestProposeBoundaryFeesStopExactlyAtBlockLimit(t *testing.T) {
	keys := genKeys(t, 5)
	n, _ := newTestNode(t, keys, 4)

	txMax := NewTransaction(keys[0].priv, 1, []byte("fee-max"), feeUintMax, 100)
	txHighBit := NewTransaction(keys[1].priv, 1, []byte("fee-high-bit"), feeHighBit, 100)
	txIntMax := NewTransaction(keys[2].priv, 1, []byte("fee-int-max"), feeIntMax, 100)
	txOne := NewTransaction(keys[3].priv, 1, []byte("fee-one"), 1, 100)
	txZero := NewTransaction(keys[4].priv, 1, []byte("fee-zero"), feeZero, 100)
	// 打乱提交顺序，且在提议产生前全部提交完毕，避免混入提议冻结规则。
	for _, tx := range []*Transaction{txOne, txZero, txMax, txIntMax, txHighBit} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{txMax.ID(), txHighBit.ID(), txIntMax.ID(), txOne.ID()}
	p := assertProposalOrder(t, n, want)
	if uint64(len(p.TxIDs)) != n.Config().MaxTxsPerBlock {
		t.Fatalf("proposal length = %d, want exactly %d", len(p.TxIDs), n.Config().MaxTxsPerBlock)
	}
	if wantID := BlockID(1, 1, "", want); p.BlockID != wantID {
		t.Fatalf("block id = %s, want %s", p.BlockID, wantID)
	}

	// 选入的四笔等待投票；未选入的 0 费用交易仍在池中等待打包。
	selected := []struct {
		keyIdx int
		tx     *Transaction
	}{
		{0, txMax}, {1, txHighBit}, {2, txIntMax}, {3, txOne},
	}
	for _, s := range selected {
		assertAccountQueue(t, n, keys[s.keyIdx].pub, 0, map[string]string{
			s.tx.ID(): "waiting-vote",
		})
	}
	assertAccountQueue(t, n, keys[4].pub, 0, map[string]string{
		txZero.ID(): "waiting-pack",
	})
	info, err := n.Tx(txZero.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("unselected tx status = %s, want queued", info.Status)
	}

	// 提议不推进账户已确认序号、当前轮次与确认高度。
	for i := range keys {
		if acct := n.Account(keys[i].pub); acct.ConfirmedSequence != 0 {
			t.Fatalf("account %d confirmed sequence = %d, want 0", i, acct.ConfirmedSequence)
		}
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0", n.Height())
	}
}
