package consensus

import (
	"crypto/ed25519"
	"fmt"
	"testing"
)

// 本文件钉住账户查询“最早缺少序号”（AccountInfo.Gap）的回归保障，重点覆盖
// 已经有确认记录、后面同时存在多处缺口的账户。
//
// 缺口的语义是：当前待处理交易（池内排队 + 已进入候选等待投票）中，挡在更高
// 序号前面的第一处缺失。待处理列表按序号升序返回，费用高低与提交先后都不能
// 改变次序或缺口；更高序号的交易必须一直留在待处理列表里，不能因为前面有缺口
// 就被查询漏掉。待处理列表末尾尚未提交的“下一笔”不算缺口；已经进入候选、
// 正在等待投票的交易同样在场，不能被当作缺失。
//
// 本组测试只沿用现有的 Submit/Propose/Vote 与 Account 查询，不新增公开入口，
// 不改变账户查询的返回形式，也不改变任何交易或候选的状态流转。

// pendingExpect 是账户待处理列表中一个位置的期望：序号、交易标识与等待说明。
type pendingExpect struct {
	seq  uint64
	id   string
	note string // "waiting-vote" 或 "waiting-pack"
}

// assertAccountGapView 钉住账户查询的完整视图：已确认序号、最早缺口、按序号
// 升序排列的待处理交易标识，以及每笔交易的状态与等待说明（waiting-vote 对应
// proposed，waiting-pack 对应 queued）。
func assertAccountGapView(t *testing.T, n *Node, pub ed25519.PublicKey, confirmed, gap uint64, want []pendingExpect) {
	t.Helper()
	acct := n.Account(pub)
	if acct.Sender != fmt.Sprintf("%x", pub) {
		t.Fatalf("account sender = %q, want %x", acct.Sender, pub)
	}
	if acct.ConfirmedSequence != confirmed {
		t.Fatalf("confirmed sequence = %d, want %d", acct.ConfirmedSequence, confirmed)
	}
	if acct.Gap != gap {
		t.Fatalf("earliest gap = %d, want %d (pending view: %+v)", acct.Gap, gap, acct.Pending)
	}
	if len(acct.Pending) != len(want) {
		t.Fatalf("pending count = %d, want %d: %+v", len(acct.Pending), len(want), acct.Pending)
	}
	for i, w := range want {
		info := acct.Pending[i]
		if info.ID != w.id {
			t.Fatalf("pending[%d] id = %s, want %s (full order: %v)", i, info.ID, w.id, pendingIDs(acct))
		}
		if info.Tx == nil || info.Tx.Sequence != w.seq {
			gotSeq := uint64(0)
			if info.Tx != nil {
				gotSeq = info.Tx.Sequence
			}
			t.Fatalf("pending[%d] sequence = %d, want %d", i, gotSeq, w.seq)
		}
		if info.Note != w.note {
			t.Fatalf("pending[%d] (seq %d) note = %q, want %q", i, w.seq, info.Note, w.note)
		}
		wantStatus := StatusQueued
		if w.note == "waiting-vote" {
			wantStatus = StatusProposed
		}
		if info.Status != wantStatus {
			t.Fatalf("pending[%d] (seq %d) status = %s, want %s", i, w.seq, info.Status, wantStatus)
		}
	}
}

func pendingIDs(acct *AccountInfo) []string {
	ids := make([]string, 0, len(acct.Pending))
	for _, p := range acct.Pending {
		ids = append(ids, p.ID)
	}
	return ids
}

// confirmAccountPrefix 用若干轮确认把单个账户序号 1..n 的连续交易确认掉，使该
// 账户的已确认序号推进到 n（按节点单块上限分批，每批占满一块）。返回确认后的
// 当前轮次。
func confirmAccountPrefix(t *testing.T, n *Node, keys []testKey, who int, maxSeq uint64) uint64 {
	t.Helper()
	for seq := uint64(1); seq <= maxSeq; seq++ {
		tx := NewTransaction(keys[who].priv, seq, []byte(fmt.Sprintf("seq-%d", seq)), 1, 100)
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit seq %d: %v", seq, err)
		}
	}
	confirmed := uint64(0)
	for confirmed < maxSeq {
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		want := n.st.MaxTxs
		if remain := maxSeq - confirmed; remain < want {
			want = remain
		}
		if uint64(len(p.TxIDs)) != want {
			t.Fatalf("setup: proposal packed %d txs, want %d (%v)", len(p.TxIDs), want, p.TxIDs)
		}
		confirmByVotes(t, n, keys, p.BlockID)
		confirmed += uint64(len(p.TxIDs))
	}
	if got := n.Account(keys[who].pub).ConfirmedSequence; got != maxSeq {
		t.Fatalf("setup: confirmed sequence = %d, want %d", got, maxSeq)
	}
	return n.CurrentRound()
}

// TestAccountEarliestGapMultipleGapsRegression 钉住纯排队场景：账户已确认到
// 序号 4，随后只接收 6、8、10（同时存在 5/7/9 三处缺口），查询必须始终按
// 6、8、10 列出全部待处理交易并指出最早缺口 5；逐笔补交 5、7、9 时缺口依次
// 移到 7、9，最后变为 0。费用高低与提交先后不能改变次序与缺口，更高序号的
// 交易不能因前面有缺口而从待处理列表中消失；末尾尚未提交的下一笔不算缺口。
func TestAccountEarliestGapMultipleGapsRegression(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 先把该账户确认到序号 4（产生真实的确认记录，而非从零序号开始）。
	round := confirmAccountPrefix(t, n, keys, 0, 4)
	if round != 2 {
		t.Fatalf("setup: current round = %d, want 2", round)
	}

	// 故意打乱提交先后（10、6、8）并让费用与序号反向（序号越大费用越低）：
	// 次序与缺口都只能取决于序号，不能取决于费用或提交顺序。
	tx10 := NewTransaction(keys[0].priv, 10, []byte("ten"), 2, 100)
	tx6 := NewTransaction(keys[0].priv, 6, []byte("six"), 30, 100)
	tx8 := NewTransaction(keys[0].priv, 8, []byte("eight"), 15, 100)
	for _, tx := range []*Transaction{tx10, tx6, tx8} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 已确认序号仍为 4；待处理按 6、8、10 排列（高费用的 6 也不会排到更前，
	// 先提交的 10 也不会排到更前），三处缺口中最早的一处是 5。
	assertAccountGapView(t, n, keys[0].pub, 4, 5, []pendingExpect{
		{6, tx6.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 补交 5：5、6 连续，最早缺口移到 7；8、10 仍完整保留在列表中。
	tx5 := NewTransaction(keys[0].priv, 5, []byte("five"), 1, 100)
	if _, err := n.Submit(tx5); err != nil {
		t.Fatal(err)
	}
	assertAccountGapView(t, n, keys[0].pub, 4, 7, []pendingExpect{
		{5, tx5.ID(), "waiting-pack"},
		{6, tx6.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 补交 7：缺口移到 9。
	tx7 := NewTransaction(keys[0].priv, 7, []byte("seven"), 1, 100)
	if _, err := n.Submit(tx7); err != nil {
		t.Fatal(err)
	}
	assertAccountGapView(t, n, keys[0].pub, 4, 9, []pendingExpect{
		{5, tx5.ID(), "waiting-pack"},
		{6, tx6.ID(), "waiting-pack"},
		{7, tx7.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 补交 9：5 到 10 连续，缺口归 0；列表末尾没有更多交易，也不凭空报缺口。
	tx9 := NewTransaction(keys[0].priv, 9, []byte("nine"), 1, 100)
	if _, err := n.Submit(tx9); err != nil {
		t.Fatal(err)
	}
	assertAccountGapView(t, n, keys[0].pub, 4, 0, []pendingExpect{
		{5, tx5.ID(), "waiting-pack"},
		{6, tx6.ID(), "waiting-pack"},
		{7, tx7.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{9, tx9.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 全过程没有提议：轮次与已确认序号保持不变，所有交易仍只是排队。
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2 (submits must not advance rounds)", n.CurrentRound())
	}
}

// TestAccountGapAcrossFrozenProposalRegression 钉住待处理列表同时包含等待投票
// 与等待打包交易时的缺口计算：单块上限两笔，账户确认到 4、池内有 5、6、8、10
// （缺口 7）时产生本地提议，5、6 进入候选等待投票，8、10 排队等待打包，缺口
// 仍是 7；提议冻结后补交 7、9，缺口依次变为 9、0，已冻结的提议始终只含 5、6。
// 该提议确认后已确认序号推进到 6，待处理只剩 7 至 10，缺口继续为 0——不能因
// 5、6 已离开交易池就把缺口重新报成 5。
func TestAccountGapAcrossFrozenProposalRegression(t *testing.T) {
	keys := genKeys(t, 4)
	// 单块上限两笔：本场景的核心约束。
	n, _ := newTestNode(t, keys, 2)

	round := confirmAccountPrefix(t, n, keys, 0, 4)
	if round != 3 {
		t.Fatalf("setup: current round = %d, want 3 (seqs 1-4 confirm in two max-2 blocks)", round)
	}

	// 先收到 6、8、10，再补齐 5：此刻 5、6 连续，7 缺失，8、10 在更后面。
	tx6 := NewTransaction(keys[0].priv, 6, []byte("six"), 10, 100)
	tx8 := NewTransaction(keys[0].priv, 8, []byte("eight"), 20, 100)
	tx10 := NewTransaction(keys[0].priv, 10, []byte("ten"), 30, 100)
	for _, tx := range []*Transaction{tx6, tx8, tx10} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	tx5 := NewTransaction(keys[0].priv, 5, []byte("five"), 5, 100)
	if _, err := n.Submit(tx5); err != nil {
		t.Fatal(err)
	}
	assertAccountGapView(t, n, keys[0].pub, 4, 7, []pendingExpect{
		{5, tx5.ID(), "waiting-pack"},
		{6, tx6.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 补齐 5、尚未补齐 7 时产生本地提议：按打包规则只能取 5、6（7 缺失使 8
	// 不可确认），8、10 留在池内排队。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{tx5.ID(), tx6.ID()}) {
		t.Fatalf("frozen proposal = %v, want [5,6]", p.TxIDs)
	}

	// 已确认序号仍为 4，缺口仍为 7：5、6 已在候选中等待投票，属于“在场”，
	// 不能被当成缺失；8、10 等待打包，且不能因为挡在缺口之后就被漏掉。
	assertAccountGapView(t, n, keys[0].pub, 4, 7, []pendingExpect{
		{5, tx5.ID(), "waiting-vote"},
		{6, tx6.ID(), "waiting-vote"},
		{8, tx8.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 提议产生后补交 7：缺口移到 9，7 只能排队等待打包；冻结的提议仍只含 5、6。
	tx7 := NewTransaction(keys[0].priv, 7, []byte("seven"), 40, 100)
	if _, err := n.Submit(tx7); err != nil {
		t.Fatal(err)
	}
	assertAccountGapView(t, n, keys[0].pub, 4, 9, []pendingExpect{
		{5, tx5.ID(), "waiting-vote"},
		{6, tx6.ID(), "waiting-vote"},
		{7, tx7.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})
	if p2, ok := n.Proposal(); !ok || p2.BlockID != p.BlockID ||
		fmt.Sprint(p2.TxIDs) != fmt.Sprint([]string{tx5.ID(), tx6.ID()}) {
		t.Fatalf("proposal must stay frozen at [5,6], got %+v ok=%v", p2, ok)
	}

	// 再补交 9：5 到 10 连续，缺口归 0；5、6 仍等待投票，其余等待打包。
	tx9 := NewTransaction(keys[0].priv, 9, []byte("nine"), 50, 100)
	if _, err := n.Submit(tx9); err != nil {
		t.Fatal(err)
	}
	assertAccountGapView(t, n, keys[0].pub, 4, 0, []pendingExpect{
		{5, tx5.ID(), "waiting-vote"},
		{6, tx6.ID(), "waiting-vote"},
		{7, tx7.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{9, tx9.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})
	if p2, ok := n.Proposal(); !ok || p2.BlockID != p.BlockID || len(p2.TxIDs) != 2 {
		t.Fatalf("proposal must remain the frozen [5,6], got %+v ok=%v", p2, ok)
	}

	// 投票确认冻结的提议：已确认序号推进到 6，5、6 离开交易池；待处理只剩
	// 7 至 10 且全部等待打包。缺口必须继续为 0——5、6 已确认而非缺失，
	// 不能因为它们离开池子就重新报缺口 5。
	confirmByVotes(t, n, keys, p.BlockID)
	if n.CurrentRound() != 4 {
		t.Fatalf("round after confirm = %d, want 4", n.CurrentRound())
	}
	assertAccountGapView(t, n, keys[0].pub, 6, 0, []pendingExpect{
		{7, tx7.ID(), "waiting-pack"},
		{8, tx8.ID(), "waiting-pack"},
		{9, tx9.ID(), "waiting-pack"},
		{10, tx10.ID(), "waiting-pack"},
	})

	// 5、6 已确认并关联确认块（前两块确认 1-4，本块为高度 3），不再出现在
	// 待处理列表；7 至 10 仍是排队交易。
	for i, tx := range []*Transaction{tx5, tx6} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 3 {
			t.Fatalf("tx %d status = %s height = %d, want confirmed at height 3", i+5, info.Status, info.BlockHeight)
		}
	}
	for _, tx := range []*Transaction{tx7, tx8, tx9, tx10} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued {
			t.Fatalf("seq %d status = %s, want queued after the frozen proposal confirms", tx.Sequence, info.Status)
		}
	}
}

// TestAccountGapZeroEmptyAndContiguousRegression 钉住缺口归 0 的两个边界：
// 没有待处理交易的账户返回空列表与缺口 0（无论它从未提交过，还是交易已全部
// 确认）；从已确认序号加一开始连续排队、末尾没有更多交易的账户，缺口同样为 0
// ——待处理列表尽头尚未提交的下一笔不算缺口。
func TestAccountGapZeroEmptyAndContiguousRegression(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 从未出现过的账户：已确认序号 0、空（非 nil）待处理列表、缺口 0。
	acct := n.Account(keys[3].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 || len(acct.Pending) != 0 {
		t.Fatalf("untouched account = confirmed %d gap %d pending %d, want 0/0/0",
			acct.ConfirmedSequence, acct.Gap, len(acct.Pending))
	}
	if acct.Pending == nil {
		t.Fatal("pending list must be an empty slice, not nil")
	}

	// 全部交易确认完毕、池中一笔不剩：同样空列表、缺口 0。
	confirmAccountPrefix(t, n, keys, 0, 2)
	acct = n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 2 || acct.Gap != 0 || len(acct.Pending) != 0 {
		t.Fatalf("fully confirmed account = confirmed %d gap %d pending %d, want 2/0/0",
			acct.ConfirmedSequence, acct.Gap, len(acct.Pending))
	}

	// 从零序号起连续排队（1、2 都在），末尾没有更多交易：缺口 0。
	tx1 := NewTransaction(keys[1].priv, 1, []byte("one"), 1, 100)
	tx2 := NewTransaction(keys[1].priv, 2, []byte("two"), 1, 100)
	for _, tx := range []*Transaction{tx1, tx2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	assertAccountGapView(t, n, keys[1].pub, 0, 0, []pendingExpect{
		{1, tx1.ID(), "waiting-pack"},
		{2, tx2.ID(), "waiting-pack"},
	})

	// 已有确认记录后，从已确认序号加一起连续排队、其后再无交易：缺口仍为 0。
	// 账户 0 已确认到 2，再排队 3、4；不存在序号 5 的交易也不算缺口。
	tx3 := NewTransaction(keys[0].priv, 3, []byte("three"), 1, 100)
	tx4 := NewTransaction(keys[0].priv, 4, []byte("four"), 1, 100)
	for _, tx := range []*Transaction{tx3, tx4} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	assertAccountGapView(t, n, keys[0].pub, 2, 0, []pendingExpect{
		{3, tx3.ID(), "waiting-pack"},
		{4, tx4.ID(), "waiting-pack"},
	})
}
