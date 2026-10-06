package consensus

import (
	"fmt"
	"testing"
)

// assertAccountState 断言账户查询的整体结果：已确认序号、最早缺口，
// 以及待处理交易按序号升序排列、各笔的等待说明（waiting-vote / waiting-pack）。
// wantSeqs 为空时表示待处理列表必须为空。
func assertAccountState(t *testing.T, n *Node, sender []byte, confirmed, gap uint64, wantSeqs []uint64, wantNotes []string) {
	t.Helper()
	acct := n.Account(sender)
	if acct.ConfirmedSequence != confirmed {
		t.Fatalf("confirmed sequence = %d, want %d", acct.ConfirmedSequence, confirmed)
	}
	if acct.Gap != gap {
		t.Fatalf("gap = %d, want %d (confirmed %d)", acct.Gap, gap, confirmed)
	}
	if len(acct.Pending) != len(wantSeqs) {
		t.Fatalf("pending count = %d, want %d (account %+v)", len(acct.Pending), len(wantSeqs), acct)
	}
	for i, seq := range wantSeqs {
		p := acct.Pending[i]
		if p.Tx.Sequence != seq {
			t.Fatalf("pending[%d] sequence = %d, want %d (pending must stay sorted and complete)", i, p.Tx.Sequence, seq)
		}
		if wantNotes != nil && p.Note != wantNotes[i] {
			t.Fatalf("pending[%d] (seq %d) note = %q, want %q", i, seq, p.Note, wantNotes[i])
		}
	}
}

// confirmLocalProposal 产生当前轮次的本地提议，核对其交易顺序，
// 再由前 3 名验证者（4 人名单，门槛为严格超过 2/3 即 3 票）投票确认，
// 返回确认块。提议内容不符合预期时直接失败，以免后续断言建立在错误前提上。
func confirmLocalProposal(t *testing.T, n *Node, keys []testKey, wantTxIDs []string) Block {
	t.Helper()
	round := n.CurrentRound()
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(wantTxIDs) {
		t.Fatalf("round %d proposal = %v, want %v", round, p.TxIDs, wantTxIDs)
	}
	var res *VoteResult
	for i := 0; i < 3; i++ {
		res, err = n.Vote(keys[i].pub, round, p.BlockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
	}
	if !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote should confirm the local proposal: %+v", res)
	}
	return *res.Block
}

// submitAll 依次提交交易，任何一笔被拒绝都直接失败。
func submitAll(t *testing.T, n *Node, txs ...*Transaction) {
	t.Helper()
	for _, tx := range txs {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit seq %d: %v", tx.Sequence, err)
		}
	}
}

// advanceAccountTo 把账户 keys[0] 的序号 1..upto 全部确认：
// 单块上限 2，逐轮提议并投票确认，返回时账户已确认序号为 upto。
// 提交顺序故意打乱、费用高低不一，确认结果不依赖提交先后与费用。
func advanceAccountTo(t *testing.T, n *Node, keys []testKey, upto uint64) {
	t.Helper()
	txs := map[uint64]*Transaction{}
	fees := []uint64{7, 1, 9, 3}
	for seq := uint64(1); seq <= upto; seq++ {
		txs[seq] = NewTransaction(keys[0].priv, seq, []byte(fmt.Sprintf("tx-%d", seq)), fees[(seq-1)%uint64(len(fees))], 100)
	}
	// 打乱提交顺序：偶数序号先交，奇数序号后交。
	for seq := uint64(2); seq <= upto; seq += 2 {
		submitAll(t, n, txs[seq])
	}
	for seq := uint64(1); seq <= upto; seq += 2 {
		submitAll(t, n, txs[seq])
	}
	for confirmed := uint64(0); confirmed < upto; confirmed += 2 {
		want := []string{txs[confirmed+1].ID()}
		if confirmed+2 <= upto {
			want = append(want, txs[confirmed+2].ID())
		}
		confirmLocalProposal(t, n, keys, want)
	}
	assertAccountState(t, n, keys[0].pub, upto, 0, nil, nil)
}

// 账户已有确认记录、后面同时存在多处缺口时，账户查询的最早缺口是挡在更高
// 序号前面的第一处缺失：确认到 4 后收到 6、8、10，缺口为 5；补 5 后缺口移到 7，
// 补 7 后移到 9，补 9 后归零且待处理列表按 5 到 10 连续排列。
// 费用高低与提交先后不改变结果；更高序号的交易始终留在待处理列表中，
// 不因前面缺失而被查询漏掉；列表末尾尚未提交的下一笔不算缺口。
func TestAccountGapAcrossMultipleHoles(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)
	advanceAccountTo(t, n, keys, 4)

	mk := func(seq, fee uint64) *Transaction {
		return NewTransaction(keys[0].priv, seq, []byte(fmt.Sprintf("gap-%d", seq)), fee, 100)
	}
	tx5, tx6, tx7, tx8, tx9, tx10 := mk(5, 2), mk(6, 9), mk(7, 4), mk(8, 1), mk(9, 8), mk(10, 3)

	// 乱序、费用交错地提交 6、8、10：已确认序号保持 4，最早缺口为 5，
	// 三笔高序号交易全部按序号升序留在待处理列表中。
	submitAll(t, n, tx10, tx6, tx8)
	assertAccountState(t, n, keys[0].pub, 4, 5,
		[]uint64{6, 8, 10}, []string{"waiting-pack", "waiting-pack", "waiting-pack"})

	// 补交 5：缺口移到 7。
	submitAll(t, n, tx5)
	assertAccountState(t, n, keys[0].pub, 4, 7,
		[]uint64{5, 6, 8, 10}, []string{"waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack"})

	// 补交 7：缺口移到 9。
	submitAll(t, n, tx7)
	assertAccountState(t, n, keys[0].pub, 4, 9,
		[]uint64{5, 6, 7, 8, 10},
		[]string{"waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack"})

	// 补交 9：序号 5 到 10 连续，缺口归零；末尾尚未提交的 11 不算缺口。
	submitAll(t, n, tx9)
	assertAccountState(t, n, keys[0].pub, 4, 0,
		[]uint64{5, 6, 7, 8, 9, 10},
		[]string{"waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack"})
}

// 待处理列表同时包含等待投票与等待打包的交易：单块上限 2，补齐 5 但尚未补齐 7
// 时产生本地提议，序号 5、6 进入候选（waiting-vote），8、10 仍在排队
// （waiting-pack）；已确认序号仍为 4，缺口仍为 7——进入候选的交易不算缺失。
// 提议冻结后补交 7、9，缺口依次变为 9 和 0，提议仍只含 5、6。
// 提议确认后已确认序号推进到 6，待处理只剩 7 至 10，缺口继续为 0，
// 不因 5、6 已离开池中而重新报缺口 5。
func TestAccountGapWithProposedAndQueued(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)
	advanceAccountTo(t, n, keys, 4)

	mk := func(seq, fee uint64) *Transaction {
		return NewTransaction(keys[0].priv, seq, []byte(fmt.Sprintf("mix-%d", seq)), fee, 100)
	}
	tx5, tx6, tx7, tx8, tx9, tx10 := mk(5, 1), mk(6, 6), mk(7, 3), mk(8, 9), mk(9, 2), mk(10, 5)

	submitAll(t, n, tx6, tx8, tx10, tx5)
	assertAccountState(t, n, keys[0].pub, 4, 7,
		[]uint64{5, 6, 8, 10}, []string{"waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack"})

	// 产生本地提议：5、6 进入候选等待投票，8、10 仍等待打包。
	// 缺口仍是 7：已确认序号 4 之后，5、6 已在候选中不算缺失。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantProposal := []string{tx5.ID(), tx6.ID()}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(wantProposal) {
		t.Fatalf("proposal = %v, want %v", p.TxIDs, wantProposal)
	}
	assertAccountState(t, n, keys[0].pub, 4, 7,
		[]uint64{5, 6, 8, 10},
		[]string{"waiting-vote", "waiting-vote", "waiting-pack", "waiting-pack"})

	// 提议产生后补交 7：缺口移到 9，已冻结的提议仍只含 5、6。
	submitAll(t, n, tx7)
	assertAccountState(t, n, keys[0].pub, 4, 9,
		[]uint64{5, 6, 7, 8, 10},
		[]string{"waiting-vote", "waiting-vote", "waiting-pack", "waiting-pack", "waiting-pack"})
	frozen, ok := n.Proposal()
	if !ok || fmt.Sprint(frozen.TxIDs) != fmt.Sprint(wantProposal) {
		t.Fatalf("proposal must stay frozen at %v, got %+v", wantProposal, frozen)
	}

	// 补交 9：缺口归零，提议依旧不变。
	submitAll(t, n, tx9)
	assertAccountState(t, n, keys[0].pub, 4, 0,
		[]uint64{5, 6, 7, 8, 9, 10},
		[]string{"waiting-vote", "waiting-vote", "waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack"})
	frozen, ok = n.Proposal()
	if !ok || fmt.Sprint(frozen.TxIDs) != fmt.Sprint(wantProposal) {
		t.Fatalf("proposal must stay frozen at %v, got %+v", wantProposal, frozen)
	}

	// 投票确认该提议：已确认序号推进到 6，待处理只剩 7 至 10。
	// 缺口继续为 0，不能因为 5、6 已离开池中就重新报缺口 5。
	round := n.CurrentRound()
	var res *VoteResult
	for i := 0; i < 3; i++ {
		res, err = n.Vote(keys[i].pub, round, p.BlockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
	}
	if !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote should confirm: %+v", res)
	}
	assertAccountState(t, n, keys[0].pub, 6, 0,
		[]uint64{7, 8, 9, 10},
		[]string{"waiting-pack", "waiting-pack", "waiting-pack", "waiting-pack"})
}

// 边界情形：没有任何待处理交易的账户返回空列表与缺口 0；
// 从已确认序号加一开始连续排队、但末尾没有更多交易的账户同样返回缺口 0
// （末尾尚未提交的下一笔不算缺口）；全部确认完毕后回到空列表与缺口 0。
func TestAccountGapEmptyAndContinuous(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 2)

	// 从未提交过交易的账户：空列表、缺口 0、已确认序号 0。
	assertAccountState(t, n, keys[1].pub, 0, 0, nil, nil)

	// 从已确认序号加一开始连续排队，末尾没有更多交易：缺口 0。
	tx1 := NewTransaction(keys[0].priv, 1, []byte("one"), 1, 100)
	tx2 := NewTransaction(keys[0].priv, 2, []byte("two"), 2, 100)
	tx3 := NewTransaction(keys[0].priv, 3, []byte("three"), 3, 100)
	submitAll(t, n, tx1, tx2, tx3)
	assertAccountState(t, n, keys[0].pub, 0, 0,
		[]uint64{1, 2, 3}, []string{"waiting-pack", "waiting-pack", "waiting-pack"})
	// 未提交过交易的账户不受邻居影响。
	assertAccountState(t, n, keys[1].pub, 0, 0, nil, nil)

	// 全部确认完毕后：已确认序号 3，待处理列表为空，缺口 0。
	confirmLocalProposal(t, n, keys, []string{tx1.ID(), tx2.ID()})
	confirmLocalProposal(t, n, keys, []string{tx3.ID()})
	assertAccountState(t, n, keys[0].pub, 3, 0, nil, nil)
}
