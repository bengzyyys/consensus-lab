package consensus

import "testing"

// 回归保障：交易提交会永久保留“曾被接受”的历史，即使交易后来已退出交易池
// （被替换 replaced 或进入到期轮次失效 expired）。重复判定以交易标识为唯一准据：
// 退出池不等于从未被接受；同一发送者加同一序号也不等于同一笔交易。
// 本文件沿用既有 Go 库入口（Submit/Tx/Account/EndRound）、既有拒绝原因
// （duplicate-transaction、fee-not-higher、tx-expired）与既有接收规则，
// 不引入任何新行为，只锁定这些既有结果。

// 排队交易被同发送者同序号、费用严格更高的新交易替换后，按原始内容和签名
// 重新提交旧交易：必须返回 duplicate-transaction 而不是 fee-not-higher，
// 旧交易不能重新入池，两条记录与账户待处理列表均保持替换后的样子。
func TestResubmitReplacedHistoryReturnsDuplicate(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 旧交易先被接受，再被费用严格更高的同发送者同序号交易替换。
	old := NewTransaction(keys[0].priv, 1, []byte("old-tx"), 1, 100)
	if _, err := n.Submit(old); err != nil {
		t.Fatal(err)
	}
	oldID := old.ID()
	newTx := NewTransaction(keys[0].priv, 1, []byte("new-tx"), 5, 100)
	rep, err := n.Submit(newTx)
	if err != nil {
		t.Fatal(err)
	}
	newID := newTx.ID()
	if rep.ReplacedID != oldID || rep.TxID != newID {
		t.Fatalf("setup replacement result wrong: %+v", rep)
	}

	// 记录重投前的全局状态，拒绝操作不得改动它们。
	entriesBefore := len(n.st.Entries)
	roundBefore := n.CurrentRound()
	heightBefore := n.Height()

	// 按最初被接受时的发送者、序号、内容、费用、到期轮次与签名原样重建。
	// Ed25519 签名确定，逐字节相同的输入必须得到同一交易标识。
	replay := NewTransaction(keys[0].priv, 1, []byte("old-tx"), 1, 100)
	if replay.ID() != oldID {
		t.Fatalf("test setup: byte-identical replay id %s != original %s", replay.ID(), oldID)
	}
	res, err := n.Submit(replay)
	if err == nil {
		t.Fatalf("resubmit of replaced tx must be rejected, got result %+v", res)
	}
	if got := reason(err); got != ReasonDuplicate {
		t.Fatalf("resubmit replaced tx reason = %q, want %s (must not be %s)",
			got, ReasonDuplicate, ReasonLowFee)
	}
	// 拒绝结果不得携带任何成功接收或再次替换的信息。
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 拒绝本身不新增记录、不推进轮次、不生成确认块。
	if len(n.st.Entries) != entriesBefore {
		t.Fatalf("entries changed %d -> %d after rejected resubmit", entriesBefore, len(n.st.Entries))
	}
	if n.CurrentRound() != roundBefore || n.Height() != heightBefore {
		t.Fatalf("round/height changed: %d/%d, want %d/%d",
			n.CurrentRound(), n.Height(), roundBefore, heightBefore)
	}
	if blk, ok := n.LatestBlock(); ok {
		t.Fatalf("rejected resubmit must not confirm a block, got %+v", blk)
	}

	// 旧记录仍显示 replaced，替换关联仍指向原来接受的新交易，完整内容保留。
	oldInfo, err := n.Tx(oldID)
	if err != nil {
		t.Fatal(err)
	}
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newID {
		t.Fatalf("old record = status %s replacedBy %s, want replaced/%s",
			oldInfo.Status, oldInfo.ReplacedBy, newID)
	}
	assertTxMatches(t, oldInfo.Tx, old)

	// 旧交易不能重新进入池：该账户序号 1 的当前交易仍只能是新交易。
	if cur := n.st.Pool[old.SenderHex()][1]; cur != newID {
		t.Fatalf("pool slot sender/seq1 = %s, want %s; old tx must not re-enter pool", cur, newID)
	}

	// 新交易的内容与排队状态保持不变，也没有产生新的替换关联。
	newInfo, err := n.Tx(newID)
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.Status != StatusQueued || newInfo.ReplacedBy != "" {
		t.Fatalf("new record changed: status=%s replacedBy=%q, want queued/empty",
			newInfo.Status, newInfo.ReplacedBy)
	}
	assertTxMatches(t, newInfo.Tx, newTx)

	// 账户待处理列表仍是替换后的唯一一笔：新交易，等待打包。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 1 {
		t.Fatalf("account pending changed: %+v", acct)
	}
	p := acct.Pending[0]
	if p.ID != newID || p.Status != StatusQueued || p.Note != "waiting-pack" {
		t.Fatalf("account pending entry wrong: %+v", p)
	}

	// 对照：同发送者同序号但标识不同的另一笔交易（费用仍低于当前交易）
	// 必须走 fee-not-higher 而非 duplicate，且不留任何历史——
	// 重复判据是交易标识，不是“发送者+序号”。
	other := NewTransaction(keys[0].priv, 1, []byte("other-tx"), 3, 100)
	if other.ID() == oldID || other.ID() == newID {
		t.Fatal("test setup: distinct content must yield a distinct id")
	}
	if _, err := n.Submit(other); reason(err) != ReasonLowFee {
		t.Fatalf("distinct-id same-sender/seq tx reason = %v, want %s", err, ReasonLowFee)
	}
	if _, err := n.Tx(other.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("fee-rejected distinct tx reason = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}
	if len(n.st.Entries) != entriesBefore {
		t.Fatalf("entries changed after control rejection: %d -> %d", entriesBefore, len(n.st.Entries))
	}
}

// 因进入到期轮次而失效的交易，再次按原样提交时，即使其到期轮次已不大于
// 当前轮次，仍必须返回 duplicate-transaction 而不是 tx-expired。历史记录、
// 账户待处理列表与已确认序号均不因此变化，拒绝操作不留任何副作用。
func TestResubmitExpiredHistoryReturnsDuplicate(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 第 1 轮接受到期轮次为 2 的交易。
	old := NewTransaction(keys[0].priv, 1, []byte("expiring-tx"), 1, 2)
	if _, err := n.Submit(old); err != nil {
		t.Fatal(err)
	}
	oldID := old.ID()

	// 结束第 1 轮进入第 2 轮：expiry 2 <= round 2，交易失效并退出池。
	round, err := n.EndRound()
	if err != nil || round != 2 {
		t.Fatalf("EndRound = %d, %v; want round 2", round, err)
	}
	info, err := n.Tx(oldID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired {
		t.Fatalf("setup: status = %s, want expired", info.Status)
	}

	entriesBefore := len(n.st.Entries)

	// 原样重建并重投：六字段与签名均与最初被接受时一致，标识因此相同。
	replay := NewTransaction(keys[0].priv, 1, []byte("expiring-tx"), 1, 2)
	if replay.ID() != oldID {
		t.Fatalf("test setup: byte-identical replay id %s != original %s", replay.ID(), oldID)
	}
	res, err := n.Submit(replay)
	if err == nil {
		t.Fatalf("resubmit of expired history must be rejected, got result %+v", res)
	}
	if got := reason(err); got != ReasonDuplicate {
		t.Fatalf("resubmit expired tx reason = %q, want %s (must not be %s even though expiry %d <= round %d)",
			got, ReasonDuplicate, ReasonExpired, old.Expiry, n.CurrentRound())
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 拒绝操作本身：不新增交易记录、不改变当前轮次、不生成确认块。
	if len(n.st.Entries) != entriesBefore {
		t.Fatalf("entries changed %d -> %d after rejected resubmit", entriesBefore, len(n.st.Entries))
	}
	if n.CurrentRound() != 2 || n.Height() != 0 {
		t.Fatalf("round/height changed: %d/%d, want 2/0", n.CurrentRound(), n.Height())
	}
	if blk, ok := n.LatestBlock(); ok {
		t.Fatalf("rejected resubmit must not confirm a block, got %+v", blk)
	}

	// 原记录继续显示 expired，完整交易内容与签名保留，不带区块或替换关联。
	info, err = n.Tx(oldID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired || info.BlockID != "" || info.BlockHeight != 0 || info.ReplacedBy != "" {
		t.Fatalf("expired record wrong: %+v", info)
	}
	assertTxMatches(t, info.Tx, old)

	// 账户待处理列表中不能重新出现它，已确认序号也不因此推进。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || len(acct.Pending) != 0 {
		t.Fatalf("expired tx must not reappear pending nor advance sequence: %+v", acct)
	}
}

// 对照：一笔从未被接受、提交时就已经到期的交易返回 tx-expired，
// 并且查询不到任何历史记录。它与上面的历史旧交易可以同发送者同序号，
// 区别仅在交易标识（内容与签名不同）。
func TestNeverAcceptedExpiredTxRejectedWithoutHistory(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 先制造一条 expired 历史，保证“历史交易”与“从未接受交易”同场可比。
	hist := NewTransaction(keys[0].priv, 1, []byte("accepted-then-expired"), 1, 2)
	if _, err := n.Submit(hist); err != nil {
		t.Fatal(err)
	}
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	if info, _ := n.Tx(hist.ID()); info.Status != StatusExpired {
		t.Fatalf("setup: history status = %s, want expired", info.Status)
	}
	entriesBefore := len(n.st.Entries)

	// 同发送者同序号、但内容与签名全新的另一笔交易：当前为第 2 轮，提交即到期。
	fresh := NewTransaction(keys[0].priv, 1, []byte("never-accepted"), 1, 2)
	if fresh.ID() == hist.ID() {
		t.Fatal("test setup: fresh tx must have a distinct id from history tx")
	}
	res, err := n.Submit(fresh)
	if err == nil {
		t.Fatalf("already-expired new tx must be rejected, got result %+v", res)
	}
	if got := reason(err); got != ReasonExpired {
		t.Fatalf("never-accepted expired tx reason = %q, want %s", got, ReasonExpired)
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 从未被接受：按标识查询不到历史，条目数不增加。
	if _, err := n.Tx(fresh.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("fresh expired tx lookup reason = %v, want %s", err, ReasonUnknownTx)
	}
	if len(n.st.Entries) != entriesBefore {
		t.Fatalf("entries changed %d -> %d; expired rejection keeps no history",
			entriesBefore, len(n.st.Entries))
	}

	// 既有 expired 历史不受这次拒绝影响。
	histInfo, err := n.Tx(hist.ID())
	if err != nil {
		t.Fatal(err)
	}
	if histInfo.Status != StatusExpired {
		t.Fatalf("history record changed: status = %s, want expired", histInfo.Status)
	}

	// 同场再验一次分野：历史交易原样重投是 duplicate，而非 tx-expired。
	replay := NewTransaction(keys[0].priv, 1, []byte("accepted-then-expired"), 1, 2)
	if replay.ID() != hist.ID() {
		t.Fatalf("test setup: replay id %s != history id %s", replay.ID(), hist.ID())
	}
	if _, err := n.Submit(replay); reason(err) != ReasonDuplicate {
		t.Fatalf("history replay reason = %v, want %s", err, ReasonDuplicate)
	}
}

// 过期只结束旧交易的有效性，并不占用该账户尚未确认的序号。同发送者同序号
// 重新签署一笔到期轮次晚于当前轮次的新交易，只要符合既有接收条件就正常入队；
// 它不是替换（即使新费用更低也不能按 fee-not-higher 拒绝），旧记录保持 expired。
func TestExpiryFreesSequenceForFreshlySignedTx(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	old := NewTransaction(keys[0].priv, 1, []byte("old-expired"), 2, 2)
	if _, err := n.Submit(old); err != nil {
		t.Fatal(err)
	}
	oldID := old.ID()
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	if info, _ := n.Tx(oldID); info.Status != StatusExpired {
		t.Fatalf("setup: status = %s, want expired", info.Status)
	}
	entriesBefore := len(n.st.Entries)

	// 同发送者同序号重新签署：新内容、费用甚至低于旧交易、到期轮次晚于当前轮次。
	renewed := NewTransaction(keys[0].priv, 1, []byte("renewed"), 1, 10)
	if renewed.ID() == oldID {
		t.Fatal("test setup: renewed tx must have a distinct id")
	}
	if n.CurrentRound() >= renewed.Expiry {
		t.Fatalf("test setup: renewed expiry %d must be greater than current round %d",
			renewed.Expiry, n.CurrentRound())
	}
	res, err := n.Submit(renewed)
	if err != nil {
		t.Fatalf("freshly signed tx for expired sequence must be accepted: %v", err)
	}
	newID := renewed.ID()
	// 旧交易早已退出池：这是一次全新接收，不是替换，也没有挤出任何交易。
	if res.TxID != newID || res.ReplacedID != "" || res.EvictedID != "" {
		t.Fatalf("renewed submit result wrong: %+v", res)
	}

	// 仅新增新交易一条记录；旧记录仍为 expired，替换关联保持为空。
	if len(n.st.Entries) != entriesBefore+1 {
		t.Fatalf("entries changed %d -> %d, want %d", entriesBefore, len(n.st.Entries), entriesBefore+1)
	}
	oldInfo, err := n.Tx(oldID)
	if err != nil {
		t.Fatal(err)
	}
	if oldInfo.Status != StatusExpired || oldInfo.ReplacedBy != "" {
		t.Fatalf("old record changed: status=%s replacedBy=%q, want expired/empty",
			oldInfo.Status, oldInfo.ReplacedBy)
	}
	// 新交易按自己的标识可查，内容与签名完整，处于排队状态。
	newInfo, err := n.Tx(newID)
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.Status != StatusQueued {
		t.Fatalf("renewed tx status = %s, want queued", newInfo.Status)
	}
	assertTxMatches(t, newInfo.Tx, renewed)

	// 账户待处理列表中，序号 1 对应的是有效的新交易；旧 expired 交易不出现。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 0 || len(acct.Pending) != 1 {
		t.Fatalf("account wrong after renewal: %+v", acct)
	}
	p := acct.Pending[0]
	if p.ID != newID || p.Status != StatusQueued || p.Note != "waiting-pack" {
		t.Fatalf("pending entry wrong: %+v", p)
	}

	// 正常进入排队即可被下一轮打包，证明它走的是既有接收规则。
	prop, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if prop.Round != 2 || len(prop.TxIDs) != 1 || prop.TxIDs[0] != newID {
		t.Fatalf("renewed tx should pack in round 2, got %+v", prop)
	}
}
