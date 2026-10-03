package consensus

import (
	"crypto/ed25519"
	"fmt"
	"testing"
)

// craftTxIDBelow 生成一笔交易，通过调整内容使其标识按字典序小于 bound。
// 用于构造与具体密钥无关的确定性同费比较场景。
func craftTxIDBelow(t *testing.T, priv ed25519.PrivateKey, seq, fee uint64, bound string) *Transaction {
	t.Helper()
	for i := 0; ; i++ {
		tx := NewTransaction(priv, seq, []byte(fmt.Sprintf("craft-%d", i)), fee, 100)
		if tx.ID() < bound {
			return tx
		}
	}
}

// 同账户前一笔选入后，后一笔才参与剩余位置的竞争：
// 甲1(30)/甲2(100)、乙1(50)/乙2(20)，上限 4 时完整顺序为 乙1、甲1、甲2、乙2——
// 甲2 开始时不能越过甲1，甲1 入选后甲2 凭更高费用排在乙2 之前；
// 上限 3 时提议只含前三笔，乙2 留在池中等待打包。
// 提议生成本身不推进任何账户的已确认序号。
func TestPackingUnlocksNextSequence(t *testing.T) {
	keys := genKeys(t, 4)
	// 甲乙两个账户各两笔交易，费用交叉：甲1(30) < 乙1(50) < 甲2(100)，乙2(20) 最低。
	build := func(maxTxs uint64) (*Node, []*Transaction) {
		n, _ := newTestNode(t, keys, maxTxs)
		jia1 := NewTransaction(keys[0].priv, 1, []byte("jia1"), 30, 100)
		jia2 := NewTransaction(keys[0].priv, 2, []byte("jia2"), 100, 100)
		yi1 := NewTransaction(keys[1].priv, 1, []byte("yi1"), 50, 100)
		yi2 := NewTransaction(keys[1].priv, 2, []byte("yi2"), 20, 100)
		txs := []*Transaction{jia1, jia2, yi1, yi2}
		for _, tx := range txs {
			if _, err := n.Submit(tx); err != nil {
				t.Fatal(err)
			}
		}
		return n, txs
	}

	// 上限 4：全部入选，完整顺序 乙1、甲1、甲2、乙2。
	n4, txs := build(4)
	jia1, jia2, yi1, yi2 := txs[0], txs[1], txs[2], txs[3]
	p4, err := n4.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want4 := []string{yi1.ID(), jia1.ID(), jia2.ID(), yi2.ID()}
	if fmt.Sprint(p4.TxIDs) != fmt.Sprint(want4) {
		t.Fatalf("maxTxs=4 packed %v, want %v", p4.TxIDs, want4)
	}
	// 提议不推进已确认序号；四笔均等待投票。
	for i, k := range keys[:2] {
		if acct := n4.Account(k.pub); acct.ConfirmedSequence != 0 {
			t.Fatalf("account %d confirmed seq = %d after propose, want 0", i, acct.ConfirmedSequence)
		}
	}
	for _, tx := range txs {
		info, err := n4.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusProposed {
			t.Fatalf("tx %s status = %s, want proposed", tx.ID(), info.Status)
		}
	}
	for i, k := range keys[:2] {
		acct := n4.Account(k.pub)
		if len(acct.Pending) != 2 {
			t.Fatalf("account %d pending = %d, want 2", i, len(acct.Pending))
		}
		for _, pend := range acct.Pending {
			if pend.Status != StatusProposed || pend.Note != "waiting-vote" {
				t.Fatalf("account %d pending %+v, want proposed/waiting-vote", i, pend)
			}
		}
	}

	// 上限 3：提议只含 乙1、甲1、甲2，乙2 仍在池中排队等待打包。
	n3, txs3 := build(3)
	p3, err := n3.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want3 := []string{txs3[2].ID(), txs3[0].ID(), txs3[1].ID()}
	if fmt.Sprint(p3.TxIDs) != fmt.Sprint(want3) {
		t.Fatalf("maxTxs=3 packed %v, want %v", p3.TxIDs, want3)
	}
	info, err := n3.Tx(txs3[3].ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("yi2 status = %s, want queued", info.Status)
	}
	yi := n3.Account(keys[1].pub)
	if yi.ConfirmedSequence != 0 {
		t.Fatalf("yi confirmed seq = %d after propose, want 0", yi.ConfirmedSequence)
	}
	if len(yi.Pending) != 2 || yi.Pending[0].Note != "waiting-vote" || yi.Pending[1].Note != "waiting-pack" {
		t.Fatalf("yi pending notes wrong: %+v", yi.Pending)
	}
	if yi.Pending[1].ID != txs3[3].ID() || yi.Pending[1].Status != StatusQueued {
		t.Fatalf("yi2 should remain queued/waiting-pack: %+v", yi.Pending[1])
	}
}

// 同费比较发生在交易实际具备入选条件时：
// 甲1 选入后甲2 刚成为可选，与乙的下一笔同费时按标识字典序取较小者，
// 不因为刚具备条件而固定排后；尚未具备条件的甲3 即使标识最小也不能提前占位。
func TestPackingTieBreakOnNewlyEligible(t *testing.T) {
	keys := genKeys(t, 4)

	// 情形一：甲2 标识小于乙1，同费时甲2 一具备条件即胜出，最终 甲1、甲2、甲3。
	n1, _ := newTestNode(t, keys, 3)
	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 100, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 50, 100)
	a2 := craftTxIDBelow(t, keys[0].priv, 2, 50, b1.ID()) // 与 b1 同费，标识更小
	a3 := craftTxIDBelow(t, keys[0].priv, 3, 50, a2.ID()) // 标识最小，但需等 a2 入选
	for _, tx := range []*Transaction{a1, b1, a2, a3} {
		if _, err := n1.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := n1.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want1 := []string{a1.ID(), a2.ID(), a3.ID()}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint(want1) {
		t.Fatalf("packed %v, want %v (a2 wins the tie once eligible; a3 cannot jump ahead)", p1.TxIDs, want1)
	}
	if info, _ := n1.Tx(b1.ID()); info.Status != StatusQueued {
		t.Fatalf("b1 status = %s, want queued", info.Status)
	}
	if acct := n1.Account(keys[1].pub); len(acct.Pending) != 1 || acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("b1 should remain waiting-pack: %+v", acct.Pending)
	}

	// 情形二：乙1 标识更小，同费时乙1 胜出；甲3 标识最小但仍不能提前占位。
	n2, _ := newTestNode(t, keys, 3)
	c1 := NewTransaction(keys[0].priv, 1, []byte("c1"), 100, 100)
	d2 := NewTransaction(keys[0].priv, 2, []byte("d2"), 50, 100)
	d3 := craftTxIDBelow(t, keys[0].priv, 3, 50, d2.ID())
	e1 := craftTxIDBelow(t, keys[1].priv, 1, 50, d3.ID()) // 与 d2/d3 同费，标识最小
	for _, tx := range []*Transaction{c1, d2, d3, e1} {
		if _, err := n2.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p2, err := n2.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want2 := []string{c1.ID(), e1.ID(), d2.ID()}
	if fmt.Sprint(p2.TxIDs) != fmt.Sprint(want2) {
		t.Fatalf("packed %v, want %v (tie broken by id, not by eligibility order)", p2.TxIDs, want2)
	}
	if info, _ := n2.Tx(d3.ID()); info.Status != StatusQueued {
		t.Fatalf("d3 status = %s, want queued", info.Status)
	}
}

// 相同的有效交易集合，提交顺序不同（无替换、无容量淘汰），
// 在轮次、已确认历史与配置一致时，最终交易顺序与区块标识不变。
func TestPackingIndependentOfSubmissionOrder(t *testing.T) {
	keys := genKeys(t, 4)
	jia1 := NewTransaction(keys[0].priv, 1, []byte("jia1"), 30, 100)
	jia2 := NewTransaction(keys[0].priv, 2, []byte("jia2"), 100, 100)
	yi1 := NewTransaction(keys[1].priv, 1, []byte("yi1"), 50, 100)
	yi2 := NewTransaction(keys[1].priv, 2, []byte("yi2"), 20, 100)
	txs := []*Transaction{jia1, jia2, yi1, yi2}

	orders := [][]int{
		{0, 1, 2, 3}, // 正序
		{3, 2, 1, 0}, // 逆序
		{2, 0, 3, 1}, // 交错
	}
	want := []string{yi1.ID(), jia1.ID(), jia2.ID(), yi2.ID()}
	var wantBlock string
	for _, ord := range orders {
		n, _ := newTestNode(t, keys, 4)
		for _, i := range ord {
			if _, err := n.Submit(txs[i]); err != nil {
				t.Fatal(err)
			}
		}
		p, err := n.Propose()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
			t.Fatalf("submit order %v packed %v, want %v", ord, p.TxIDs, want)
		}
		if wantBlock == "" {
			wantBlock = p.BlockID
		} else if p.BlockID != wantBlock {
			t.Fatalf("submit order %v block id %s, want %s", ord, p.BlockID, wantBlock)
		}
	}
}

// 序号缺口边界：账户缺少下一条应确认的交易时，更高序号的交易无论费用多高
// 都不能入选，其他账户仍可正常竞争；没有任何账户提供可选交易时产生空块。
func TestPackingSequenceGap(t *testing.T) {
	keys := genKeys(t, 4)

	// 丙缺序号 1：丙2 费用最高也不能入选；丁正常参与竞争。
	n, _ := newTestNode(t, keys, 4)
	c2 := NewTransaction(keys[0].priv, 2, []byte("c2"), 1000, 100)
	d1 := NewTransaction(keys[1].priv, 1, []byte("d1"), 1, 100)
	for _, tx := range []*Transaction{c2, d1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{d1.ID()}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("packed %v, want %v (gap blocks c2 despite highest fee)", p.TxIDs, want)
	}
	if info, _ := n.Tx(c2.ID()); info.Status != StatusQueued {
		t.Fatalf("c2 status = %s, want queued", info.Status)
	}
	acct := n.Account(keys[0].pub)
	if acct.Gap != 1 || len(acct.Pending) != 1 || acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("c2 should wait on gap 1: %+v", acct)
	}

	// 池中只有等待缺口的交易：没有任何账户提供可选交易，产生空块。
	n2, _ := newTestNode(t, keys, 4)
	if _, err := n2.Submit(NewTransaction(keys[0].priv, 2, []byte("c2"), 1000, 100)); err != nil {
		t.Fatal(err)
	}
	p2, err := n2.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Empty || len(p2.TxIDs) != 0 {
		t.Fatalf("want empty proposal, got %+v", p2)
	}
	if wantEmpty := BlockID(1, 1, "", []string{}); p2.BlockID != wantEmpty {
		t.Fatalf("empty block id = %s, want %s", p2.BlockID, wantEmpty)
	}
}

// 已确认序号不为 0 的账户从其下一序号开始参与打包，不能重新从 1 取交易；
// 提议生成不推进已确认序号，确认后才推进。
func TestPackingFromConfirmedSequence(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 4)

	// 第一轮：戊的序号 1、2 进入提议并被确认。
	e1 := NewTransaction(keys[0].priv, 1, []byte("e1"), 5, 100)
	e2 := NewTransaction(keys[0].priv, 2, []byte("e2"), 6, 100)
	for _, tx := range []*Transaction{e1, e2} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{e1.ID(), e2.ID()}; fmt.Sprint(p1.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("round 1 packed %v, want %v", p1.TxIDs, want)
	}
	confirmByVotes(t, n, keys, p1.BlockID)
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 2 {
		t.Fatalf("confirmed seq = %d, want 2", acct.ConfirmedSequence)
	}

	// 第二轮：戊从序号 3 开始（戊3(10)、戊4(100)），与己1(50) 竞争。
	// 完整顺序：己1、戊3、戊4——戊4 费用最高也必须等戊3 入选。
	e3 := NewTransaction(keys[0].priv, 3, []byte("e3"), 10, 100)
	e4 := NewTransaction(keys[0].priv, 4, []byte("e4"), 100, 100)
	f1 := NewTransaction(keys[1].priv, 1, []byte("f1"), 50, 100)
	for _, tx := range []*Transaction{e3, e4, f1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want2 := []string{f1.ID(), e3.ID(), e4.ID()}
	if fmt.Sprint(p2.TxIDs) != fmt.Sprint(want2) {
		t.Fatalf("round 2 packed %v, want %v (must continue from confirmed seq 2, not restart at 1)", p2.TxIDs, want2)
	}
	// 提议不推进已确认序号；确认后才推进到 4。
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 2 {
		t.Fatalf("confirmed seq = %d after propose, want 2", acct.ConfirmedSequence)
	}
	confirmByVotes(t, n, keys, p2.BlockID)
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 4 {
		t.Fatalf("confirmed seq = %d after confirm, want 4", acct.ConfirmedSequence)
	}
	if acct := n.Account(keys[1].pub); acct.ConfirmedSequence != 1 {
		t.Fatalf("ji confirmed seq = %d after confirm, want 1", acct.ConfirmedSequence)
	}
}
