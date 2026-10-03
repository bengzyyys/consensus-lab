package consensus

import (
	"bytes"
	"fmt"
	"testing"
)

// assertTxEqual 校验查询结果中的交易与提交时接收的交易逐字段一致，
// 标识仍能对应内容、原签名仍有效；同时要求返回的是独立的 *Transaction。
func assertTxEqual(t *testing.T, label string, got, want *Transaction) {
	t.Helper()
	if got == want {
		t.Fatalf("%s: query must return an independent *Transaction, got the same pointer", label)
	}
	if !bytes.Equal(got.Sender, want.Sender) {
		t.Fatalf("%s: sender = %x, want %x", label, got.Sender, want.Sender)
	}
	if got.Sequence != want.Sequence {
		t.Fatalf("%s: sequence = %d, want %d", label, got.Sequence, want.Sequence)
	}
	if !bytes.Equal(got.Content, want.Content) {
		t.Fatalf("%s: content = %q, want %q", label, got.Content, want.Content)
	}
	if got.Fee != want.Fee {
		t.Fatalf("%s: fee = %d, want %d", label, got.Fee, want.Fee)
	}
	if got.Expiry != want.Expiry {
		t.Fatalf("%s: expiry = %d, want %d", label, got.Expiry, want.Expiry)
	}
	if !bytes.Equal(got.Signature, want.Signature) {
		t.Fatalf("%s: signature changed", label)
	}
	if !got.Verify() {
		t.Fatalf("%s: original signature must still verify", label)
	}
	if got.ID() != want.ID() {
		t.Fatalf("%s: tx id = %s, want %s", label, got.ID(), want.ID())
	}
}

// tamperTxInfo 以调用方身份尽力改写手中的查询结果：改写发送者公钥、序号、
// 内容、费用、到期轮次与签名，并连查询元数据（标识、状态、备注、关联字段）一起改。
func tamperTxInfo(info *TxInfo, otherPub []byte) {
	info.ID = "tampered-id"
	info.Status = StatusConfirmed
	info.Note = "tampered-note"
	info.BlockHeight = 99
	info.BlockID = "tampered-block"
	info.ReplacedBy = "tampered-replacement"
	info.DropReason = "tampered-drop"
	info.DropRound = 42
	tx := info.Tx
	tx.Sender = append([]byte(nil), otherPub...)
	tx.Sequence = 999
	tx.Content = []byte("tampered-content")
	tx.Fee = 999999
	tx.Expiry = 1 // 调用方把到期轮次改成当前轮次
	sig := append([]byte(nil), tx.Signature...)
	for i, j := 0, len(sig)-1; i < j; i, j = i+1, j-1 {
		sig[i], sig[j] = sig[j], sig[i]
	}
	tx.Signature = sig
}

// 按标识取得的待处理交易是独立副本：改写发送者公钥、内容、签名、费用、序号、
// 到期轮次与查询元数据后，再次查询仍是提交时接收的完整交易，原标识对应原内容、
// 原签名仍有效、交易仍属于原账户和原序号。
func TestTxQueryResultIsIndependentSnapshot(t *testing.T) {
	keys := genKeys(t, 4)
	// 单块上限设为 1，使费用高低直接决定打包次序。
	n, _ := newTestNode(t, keys, 1)
	txA := NewTransaction(keys[0].priv, 1, []byte("payload-a"), 5, 100)
	txB := NewTransaction(keys[1].priv, 1, []byte("payload-b"), 6, 100)
	if _, err := n.Submit(txA); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(txB); err != nil {
		t.Fatal(err)
	}

	info := mustTx(t, n, txA.ID())
	if info.Status != StatusQueued {
		t.Fatalf("setup status = %s, want queued", info.Status)
	}
	tamperTxInfo(info, keys[2].pub)

	// 节点再次查询到的仍是提交时接收的完整交易。
	fresh := mustTx(t, n, txA.ID())
	if fresh.ID != txA.ID() || fresh.Status != StatusQueued {
		t.Fatalf("fresh lookup changed: %+v", fresh)
	}
	assertTxEqual(t, "fresh Tx", fresh.Tx, txA)

	// 交易仍属于原账户和原序号，不会因为改了手中结果的发送者而“过户”。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 {
		t.Fatalf("original account wrong: %+v", acct)
	}
	if acct.Pending[0].ID != txA.ID() || acct.Pending[0].Tx.Sequence != 1 {
		t.Fatalf("original account pending wrong: %+v", acct.Pending[0])
	}
	assertTxEqual(t, "account pending", acct.Pending[0].Tx, txA)
	if stolen := n.Account(keys[2].pub); len(stolen.Pending) != 0 || stolen.ConfirmedSequence != 0 {
		t.Fatalf("tampered sender account must stay empty: %+v", stolen)
	}

	// 费用仍是提交时的 5：单块只打包一笔时，费用 6 的 txB 入选，
	// 被调高成 999999 的那份结果不影响打包选择。
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint([]string{txB.ID()}) {
		t.Fatalf("round 1 proposal = %v, want [%s]", p1.TxIDs, txB.ID())
	}
	confirmByVotesResult(t, n, keys, p1.BlockID)

	// 到期轮次仍是提交时的 100：进入第 2 轮后交易仍有效、仍排队等待。
	if n.CurrentRound() != 2 {
		t.Fatalf("round = %d, want 2", n.CurrentRound())
	}
	after := mustTx(t, n, txA.ID())
	if after.Status != StatusQueued {
		t.Fatalf("tampered expiry leaked: status = %s, want queued", after.Status)
	}
	assertTxEqual(t, "round 2 Tx", after.Tx, txA)

	// 第 2 轮按原费用与原内容正常打包并确认原交易。
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p2.TxIDs) != fmt.Sprint([]string{txA.ID()}) {
		t.Fatalf("round 2 proposal = %v, want [%s]", p2.TxIDs, txA.ID())
	}
	confirmByVotesResult(t, n, keys, p2.BlockID)
	confirmed := mustTx(t, n, txA.ID())
	if confirmed.Status != StatusConfirmed || confirmed.BlockID != p2.BlockID || confirmed.BlockHeight != 2 {
		t.Fatalf("txA confirmation wrong: %+v", confirmed)
	}
	assertTxEqual(t, "confirmed Tx", confirmed.Tx, txA)
}

// 账户待处理项中携带的交易同样是独立副本，且账户级字段（已确认序号、
// 待处理次序、缺口说明）也不随调用方编辑而变化。
func TestAccountPendingResultIsIndependentSnapshot(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	// 同账户序号 1 和 3：序号 2 缺失，账户查询应指出缺口 2。
	tx1 := NewTransaction(keys[0].priv, 1, []byte("seq-1"), 3, 100)
	tx3 := NewTransaction(keys[0].priv, 3, []byte("seq-3"), 4, 100)
	// 相邻账户的交易，用来检查篡改不会波及其他账户。
	txOther := NewTransaction(keys[1].priv, 1, []byte("other"), 1, 100)
	for _, tx := range []*Transaction{tx1, tx3, txOther} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 2 || len(acct.Pending) != 2 {
		t.Fatalf("setup account wrong: %+v", acct)
	}
	if acct.Pending[0].Tx.Sequence != 1 || acct.Pending[1].Tx.Sequence != 3 {
		t.Fatalf("pending order = [%d %d], want [1 3]",
			acct.Pending[0].Tx.Sequence, acct.Pending[1].Tx.Sequence)
	}
	for _, p := range acct.Pending {
		if p.Note != "waiting-pack" {
			t.Fatalf("note = %q, want waiting-pack", p.Note)
		}
	}

	// 改写列表中的第一笔，并连账户级字段一起改：交换次序、改确认序号与缺口。
	tamperTxInfo(acct.Pending[0], keys[2].pub)
	acct.Pending[0], acct.Pending[1] = acct.Pending[1], acct.Pending[0]
	acct.Pending = append(acct.Pending, &TxInfo{ID: "phantom"})
	acct.ConfirmedSequence = 7
	acct.Gap = 8
	acct.Sender = fmt.Sprintf("%x", keys[2].pub)

	// 新查询：已确认序号、待处理次序、缺口说明与两笔交易内容全部保持原值。
	fresh := n.Account(keys[0].pub)
	if fresh.ConfirmedSequence != 0 || fresh.Gap != 2 {
		t.Fatalf("fresh account header changed: confirmed=%d gap=%d", fresh.ConfirmedSequence, fresh.Gap)
	}
	if len(fresh.Pending) != 2 {
		t.Fatalf("fresh pending len = %d, want 2", len(fresh.Pending))
	}
	if fresh.Pending[0].ID != tx1.ID() || fresh.Pending[1].ID != tx3.ID() {
		t.Fatalf("fresh pending order changed: [%s %s]", fresh.Pending[0].ID, fresh.Pending[1].ID)
	}
	assertTxEqual(t, "fresh pending tx1", fresh.Pending[0].Tx, tx1)
	// 改写第一笔不能影响相邻的第二笔：内容、所属账户、序号与签名保持原样。
	assertTxEqual(t, "fresh pending tx3", fresh.Pending[1].Tx, tx3)
	if fresh.Pending[0].Note != "waiting-pack" || fresh.Pending[1].Note != "waiting-pack" {
		t.Fatalf("fresh notes changed: %q %q", fresh.Pending[0].Note, fresh.Pending[1].Note)
	}

	// 被改写的发送者账户不会凭空出现待处理交易；相邻账户也不受影响。
	if stolen := n.Account(keys[2].pub); len(stolen.Pending) != 0 {
		t.Fatalf("tampered sender account must stay empty: %+v", stolen)
	}
	other := n.Account(keys[1].pub)
	if len(other.Pending) != 1 || other.Pending[0].ID != txOther.ID() {
		t.Fatalf("neighbor account changed: %+v", other)
	}
	assertTxEqual(t, "neighbor account tx", other.Pending[0].Tx, txOther)

	// 按标识的直接查询同样保持原样。
	assertTxEqual(t, "Tx tx1", mustTx(t, n, tx1.ID()).Tx, tx1)
	assertTxEqual(t, "Tx tx3", mustTx(t, n, tx3.ID()).Tx, tx3)
}

// 同一笔交易可以先后取得多份结果，也可以同时出现在 Tx 与 Account 查询中：
// 编辑其中一份，其他已取得结果保持各自取得时的内容；再取得的新结果也是原值。
func TestMultiplePendingSnapshotsIndependent(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("shared"), 2, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}

	i1 := mustTx(t, n, tx.ID())
	i2 := mustTx(t, n, tx.ID())
	acct1 := n.Account(keys[0].pub)
	acctTx1 := acct1.Pending[0]

	// 改写第一份 Tx 结果：其他已取得结果（含账户查询中的同笔交易）不变。
	tamperTxInfo(i1, keys[1].pub)
	assertTxEqual(t, "second Tx copy", i2.Tx, tx)
	assertTxEqual(t, "account copy", acctTx1.Tx, tx)

	// 再改写账户查询中的那份：其余副本与新查询仍保持原值。
	tamperTxInfo(acctTx1, keys[2].pub)
	assertTxEqual(t, "second Tx copy after account tamper", i2.Tx, tx)
	assertTxEqual(t, "new Tx copy", mustTx(t, n, tx.ID()).Tx, tx)
	freshAcct := n.Account(keys[0].pub)
	if len(freshAcct.Pending) != 1 {
		t.Fatalf("fresh account pending len = %d, want 1", len(freshAcct.Pending))
	}
	assertTxEqual(t, "new account copy", freshAcct.Pending[0].Tx, tx)
}

// 查询结果是取得时的快照：排队时取得结果，节点随后产生提议，
// 新查询显示已进入提议、账户备注变为等待投票，而旧结果保持取得时的状态与内容；
// 此时编辑旧结果不改变当前提议的交易标识与顺序。
func TestPendingSnapshotKeepsFetchTimeState(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("snapshot"), 2, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}

	// 提议前取得：排队 + 等待打包。
	oldTx := mustTx(t, n, tx.ID())
	oldAcct := n.Account(keys[0].pub)
	if oldTx.Status != StatusQueued || oldAcct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("setup state wrong: tx=%s note=%s", oldTx.Status, oldAcct.Pending[0].Note)
	}

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{tx.ID()}) {
		t.Fatalf("proposal = %v, want [%s]", p.TxIDs, tx.ID())
	}

	// 旧结果仍显示取得时的排队状态与内容。
	if oldTx.Status != StatusQueued {
		t.Fatalf("old Tx status changed to %s", oldTx.Status)
	}
	if oldAcct.Pending[0].Note != "waiting-pack" {
		t.Fatalf("old account note changed to %q", oldAcct.Pending[0].Note)
	}
	assertTxEqual(t, "old Tx copy", oldTx.Tx, tx)
	assertTxEqual(t, "old account copy", oldAcct.Pending[0].Tx, tx)

	// 新查询显示交易已进入提议，账户备注变为等待投票。
	newTx := mustTx(t, n, tx.ID())
	if newTx.Status != StatusProposed {
		t.Fatalf("new Tx status = %s, want proposed", newTx.Status)
	}
	newAcct := n.Account(keys[0].pub)
	if len(newAcct.Pending) != 1 || newAcct.Pending[0].Note != "waiting-vote" {
		t.Fatalf("new account state wrong: %+v", newAcct)
	}
	assertTxEqual(t, "new Tx copy", newTx.Tx, tx)

	// 此时编辑旧结果：当前提议的标识与顺序保持原样，候选历史同样不变。
	tamperTxInfo(oldTx, keys[1].pub)
	tamperTxInfo(oldAcct.Pending[0], keys[2].pub)
	again, ok := n.Proposal()
	if !ok {
		t.Fatal("proposal must still exist")
	}
	if again.BlockID != p.BlockID || fmt.Sprint(again.TxIDs) != fmt.Sprint([]string{tx.ID()}) {
		t.Fatalf("proposal changed after tamper: %+v", again)
	}
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	var local *CandidateView
	for i := range rc.Candidates {
		if rc.Candidates[i].Local {
			local = &rc.Candidates[i]
		}
	}
	if local == nil || local.BlockID != p.BlockID || fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{tx.ID()}) {
		t.Fatalf("local candidate changed after tamper: %+v", local)
	}

	// 原交易仍可按原标识正常投票确认。
	confirmByVotesResult(t, n, keys, p.BlockID)
	confirmed := mustTx(t, n, tx.ID())
	if confirmed.Status != StatusConfirmed || confirmed.BlockID != p.BlockID {
		t.Fatalf("tx confirmation wrong: %+v", confirmed)
	}
	assertTxEqual(t, "confirmed tx", confirmed.Tx, tx)
}

// 内容为空也是合法交易：查询保留其标识、签名与其他字段；调用方向手中结果
// 填入内容、调高费用，不会把原交易变成另一笔交易。
func TestEmptyContentTxQuerySnapshot(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, nil, 1, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}
	if len(tx.Content) != 0 {
		t.Fatal("setup: empty content must be accepted")
	}

	info := mustTx(t, n, tx.ID())
	if len(info.Tx.Content) != 0 {
		t.Fatalf("content len = %d, want 0", len(info.Tx.Content))
	}
	if len(info.Tx.Signature) == 0 || !info.Tx.Verify() {
		t.Fatal("empty-content tx must keep a valid signature")
	}
	if info.ID != tx.ID() || info.Tx.ID() != tx.ID() {
		t.Fatalf("empty-content tx id mismatch: %s vs %s", info.ID, tx.ID())
	}

	// 调用方给这份结果填入内容并调高费用。
	info.Tx.Content = []byte("filled-by-caller")
	info.Tx.Fee = 50
	info.ID = "filled-id"

	fresh := mustTx(t, n, tx.ID())
	if len(fresh.Tx.Content) != 0 {
		t.Fatalf("original content changed to %q", fresh.Tx.Content)
	}
	if fresh.Tx.Fee != 1 || fresh.ID != tx.ID() {
		t.Fatalf("original tx became a different tx: %+v", fresh)
	}
	assertTxEqual(t, "empty-content tx", fresh.Tx, tx)
	acct := n.Account(keys[0].pub)
	if len(acct.Pending) != 1 || len(acct.Pending[0].Tx.Content) != 0 {
		t.Fatalf("account pending changed: %+v", acct.Pending)
	}

	// 原交易（空内容）照常打包确认。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{tx.ID()}) {
		t.Fatalf("proposal = %v, want [%s]", p.TxIDs, tx.ID())
	}
	confirmByVotesResult(t, n, keys, p.BlockID)
}

// 调用方对查询结果的改写不会借节点的正常保存落盘：触发保存并重开目录后，
// 节点中的交易与账户状态仍是提交时的原值。
func TestPendingSnapshotMutationNeverPersisted(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	tx := NewTransaction(keys[0].priv, 1, []byte("persist-me"), 4, 100)
	if _, err := n.Submit(tx); err != nil {
		t.Fatal(err)
	}

	txInfo := mustTx(t, n, tx.ID())
	tamperTxInfo(txInfo, keys[1].pub)
	acct := n.Account(keys[0].pub)
	tamperTxInfo(acct.Pending[0], keys[2].pub)
	acct.ConfirmedSequence = 9
	acct.Gap = 10

	// 产生提议触发一次正常保存。
	if _, err := n.Propose(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := mustTx(t, reopened, tx.ID())
	if got.Status != StatusProposed {
		t.Fatalf("reopened status = %s, want proposed", got.Status)
	}
	assertTxEqual(t, "reopened Tx", got.Tx, tx)
	rAcct := reopened.Account(keys[0].pub)
	if rAcct.ConfirmedSequence != 0 || rAcct.Gap != 0 || len(rAcct.Pending) != 1 {
		t.Fatalf("reopened account wrong: %+v", rAcct)
	}
	assertTxEqual(t, "reopened account tx", rAcct.Pending[0].Tx, tx)
	if _, err := reopened.Tx("tampered-id"); reason(err) != ReasonUnknownTx {
		t.Fatalf("tampered id persisted: %v", err)
	}
}

func mustTx(t *testing.T, n *Node, id string) *TxInfo {
	t.Helper()
	info, err := n.Tx(id)
	if err != nil {
		t.Fatalf("Tx(%s): %v", id, err)
	}
	return info
}
