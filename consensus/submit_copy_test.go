package consensus

import (
	"fmt"
	"testing"
)

// 提交成功后调用方原地改写手中对象的字节（发送者公钥、内容、签名）并改动
// 序号、费用、到期轮次：节点接受的始终是提交成功时的内容。按成功返回的标识
// 查询仍得到原交易，各字段保持接受时的值，标识与完整交易相符，签名仍有效；
// 账户查询仍把它列在原发送者名下，保持原序号、待处理状态和原有缺口说明，
// 不会搬到其他账户，也不会凭空产生第二条待处理记录。
func TestSubmitInputMutationDoesNotLeak(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("one"), 3, 100)
	tx3 := NewTransaction(keys[0].priv, 3, []byte("three"), 4, 100) // 序号 2 留缺口
	want1 := NewTransaction(keys[0].priv, 1, []byte("one"), 3, 100)
	want3 := NewTransaction(keys[0].priv, 3, []byte("three"), 4, 100)
	res1, err := n.Submit(tx1)
	if err != nil {
		t.Fatal(err)
	}
	res3, err := n.Submit(tx3)
	if err != nil {
		t.Fatal(err)
	}
	id1, id3 := want1.ID(), want3.ID()
	if res1.TxID != id1 || res3.TxID != id3 {
		t.Fatalf("submit ids = %s, %s, want %s, %s", res1.TxID, res3.TxID, id1, id3)
	}

	// 调用方原地改写手中两份对象：公钥、内容、签名字节逐位翻转，
	// 序号、费用、到期轮次全部改动（含把 tx3 序号改到缺口上）。
	tx1.Sender[0] ^= 0xff
	tx1.Content[0] ^= 0xff
	tx1.Signature[0] ^= 0xff
	tx1.Sequence = 9
	tx1.Fee = 9999
	tx1.Expiry = 1
	tx3.Sender[0] ^= 0xff
	tx3.Content = append(tx3.Content, "tampered"...)
	tx3.Signature[0] ^= 0xff
	tx3.Sequence = 2
	tx3.Fee = 0
	tx3.Expiry = 1

	// 按提交成功时返回的标识查询，仍是接受时的完整交易。
	for id, want := range map[string]*Transaction{id1: want1, id3: want3} {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusQueued {
			t.Fatalf("status after caller mutation = %s, want %s", info.Status, StatusQueued)
		}
		if info.ID != id || info.Tx.ID() != id {
			t.Fatalf("queried tx id = %s / %s, want %s", info.ID, info.Tx.ID(), id)
		}
		assertTxMatches(t, info.Tx, want)
	}

	// 账户查询仍把两笔交易列在原发送者名下：原序号、待处理状态、原有缺口说明。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 2 || len(acct.Pending) != 2 {
		t.Fatalf("account after caller mutation wrong: %+v", acct)
	}
	if acct.Pending[0].ID != id1 || acct.Pending[1].ID != id3 {
		t.Fatalf("pending ids = %s, %s, want %s, %s",
			acct.Pending[0].ID, acct.Pending[1].ID, id1, id3)
	}
	for i, want := range []*Transaction{want1, want3} {
		if acct.Pending[i].Note != "waiting-pack" {
			t.Fatalf("pending[%d] note = %s, want waiting-pack", i, acct.Pending[i].Note)
		}
		assertTxMatches(t, acct.Pending[i].Tx, want)
	}
	// 被改写的序号没有凭空产生第三条待处理记录，交易也没有搬到其他账户。
	if len(n.Account(tx1.Sender).Pending) != 0 || len(n.Account(tx3.Sender).Pending) != 0 {
		t.Fatal("mutated sender bytes must not own any pending transaction")
	}
	for _, k := range keys[1:] {
		if a := n.Account(k.pub); len(a.Pending) != 0 {
			t.Fatalf("account %s should be untouched: %+v", a.Sender, a)
		}
	}

	// 正常保存后重开，节点中仍是接受时的交易。
	txOther := NewTransaction(keys[1].priv, 1, []byte("other"), 1, 100)
	if _, err := n.Submit(txOther); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.Tx(id1)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, stored.Tx, want1)
	storedAcct := reopened.Account(keys[0].pub)
	if storedAcct.Gap != 2 || len(storedAcct.Pending) != 2 {
		t.Fatalf("account after reopen wrong: %+v", storedAcct)
	}
}

// 调用方把手中对象的发送者、内容、签名整体换成另一份值（而非逐字节改写）：
// 同样只改变调用方手中的对象，节点仍保留提交时接受的交易，
// 交易不会搬到新发送者名下，换出来的标识在节点中不存在。
func TestSubmitInputFieldReplacementDoesNotLeak(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("alpha"), 3, 100)
	want := NewTransaction(keys[0].priv, 1, []byte("alpha"), 3, 100)
	res, err := n.Submit(tx)
	if err != nil {
		t.Fatal(err)
	}
	id := want.ID()
	if res.TxID != id {
		t.Fatalf("submit id = %s, want %s", res.TxID, id)
	}

	// 把对象里的发送者、内容、签名整体替换为另一份值。
	replacement := NewTransaction(keys[1].priv, 7, []byte("replacement"), 9, 200)
	tx.Sender = append([]byte(nil), keys[1].pub...)
	tx.Sequence = 7
	tx.Content = append([]byte(nil), replacement.Content...)
	tx.Fee = 9
	tx.Expiry = 200
	tx.Signature = append([]byte(nil), replacement.Signature...)
	replacedID := tx.ID()
	if replacedID == id {
		t.Fatal("setup: replaced fields should change the computed id")
	}

	// 节点中仍是提交时接受的交易。
	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued || info.ID != id {
		t.Fatalf("tx after field replacement wrong: %+v", info)
	}
	assertTxMatches(t, info.Tx, want)

	// 交易没有搬到新发送者名下，也没有第二条待处理记录。
	if a := n.Account(keys[1].pub); a.ConfirmedSequence != 0 || len(a.Pending) != 0 || a.Gap != 0 {
		t.Fatalf("replacement sender account should be untouched: %+v", a)
	}
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 || acct.Pending[0].ID != id {
		t.Fatalf("original account wrong after field replacement: %+v", acct)
	}
	assertTxMatches(t, acct.Pending[0].Tx, want)

	// 换出来的标识在节点中不存在。
	if _, err := n.Tx(replacedID); reason(err) != ReasonUnknownTx {
		t.Fatalf("replaced id got %v, want %s", err, ReasonUnknownTx)
	}
}

// 打包顺序由接受时的费用与交易标识决定：提交成功后调用方提高或降低手中
// 对象的费用，不改变原先的打包顺序；确认块包含的仍是成功提交返回的原标识。
func TestSubmitInputFeeEditsDoNotChangePackingOrder(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	txA := NewTransaction(keys[0].priv, 1, []byte("a"), 3, 100)
	txB := NewTransaction(keys[1].priv, 1, []byte("b"), 7, 100)
	if _, err := n.Submit(txA); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(txB); err != nil {
		t.Fatal(err)
	}
	idA, idB := txA.ID(), txB.ID()

	// 事后提高 txA 费用、降低 txB 费用，试图颠倒打包顺序。
	txA.Fee = 1000
	txB.Fee = 0

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{idB, idA}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal order = %v, want %v (caller fee edits must not leak)", p.TxIDs, want)
	}

	// 确认块包含的仍是成功提交返回的原标识、原顺序。
	confirmByVotes(t, n, keys, p.BlockID)
	block, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(block.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("confirmed block txs = %v, want %v", block.TxIDs, want)
	}
}

// 事后改动手中对象的序号，不能让原交易越过序号缺口提前打包：
// 池内序号与缺口说明保持接受时的样子，打包仍从已确认序号加一开始连续。
func TestSubmitInputSequenceEditsDoNotJumpGap(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("one"), 1, 100)
	tx3 := NewTransaction(keys[0].priv, 3, []byte("three"), 5, 100) // 序号 2 留缺口
	if _, err := n.Submit(tx1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(tx3); err != nil {
		t.Fatal(err)
	}
	id1, id3 := tx1.ID(), tx3.ID()

	// 事后把 tx3 的序号改到缺口上，试图让它提前打包。
	tx3.Sequence = 2

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{id1}) {
		t.Fatalf("proposal = %v, want [%s] (caller sequence edits must not fill the gap)", p.TxIDs, id1)
	}
	acct := n.Account(keys[0].pub)
	if acct.Gap != 2 || len(acct.Pending) != 2 || acct.Pending[1].ID != id3 {
		t.Fatalf("account gap changed by caller edit: %+v", acct)
	}
}

// 到期轮次以接受时为准：事后改长不能延长有效期，改短也不能让原交易提前失效。
func TestSubmitInputExpiryEditsDoNotLeak(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	txShort := NewTransaction(keys[0].priv, 1, []byte("short"), 1, 2)
	txLong := NewTransaction(keys[1].priv, 1, []byte("long"), 1, 100)
	wantLong := NewTransaction(keys[1].priv, 1, []byte("long"), 1, 100)
	if _, err := n.Submit(txShort); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(txLong); err != nil {
		t.Fatal(err)
	}
	idShort, idLong := txShort.ID(), txLong.ID()

	// 事后把短效交易改成长效、把长效交易改成立即到期。
	txShort.Expiry = 100
	txLong.Expiry = 1

	// 结束本轮进入第 2 轮：短效交易按原到期轮次失效，长效交易保持排队。
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	short, err := n.Tx(idShort)
	if err != nil {
		t.Fatal(err)
	}
	if short.Status != StatusExpired {
		t.Fatalf("short-expiry tx status = %s, want %s (caller expiry extension must not leak)", short.Status, StatusExpired)
	}
	long, err := n.Tx(idLong)
	if err != nil {
		t.Fatal(err)
	}
	if long.Status != StatusQueued {
		t.Fatalf("long-expiry tx status = %s, want %s (caller expiry cut must not leak)", long.Status, StatusQueued)
	}
	assertTxMatches(t, long.Tx, wantLong)
}

// 原本能够打包且尚未到期的交易，确认流程继续使用节点接受时的交易：
// 确认块包含成功提交返回的原标识，交易查询关联实际确认的区块，
// 账户已确认序号按原交易推进；调用方事后对输入对象的任何改写都不影响结果。
func TestSubmitInputEditsDoNotAffectConfirmation(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("confirm-me"), 5, 100)
	want := NewTransaction(keys[0].priv, 1, []byte("confirm-me"), 5, 100)
	res, err := n.Submit(tx)
	if err != nil {
		t.Fatal(err)
	}
	id := want.ID()
	if res.TxID != id {
		t.Fatalf("submit id = %s, want %s", res.TxID, id)
	}

	// 提交成功后尽力改写手中的原对象。
	tx.Sender[0] ^= 0xff
	tx.Sequence = 9
	tx.Content = []byte("hijacked")
	tx.Fee = 0
	tx.Expiry = 1
	tx.Signature[0] ^= 0xff

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{id}) {
		t.Fatalf("proposal = %v, want [%s]", p.TxIDs, id)
	}
	confirmByVotes(t, n, keys, p.BlockID)

	// 确认块包含原标识；交易查询关联实际确认的区块。
	block, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(block.TxIDs) != fmt.Sprint([]string{id}) {
		t.Fatalf("confirmed block txs = %v, want [%s]", block.TxIDs, id)
	}
	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockHeight != block.Height || info.BlockID != block.ID {
		t.Fatalf("confirmed tx info wrong: %+v", info)
	}
	assertTxMatches(t, info.Tx, want)

	// 账户已确认序号按原交易推进，没有遗留待处理记录。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 1 || len(acct.Pending) != 0 || acct.Gap != 0 {
		t.Fatalf("account after confirm wrong: %+v", acct)
	}
}

// 空内容同样是合法交易：提交后再给原对象填入内容，节点中仍保留原来的
// 空内容、标识和有效签名；把修改过、签名已不匹配的对象再次提交，按现有
// 规则以 bad-signature 拒绝，之前接受的交易及其查询结果保持原样。
func TestEmptyContentSubmitInputStaysIntact(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, nil, 2, 100)
	want := NewTransaction(keys[0].priv, 1, nil, 2, 100)
	res, err := n.Submit(tx)
	if err != nil {
		t.Fatal(err)
	}
	id := want.ID()
	if res.TxID != id {
		t.Fatalf("submit id = %s, want %s", res.TxID, id)
	}

	// 提交成功后给手中的原对象填入内容。
	tx.Content = []byte("filled-after-submit")
	filledID := tx.ID()
	if filledID == id {
		t.Fatal("setup: filled content should change the computed id")
	}

	// 节点中仍保留原来的空内容、标识和有效签名。
	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != id || len(info.Tx.Content) != 0 {
		t.Fatalf("empty-content tx changed by caller fill: %+v", info)
	}
	assertTxMatches(t, info.Tx, want)
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 1 || acct.Pending[0].ID != id || len(acct.Pending[0].Tx.Content) != 0 {
		t.Fatalf("account pending changed by caller fill: %+v", acct.Pending)
	}

	// 修改过的对象签名已不再匹配，再次提交按 bad-signature 拒绝。
	if _, err := n.Submit(tx); reason(err) != ReasonBadSignature {
		t.Fatalf("resubmit of mutated tx got %v, want %s", err, ReasonBadSignature)
	}
	// 填入内容后算出的标识没有成为节点中的另一笔交易。
	if _, err := n.Tx(filledID); reason(err) != ReasonUnknownTx {
		t.Fatalf("filled id got %v, want %s", err, ReasonUnknownTx)
	}
	// 之前接受的交易及其查询结果保持原样；原样重报仍是重复交易。
	again, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, again.Tx, want)
	if _, err := n.Submit(want); reason(err) != ReasonDuplicate {
		t.Fatalf("resubmit of untouched tx got %v, want %s", err, ReasonDuplicate)
	}
	if a := n.Account(keys[0].pub); len(a.Pending) != 1 || a.Pending[0].ID != id {
		t.Fatalf("account pending changed after rejected resubmit: %+v", a.Pending)
	}
}
