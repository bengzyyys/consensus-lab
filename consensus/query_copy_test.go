package consensus

import (
	"bytes"
	"fmt"
	"testing"
)

// assertTxMatches 校验查询返回的交易与提交时的原交易逐字段一致：
// 发送者、序号、内容、费用、到期轮次、签名相同，标识对应，签名仍有效。
func assertTxMatches(t *testing.T, got, want *Transaction) {
	t.Helper()
	if got == nil {
		t.Fatal("query returned nil transaction")
	}
	if !bytes.Equal(got.Sender, want.Sender) ||
		got.Sequence != want.Sequence ||
		!bytes.Equal(got.Content, want.Content) ||
		got.Fee != want.Fee ||
		got.Expiry != want.Expiry ||
		!bytes.Equal(got.Signature, want.Signature) {
		t.Fatalf("tx = {sender:%x seq:%d content:%q fee:%d expiry:%d sig:%x}, want {sender:%x seq:%d content:%q fee:%d expiry:%d sig:%x}",
			got.Sender, got.Sequence, got.Content, got.Fee, got.Expiry, got.Signature,
			want.Sender, want.Sequence, want.Content, want.Fee, want.Expiry, want.Signature)
	}
	if got.ID() != want.ID() {
		t.Fatalf("tx id = %s, want %s", got.ID(), want.ID())
	}
	if !got.Verify() {
		t.Fatal("stored transaction signature no longer verifies")
	}
}

// tamperQueryTx 以调用方身份尽力改写手中查询结果里的交易：
// 发送者公钥、内容、签名、费用、序号、到期轮次全部改写。
func tamperQueryTx(tx *Transaction) {
	tx.Sender[0] ^= 0xff
	tx.Content = append(tx.Content, "tampered"...)
	tx.Signature[0] ^= 0xff
	tx.Fee += 1000
	tx.Sequence += 100
	tx.Expiry = 1 // 改为当前轮次，试图让交易立即到期
}

// 按标识查询与账户待处理查询返回的都是独立副本：调用方改写发送者公钥、内容、
// 签名、费用、序号、到期轮次后，节点再次查询仍返回提交时接收的完整交易，
// 原标识与内容对应、原签名有效、仍属于原账户和原序号；改费用不影响随后
// 打包采用的费用，改到期轮次不影响有效性；篡改也不会随正常保存落盘。
func TestTxQueryResultIsIndependentCopy(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	txA := NewTransaction(keys[0].priv, 1, []byte("alpha"), 3, 100)
	txB := NewTransaction(keys[1].priv, 1, []byte("beta"), 7, 100)
	if _, err := n.Submit(txA); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(txB); err != nil {
		t.Fatal(err)
	}
	idA, idB := txA.ID(), txB.ID()

	// 调用方取得按标识查询的结果并尽力改写。
	info, err := n.Tx(idA)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("setup: status = %s, want %s", info.Status, StatusQueued)
	}
	tamperQueryTx(info.Tx)
	info.ID = "tampered-id"
	info.Status = StatusConfirmed

	// 节点再次按原标识查询，仍是提交时的完整交易。
	got, err := n.Tx(idA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("status after tamper = %s, want %s", got.Status, StatusQueued)
	}
	assertTxMatches(t, got.Tx, txA)

	// 交易仍属于原账户和原序号。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 || acct.Gap != 0 {
		t.Fatalf("account after Tx-result tamper wrong: %+v", acct)
	}
	assertTxMatches(t, acct.Pending[0].Tx, txA)
	if acct.Pending[0].ID != idA || acct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("account pending entry wrong: %+v", acct.Pending[0])
	}

	// 调用方改写账户查询结果中的交易：费用调低、序号改大、内容改写。
	acct.Pending[0].Tx.Fee = 0
	acct.Pending[0].Tx.Sequence = 50
	acct.Pending[0].Tx.Content = []byte("hijacked")
	acct.Pending[0].Tx.Signature[0] ^= 0xff
	acct.ConfirmedSequence = 5

	// 两种查询路径互相不受对方结果的影响。
	got2, err := n.Tx(idA)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, got2.Tx, txA)
	acct2 := n.Account(keys[0].pub)
	if acct2.ConfirmedSequence != 0 || len(acct2.Pending) != 1 {
		t.Fatalf("account after Account-result tamper wrong: %+v", acct2)
	}
	assertTxMatches(t, acct2.Pending[0].Tx, txA)

	// 打包采用的费用不受篡改影响：txB（费用 7）仍排在 txA（费用 3）之前，
	// 即使调用方曾把结果中 txA 的费用改成 1003。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{idB, idA}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("proposal order = %v, want %v (caller fee edits must not leak)", p.TxIDs, want)
	}

	// 到期轮次被改为当前轮次不影响有效性：结束本轮后交易不失效，回到排队。
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	after, err := n.Tx(idA)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusQueued {
		t.Fatalf("status after EndRound = %s, want %s (caller expiry edits must not leak)", after.Status, StatusQueued)
	}
	assertTxMatches(t, after.Tx, txA)

	// 合法提交触发一次正常保存后重开，节点中仍是原交易。
	txC := NewTransaction(keys[2].priv, 1, []byte("c"), 1, 100)
	if _, err := n.Submit(txC); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.Tx(idA)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, stored.Tx, txA)
	storedAcct := reopened.Account(keys[0].pub)
	if storedAcct.ConfirmedSequence != 0 || len(storedAcct.Pending) != 1 {
		t.Fatalf("account after reopen wrong: %+v", storedAcct)
	}
	assertTxMatches(t, storedAcct.Pending[0].Tx, txA)
}

// 同一笔交易可以先后取得多份结果，也可以同时出现在交易查询和账户查询中：
// 编辑其中一份，其他已取得的结果保留各自取得时的内容，新取得的结果保持节点原值。
func TestMultipleTxQueryResultsIndependent(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("shared"), 4, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}
	id := tx.ID()

	first, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 1 {
		t.Fatalf("setup: pending = %d, want 1", len(acct.Pending))
	}
	fromAcct := acct.Pending[0]

	// 编辑先取得的一份，其余已取得的结果保持取得时的内容。
	tamperQueryTx(first.Tx)
	assertTxMatches(t, second.Tx, tx)
	assertTxMatches(t, fromAcct.Tx, tx)

	// 再编辑账户查询取得的一份，另一份与新查询仍保持原值。
	tamperQueryTx(fromAcct.Tx)
	assertTxMatches(t, second.Tx, tx)
	fresh, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	assertTxMatches(t, fresh.Tx, tx)
	freshAcct := n.Account(keys[0].pub)
	if len(freshAcct.Pending) != 1 {
		t.Fatalf("pending after tamper = %d, want 1", len(freshAcct.Pending))
	}
	assertTxMatches(t, freshAcct.Pending[0].Tx, tx)
}

// 账户列表里有多笔交易时，改写其中一笔不影响相邻交易的内容、所属账户或序号；
// 账户的已确认序号、待处理次序和缺口说明也不随调用方编辑而变化。
func TestAccountPendingNeighborsUnaffected(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("one"), 1, 100)
	tx2 := NewTransaction(keys[0].priv, 2, []byte("two"), 2, 100)
	tx4 := NewTransaction(keys[0].priv, 4, []byte("four"), 4, 100) // 序号 3 留缺口
	for _, tx := range []*Transaction{tx1, tx2, tx4} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 3 || len(acct.Pending) != 3 {
		t.Fatalf("setup: account = %+v", acct)
	}
	wantIDs := []string{tx1.ID(), tx2.ID(), tx4.ID()}
	var gotIDs []string
	for _, p := range acct.Pending {
		gotIDs = append(gotIDs, p.ID)
	}
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("setup: pending order = %v, want %v", gotIDs, wantIDs)
	}

	// 改写中间那笔的内容、发送者与序号。
	mid := acct.Pending[1]
	mid.Tx.Content = []byte("hijacked")
	mid.Tx.Sender = append([]byte(nil), keys[1].pub...)
	mid.Tx.Sequence = 7
	mid.Tx.Signature[0] ^= 0xff
	mid.ID = "tampered-id"

	// 相邻交易的内容、所属账户与序号不变，被改写的交易在节点中保持原值。
	again := n.Account(keys[0].pub)
	if again.ConfirmedSequence != 0 || again.Gap != 3 || len(again.Pending) != 3 {
		t.Fatalf("account summary changed by caller edit: %+v", again)
	}
	wantTxs := []*Transaction{tx1, tx2, tx4}
	for i, want := range wantTxs {
		got := again.Pending[i]
		if got.ID != wantIDs[i] {
			t.Fatalf("pending[%d] id = %s, want %s", i, got.ID, wantIDs[i])
		}
		assertTxMatches(t, got.Tx, want)
	}
	// 按标识查询邻居与被改写交易，同样保持提交时的内容。
	for i, want := range wantTxs {
		info, err := n.Tx(wantIDs[i])
		if err != nil {
			t.Fatal(err)
		}
		assertTxMatches(t, info.Tx, want)
	}
	// 被改写交易没有“搬到”其他账户名下。
	other := n.Account(keys[1].pub)
	if other.ConfirmedSequence != 0 || len(other.Pending) != 0 || other.Gap != 0 {
		t.Fatalf("neighbor account should be untouched: %+v", other)
	}
}

// 查询结果是取得时的快照：交易原本排队，调用方取得结果后节点产生提议，
// 新查询显示该交易已进入提议、账户说明变为等待投票，而先前保存的结果仍显示
// 取得时的排队状态和内容；编辑旧结果不改变当前提议的交易标识与顺序。
func TestQueryResultIsSnapshotAcrossProposal(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("snap"), 5, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}
	id := tx.ID()

	// 提议产生前取得查询结果。
	beforeTx, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if beforeTx.Status != StatusQueued {
		t.Fatalf("setup: status = %s, want %s", beforeTx.Status, StatusQueued)
	}
	beforeAcct := n.Account(keys[0].pub)
	if len(beforeAcct.Pending) != 1 || beforeAcct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("setup: account pending = %+v", beforeAcct.Pending)
	}

	// 节点产生提议。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{id}) {
		t.Fatalf("setup: proposal = %v, want [%s]", p.TxIDs, id)
	}

	// 新查询显示交易已进入提议、账户说明变为等待投票。
	afterTx, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if afterTx.Status != StatusProposed {
		t.Fatalf("status after propose = %s, want %s", afterTx.Status, StatusProposed)
	}
	afterAcct := n.Account(keys[0].pub)
	if len(afterAcct.Pending) != 1 || afterAcct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("account note after propose = %+v, want waiting-vote", afterAcct.Pending)
	}

	// 先前保存的结果仍显示取得时的排队状态和内容。
	if beforeTx.Status != StatusQueued {
		t.Fatalf("saved result status changed to %s, want %s", beforeTx.Status, StatusQueued)
	}
	assertTxMatches(t, beforeTx.Tx, tx)
	if beforeAcct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("saved account note changed to %s, want waiting-pack", beforeAcct.Pending[0].Note)
	}
	assertTxMatches(t, beforeAcct.Pending[0].Tx, tx)

	// 编辑旧结果，当前提议的交易标识与顺序保持原样。
	tamperQueryTx(beforeTx.Tx)
	beforeTx.ID = "tampered-id"
	tamperQueryTx(beforeAcct.Pending[0].Tx)
	again, ok := n.Proposal()
	if !ok {
		t.Fatal("proposal should exist")
	}
	if fmt.Sprint(again.TxIDs) != fmt.Sprint([]string{id}) || again.BlockID != p.BlockID {
		t.Fatalf("proposal changed by caller edit: %+v", again)
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 1 || fmt.Sprint(rc.Candidates[0].TxIDs) != fmt.Sprint([]string{id}) {
		t.Fatalf("candidate history changed by caller edit: %+v", rc.Candidates)
	}
}

// 内容为空也是合法交易：查询保留它的标识、签名和其他字段；
// 调用方给这份结果填入内容，不会把原交易变成另一笔交易。
func TestEmptyContentTxQueryResultStaysIntact(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, nil, 2, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}
	id := tx.ID()

	info, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != id || len(info.Tx.Content) != 0 {
		t.Fatalf("empty-content tx query wrong: %+v", info)
	}
	assertTxMatches(t, info.Tx, tx)

	// 调用方给手中的结果填入内容。
	info.Tx.Content = []byte("filled-by-caller")
	filledID := info.Tx.ID()
	if filledID == id {
		t.Fatal("setup: filled content should change the computed id")
	}
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 1 {
		t.Fatalf("setup: pending = %d, want 1", len(acct.Pending))
	}
	acct.Pending[0].Tx.Content = []byte("filled-via-account")

	// 原交易仍是那笔空内容交易：标识、签名与各字段不变。
	got, err := n.Tx(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || len(got.Tx.Content) != 0 {
		t.Fatalf("original tx changed by caller fill: %+v", got)
	}
	assertTxMatches(t, got.Tx, tx)
	again := n.Account(keys[0].pub)
	if len(again.Pending) != 1 || again.Pending[0].ID != id || len(again.Pending[0].Tx.Content) != 0 {
		t.Fatalf("account pending changed by caller fill: %+v", again.Pending)
	}
	// 填入内容后算出的标识并没有成为节点中的另一笔交易。
	if _, err := n.Tx(filledID); reason(err) != ReasonUnknownTx {
		t.Fatalf("filled id got %v, want %s", err, ReasonUnknownTx)
	}
}
