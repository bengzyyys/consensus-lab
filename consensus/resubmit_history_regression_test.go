package consensus

import (
	"crypto/ed25519"
	"testing"
)

// 本文件钉住“退出交易池后的历史交易被原样重复提交”的回归保障。
//
// 交易提交功能保留所有曾被接收交易的历史（replaced / expired / dropped /
// confirmed），重复识别一律以交易标识为准：退出池不代表这笔交易从未被接受过，
// 同一发送者加同一序号也不等于同一笔交易。因此：
//
//   - 被加费替换的旧交易按原始内容和签名重新提交时，即使它当前不在池中、费用
//     低于替换它的新交易，也必须返回 duplicate-transaction 而非 fee-not-higher；
//     旧交易不能重新入池，旧记录仍显示 replaced 且替换关联指向原来接受的新交易，
//     新交易的内容、排队状态与账户待处理列表保持不变，拒绝结果不带任何成功
//     接收或再次替换的信息。
//   - 因进入到期轮次而失效的交易再次原样提交时，即使其到期轮次已不大于当前
//     轮次，也必须返回 duplicate-transaction 而非 tx-expired；旧记录继续显示
//     expired、完整内容保留，账户待处理列表中不重新出现它，已确认序号也不
//     推进。这与一笔从未被接受、提交时就已到期的交易不同：后者返回
//     tx-expired，且查询不到任何历史记录。
//   - 过期只结束旧交易的有效性，并不占用该账户尚未确认的序号。同一发送者、
//     同一序号重新签署的一笔到期轮次晚于当前轮次的新交易，只要符合原有接收
//     条件就应正常进入排队；旧记录保持过期，新交易按自己的标识可查，账户
//     列表中该序号对应的是有效的新交易。
//
// “原样提交”要求发送者、序号、内容、费用、到期轮次和签名均与最初被接受时
// 一致，签名仍然有效。拒绝历史交易的操作本身不新增交易记录、不改变当前轮次、
// 不生成确认块。本组测试只沿用现有库入口、错误原因和接收规则，不新增入口。

// rebuildIdenticalTx 以最初接收时的全部字段（发送者、序号、内容、费用、到期轮次）
// 重新构造并签名一笔交易，并断言新对象的交易标识与原交易一致、签名仍有效，
// 即严格意义上的“按原始内容和签名重新提交”，而不是复用调用方手中的指针。
func rebuildIdenticalTx(t *testing.T, orig *Transaction, priv ed25519.PrivateKey) *Transaction {
	t.Helper()
	content := append([]byte(nil), orig.Content...)
	again := NewTransaction(priv, orig.Sequence, content, orig.Fee, orig.Expiry)
	if again.ID() != orig.ID() {
		t.Fatalf("rebuilt tx id = %s, want original id %s", again.ID(), orig.ID())
	}
	if !again.Verify() {
		t.Fatal("rebuilt transaction signature must still verify")
	}
	return again
}

// assertNoHistorySideEffects 断言一次被拒绝的提交没有留下任何副作用：
// 条目总数不变、当前轮次与高度不变、没有新增确认块。
func assertNoHistorySideEffects(t *testing.T, n *Node, entriesBefore int, wantRound uint64) {
	t.Helper()
	if got := len(n.st.Entries); got != entriesBefore {
		t.Fatalf("rejected submit must not add records: entries %d -> %d", entriesBefore, got)
	}
	if n.CurrentRound() != wantRound {
		t.Fatalf("current round = %d, want unchanged %d", n.CurrentRound(), wantRound)
	}
	if n.Height() != 0 {
		t.Fatalf("rejected submit must not produce blocks, height = %d", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("rejected submit must not produce a latest block")
	}
}

// 排队中的旧交易被同一发送者、同一序号、费用严格更高的新交易替换后，按原始
// 内容和签名重新提交旧交易：必须返回 duplicate-transaction（而非
// fee-not-higher），旧交易不能重新入池，历史关联与新交易状态全部保持原样。
func TestResubmitReplacedTransactionIsDuplicate(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 旧交易先被正常接收进入排队。
	old := NewTransaction(keys[0].priv, 1, []byte("old-body"), 5, 100)
	res, err := n.Submit(old)
	if err != nil {
		t.Fatal(err)
	}
	if res.TxID != old.ID() || res.ReplacedID != "" || res.EvictedID != "" {
		t.Fatalf("initial accept result wrong: %+v", res)
	}

	// 同发送者同序号、内容不同、费用严格更高的新交易完成替换。
	newTx := NewTransaction(keys[0].priv, 1, []byte("new-body"), 8, 100)
	rep, err := n.Submit(newTx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TxID != newTx.ID() || rep.ReplacedID != old.ID() || rep.EvictedID != "" {
		t.Fatalf("replacement result wrong: %+v", rep)
	}
	oldInfo := n.mustTx(t, old.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newTx.ID() {
		t.Fatalf("old tx = status %s replacedBy %s, want replaced/%s", oldInfo.Status, oldInfo.ReplacedBy, newTx.ID())
	}
	newInfo := n.mustTx(t, newTx.ID())
	if newInfo.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", newInfo.Status)
	}

	// 按原始内容和签名重新提交旧交易（独立重建的对象，标识一致、签名有效）。
	oldAgain := rebuildIdenticalTx(t, old, keys[0].priv)
	entriesBefore := len(n.st.Entries)
	out, err := n.Submit(oldAgain)
	if err == nil {
		t.Fatal("resubmitting a replaced tx must be rejected")
	}
	if r := reason(err); r != ReasonDuplicate {
		t.Fatalf("resubmit reason = %q, want %s (must not be %s)", r, ReasonDuplicate, ReasonLowFee)
	}
	// 拒绝结果不能携带任何成功接收或再次替换的信息。
	if out != nil {
		t.Fatalf("rejected resubmit must return no result, got %+v", out)
	}

	// 旧记录仍显示 replaced，替换关联仍指向原来接受的新交易，完整内容保留。
	oldInfo = n.mustTx(t, old.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newTx.ID() {
		t.Fatalf("old record changed: status %s replacedBy %s", oldInfo.Status, oldInfo.ReplacedBy)
	}
	assertTxMatches(t, oldInfo.Tx, old)
	if oldInfo.BlockHeight != 0 || oldInfo.BlockID != "" {
		t.Fatalf("replaced tx must not reference a block: %+v", oldInfo)
	}

	// 新交易的内容与排队状态保持不变，没有发生第二次替换。
	newInfo = n.mustTx(t, newTx.ID())
	if newInfo.Status != StatusQueued || newInfo.ReplacedBy != "" {
		t.Fatalf("new tx changed: %+v", newInfo)
	}
	assertTxMatches(t, newInfo.Tx, newTx)

	// 账户待处理列表中该序号仍只对应新交易；已确认序号不推进。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 {
		t.Fatalf("account sequence state wrong: %+v", acct)
	}
	if len(acct.Pending) != 1 {
		t.Fatalf("pending count = %d, want 1: %+v", len(acct.Pending), acct.Pending)
	}
	pending := acct.Pending[0]
	if pending.ID != newTx.ID() || pending.Status != StatusQueued || pending.Note != "waiting-pack" {
		t.Fatalf("pending entry wrong: %+v", pending)
	}
	if pending.Tx == nil || pending.Tx.Sequence != 1 {
		t.Fatalf("pending tx should still be the new seq-1 tx: %+v", pending.Tx)
	}

	// 旧交易没有重新进入池：该账户序号 1 的当前标识仍是新交易。
	if cur := n.st.Pool[old.SenderHex()]; len(cur) != 1 || cur[1] != newTx.ID() {
		t.Fatalf("pool mapping wrong: %+v", cur)
	}
	assertNoHistorySideEffects(t, n, entriesBefore, 1)

	// 历史随状态持久化：重启后同一标识的旧交易仍被识别为重复，而非按费用规则
	// 重新判定；旧记录依旧是 replaced 并指向新交易。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Submit(rebuildIdenticalTx(t, old, keys[0].priv)); reason(err) != ReasonDuplicate {
		t.Fatalf("resubmit after reopen got %v, want %s", err, ReasonDuplicate)
	}
	reOld, err := reopened.Tx(old.ID())
	if err != nil {
		t.Fatal(err)
	}
	if reOld.Status != StatusReplaced || reOld.ReplacedBy != newTx.ID() {
		t.Fatalf("old record after reopen wrong: %+v", reOld)
	}
}

// 因进入到期轮次而失效的交易再次原样提交：即使到期轮次已不大于当前轮次，
// 也必须返回 duplicate-transaction（而非 tx-expired）；旧记录保持 expired、
// 完整内容保留，不重新出现在账户待处理列表，已确认序号不推进，拒绝不留任何
// 副作用。对照组：同发送者同序号但从未被接受、提交时已到期的新交易返回
// tx-expired，且查询不到历史记录。
func TestResubmitExpiredTransactionIsDuplicate(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 到期轮次为 2：轮次 1 接收，进入轮次 2 时失效。
	old := NewTransaction(keys[0].priv, 1, []byte("expiring-body"), 5, 2)
	if _, err := n.Submit(old); err != nil {
		t.Fatal(err)
	}
	if r, err := n.EndRound(); err != nil || r != 2 {
		t.Fatalf("end round 1: round=%d err=%v", r, err)
	}
	oldInfo := n.mustTx(t, old.ID())
	if oldInfo.Status != StatusExpired {
		t.Fatalf("setup: status = %s, want expired", oldInfo.Status)
	}
	// 再推进一轮：原样提交时到期轮次 2 已严格小于当前轮次 3。
	if r, err := n.EndRound(); err != nil || r != 3 {
		t.Fatalf("end round 2: round=%d err=%v", r, err)
	}
	if acct := n.Account(keys[0].pub); acct.ConfirmedSequence != 0 || len(acct.Pending) != 0 {
		t.Fatalf("expired tx must leave no pending entry: %+v", acct)
	}

	// 原样重新提交：独立重建对象，发送者/序号/内容/费用/到期轮次/签名全部一致。
	oldAgain := rebuildIdenticalTx(t, old, keys[0].priv)
	entriesBefore := len(n.st.Entries)
	out, err := n.Submit(oldAgain)
	if err == nil {
		t.Fatal("resubmitting an expired historical tx must be rejected")
	}
	if r := reason(err); r != ReasonDuplicate {
		t.Fatalf("resubmit reason = %q, want %s (must not be %s even though expiry %d <= current round)",
			r, ReasonDuplicate, ReasonExpired, old.Expiry)
	}
	if out != nil {
		t.Fatalf("rejected resubmit must return no result, got %+v", out)
	}

	// 原记录继续显示 expired，完整交易内容保留，不带确认块或替换关联。
	oldInfo = n.mustTx(t, old.ID())
	if oldInfo.Status != StatusExpired || oldInfo.ReplacedBy != "" {
		t.Fatalf("old record changed: %+v", oldInfo)
	}
	assertTxMatches(t, oldInfo.Tx, old)
	if oldInfo.BlockHeight != 0 || oldInfo.BlockID != "" {
		t.Fatalf("expired tx must not reference a block: %+v", oldInfo)
	}

	// 账户待处理列表中不能重新出现它，已确认序号也不因此推进。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 || len(acct.Pending) != 0 {
		t.Fatalf("account state after resubmit wrong: %+v", acct)
	}
	if _, ok := n.st.Pool[old.SenderHex()]; ok {
		t.Fatal("expired tx must not be back in the pool mapping")
	}
	assertNoHistorySideEffects(t, n, entriesBefore, 3)

	// 对照组：同一发送者、同一序号，但标识不同、从未被接受过的交易，提交时
	// （到期轮次 1 不大于当前轮次 3）必须按新交易规则返回 tx-expired。
	never := NewTransaction(keys[0].priv, 1, []byte("never-accepted"), 5, 1)
	if never.ID() == old.ID() {
		t.Fatal("test setup: never-accepted tx must have a different id")
	}
	if _, err := n.Submit(never); reason(err) != ReasonExpired {
		t.Fatalf("never-accepted expired tx got %v, want %s", err, ReasonExpired)
	}
	// 它不留任何历史记录：按标识查询得到 unknown-transaction。
	if _, err := n.Tx(never.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("rejected fresh tx must have no history, got %v", err)
	}
	if got := len(n.st.Entries); got != entriesBefore {
		t.Fatalf("rejected fresh tx must leave no record: entries %d -> %d", entriesBefore, got)
	}
}

// 过期只结束旧交易的有效性，并不占用该账户尚未确认的序号：同一发送者、同一
// 序号重新签署、到期轮次晚于当前轮次的新交易正常进入排队；旧记录保持过期，
// 新交易按自己的标识可查并可正常打包确认，账户列表中该序号对应有效新交易。
func TestExpiredSequenceAcceptsFreshResignedTransaction(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 旧交易在进入轮次 2 时过期，随后再进入轮次 3。
	old := NewTransaction(keys[0].priv, 1, []byte("expiring-body"), 5, 2)
	if _, err := n.Submit(old); err != nil {
		t.Fatal(err)
	}
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	if n.CurrentRound() != 3 {
		t.Fatalf("setup round = %d, want 3", n.CurrentRound())
	}
	if info := n.mustTx(t, old.ID()); info.Status != StatusExpired {
		t.Fatalf("setup: old status = %s, want expired", info.Status)
	}

	// 原样重提仍按标识判为重复，且不改变任何状态。
	entriesBefore := len(n.st.Entries)
	if _, err := n.Submit(rebuildIdenticalTx(t, old, keys[0].priv)); reason(err) != ReasonDuplicate {
		t.Fatalf("verbatim resubmit got %v, want %s", err, ReasonDuplicate)
	}
	assertNoHistorySideEffects(t, n, entriesBefore, 3)

	// 重新签署一笔同发送者同序号、内容不同、到期轮次晚于当前轮次的新交易。
	fresh := NewTransaction(keys[0].priv, 1, []byte("renewed-body"), 5, 10)
	if fresh.ID() == old.ID() {
		t.Fatal("test setup: fresh tx must have its own id")
	}
	res, err := n.Submit(fresh)
	if err != nil {
		t.Fatalf("fresh tx meeting accept rules must be admitted: %v", err)
	}
	// 这是一次全新接收，不是替换：旧交易早已退出池，不产生替换或挤出关联。
	if res.TxID != fresh.ID() || res.ReplacedID != "" || res.EvictedID != "" {
		t.Fatalf("fresh accept result wrong: %+v", res)
	}

	// 新交易按自己的标识可查、排队等待打包，内容与提交一致。
	freshInfo := n.mustTx(t, fresh.ID())
	if freshInfo.Status != StatusQueued || freshInfo.ReplacedBy != "" {
		t.Fatalf("fresh tx wrong: %+v", freshInfo)
	}
	assertTxMatches(t, freshInfo.Tx, fresh)

	// 账户待处理列表中序号 1 对应的是有效新交易；旧过期交易不在列表中。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 || len(acct.Pending) != 1 {
		t.Fatalf("account state wrong: %+v", acct)
	}
	pending := acct.Pending[0]
	if pending.ID != fresh.ID() || pending.Status != StatusQueued || pending.Note != "waiting-pack" {
		t.Fatalf("pending entry wrong: %+v", pending)
	}
	if pending.Tx == nil || pending.Tx.Sequence != 1 || pending.Tx.Expiry != 10 {
		t.Fatalf("pending tx should be the fresh seq-1 tx: %+v", pending.Tx)
	}
	if cur := n.st.Pool[fresh.SenderHex()]; len(cur) != 1 || cur[1] != fresh.ID() {
		t.Fatalf("pool mapping wrong: %+v", cur)
	}

	// 旧记录仍保持过期、完整内容保留，不被新接收改写。
	oldInfo := n.mustTx(t, old.ID())
	if oldInfo.Status != StatusExpired || oldInfo.ReplacedBy != "" {
		t.Fatalf("old record should stay expired: %+v", oldInfo)
	}
	assertTxMatches(t, oldInfo.Tx, old)

	// 新交易可正常打包并确认；确认只推进新交易，旧记录依旧 expired。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TxIDs) != 1 || p.TxIDs[0] != fresh.ID() {
		t.Fatalf("proposal should pack the fresh tx: %v", p.TxIDs)
	}
	confirmByVotes(t, n, keys, p.BlockID)
	if n.CurrentRound() != 4 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 4/1 after confirmation", n.CurrentRound(), n.Height())
	}
	freshInfo = n.mustTx(t, fresh.ID())
	if freshInfo.Status != StatusConfirmed || freshInfo.BlockHeight != 1 || freshInfo.BlockID != p.BlockID {
		t.Fatalf("fresh tx should be confirmed: %+v", freshInfo)
	}
	oldInfo = n.mustTx(t, old.ID())
	if oldInfo.Status != StatusExpired {
		t.Fatalf("old record changed after new tx confirmed: %s", oldInfo.Status)
	}
	acct = n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 1 || len(acct.Pending) != 0 {
		t.Fatalf("account after confirmation wrong: %+v", acct)
	}
}
