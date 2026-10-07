package consensus

import "testing"

// 本文件钉住交易池“连续加费替换”的公开回归结果，保证沿着历史交易的替换关系
// 逐笔查询时，可以看清楚每一步发生了什么。
//
// 同一发送者、同一尚未确认序号的交易仍在排队时，先后接收两笔签名有效、尚未到期、
// 费用依次严格提高的新交易：三笔交易的标识互不相同、内容可以区分，占用的是同一
// 个待处理位置。重点是第二次替换之后公开可见的历史——
//
//   - 每次成功提交都返回“本次接受的交易标识”和“刚被替换那笔的标识”，
//     且不挤出任何交易；
//   - 查询原交易显示 replaced，替换关联只指向第一次接受的新交易；
//     查询中间那笔同样显示 replaced，关联指向最后接受的交易；
//     更早的关联不能被改指到最后一笔（关联是逐跳形成的链，不是统一改指最新）；
//   - 历史交易的发送者、序号、内容、费用、到期轮次与签名保持原样，
//     replaced 记录不携带任何确认块信息；
//   - 账户在该序号上只列出最新交易（queued / waiting-pack），
//     已确认序号与序号缺口不因替换改变。
//
// 另覆盖：池已被原交易与另一发送者的排队交易占满时，连续替换照常成功、不挤出
// 另一笔交易、不推进轮次与确认高度，已替换历史也不重新占用池容量；
// 在尚未产生提议、最新交易仍排队时，标识不同但费用不更高的尝试返回
// fee-not-higher 且不留历史，原样重投已被替换的交易返回 duplicate-transaction，
// 拒绝后既有替换链、最新排队交易与其他账户交易均不变，随后合法的更高费用交易
// 仍可正常替换。本组测试沿用既有 Go 库入口（Submit/Tx/Account）与既有拒绝原因，
// 不引入或改变任何提交、打包与查询行为。

// assertReplacedHistory 断言一笔历史交易在替换链中处于 replaced 形态：
// 关联恰好指向 wantReplacedBy（不能被后续替换改指到更后的交易），发送者、序号、
// 内容、费用、到期轮次与签名与提交时逐字段一致，且不携带确认块或淘汰信息。
func assertReplacedHistory(t *testing.T, n *Node, old *Transaction, wantReplacedBy string) {
	t.Helper()
	info := mustTx(t, n, old.ID())
	if info.Status != StatusReplaced {
		t.Fatalf("tx %q status = %s, want replaced", old.Content, info.Status)
	}
	if info.ReplacedBy != wantReplacedBy {
		t.Fatalf("tx %q replacedBy = %q, want %q (earlier links must not be repointed to the latest)",
			old.Content, info.ReplacedBy, wantReplacedBy)
	}
	if info.BlockHeight != 0 || info.BlockID != "" {
		t.Fatalf("replaced tx %q must not carry block info: height=%d block=%q",
			old.Content, info.BlockHeight, info.BlockID)
	}
	if info.DropReason != "" || info.DropRound != 0 {
		t.Fatalf("replaced tx %q must not carry drop info: reason=%q round=%d",
			old.Content, info.DropReason, info.DropRound)
	}
	assertTxMatches(t, info.Tx, old)
}

// assertQueuedHistory 断言链上最新接受的交易排队等待打包：保留完整原内容，
// 不带替换、确认块或淘汰关联。
func assertQueuedHistory(t *testing.T, n *Node, tx *Transaction) {
	t.Helper()
	info := mustTx(t, n, tx.ID())
	if info.Status != StatusQueued {
		t.Fatalf("tx %q status = %s, want queued", tx.Content, info.Status)
	}
	if info.ReplacedBy != "" || info.BlockHeight != 0 || info.BlockID != "" ||
		info.DropReason != "" || info.DropRound != 0 {
		t.Fatalf("queued tx %q must carry no links: %+v", tx.Content, info)
	}
	if info.Note != "" {
		t.Fatalf("direct tx query note = %q, want empty (note only appears on account pending)", info.Note)
	}
	assertTxMatches(t, info.Tx, tx)
}

// assertOnlyQueuedPending 断言账户在该序号上只列出最新这一笔待处理交易，
// 显示 queued / waiting-pack；已确认序号与序号缺口不因替换改变。
func assertOnlyQueuedPending(t *testing.T, n *Node, key testKey, latestID string, confirmed, gap uint64) {
	t.Helper()
	acct := n.Account(key.pub)
	if acct.ConfirmedSequence != confirmed {
		t.Fatalf("confirmed sequence = %d, want %d (replacements must not advance it)",
			acct.ConfirmedSequence, confirmed)
	}
	if acct.Gap != gap {
		t.Fatalf("account gap = %d, want %d (unchanged by replacements)", acct.Gap, gap)
	}
	if len(acct.Pending) != 1 {
		t.Fatalf("pending count = %d, want 1 (only the latest tx occupies the slot): %+v",
			len(acct.Pending), acct.Pending)
	}
	p := acct.Pending[0]
	if p.ID != latestID || p.Status != StatusQueued || p.Note != "waiting-pack" {
		t.Fatalf("pending entry = %+v, want %s queued/waiting-pack", p, latestID)
	}
}

// TestConsecutiveFeeBumpReplacementChain 覆盖同一待处理位置上连续两次加费替换后
// 的公开结果：两次提交结果、逐跳替换关联、历史原样保留与账户视图。
func TestConsecutiveFeeBumpReplacementChain(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	const seq uint64 = 1
	// 原交易，以及两笔同发送者同序号、签名有效、尚未到期、费用依次严格提高、
	// 内容与标识互不相同的新交易。
	orig := NewTransaction(keys[0].priv, seq, []byte("orig"), 1, 100)
	mid := NewTransaction(keys[0].priv, seq, []byte("middle"), 5, 100)
	last := NewTransaction(keys[0].priv, seq, []byte("last"), 9, 100)
	origID, midID, lastID := orig.ID(), mid.ID(), last.ID()
	if origID == midID || midID == lastID || origID == lastID {
		t.Fatal("test setup: the three transactions must have distinct ids")
	}
	if !(orig.Fee < mid.Fee && mid.Fee < last.Fee) {
		t.Fatalf("test setup: fees must rise strictly: %d < %d < %d", orig.Fee, mid.Fee, last.Fee)
	}

	// 原交易先被接收。
	r0, err := n.Submit(orig)
	if err != nil {
		t.Fatal(err)
	}
	if r0.TxID != origID || r0.ReplacedID != "" || r0.EvictedID != "" {
		t.Fatalf("orig submit result = %+v, want accept %s with no replacement/eviction", r0, origID)
	}

	// 第一次替换：返回本次接受的标识与刚被替换的原交易标识，不挤出任何交易。
	r1, err := n.Submit(mid)
	if err != nil {
		t.Fatal(err)
	}
	if r1.TxID != midID || r1.ReplacedID != origID || r1.EvictedID != "" {
		t.Fatalf("first replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			r1, midID, origID)
	}

	// 第二次替换：被替换的是“刚在该位置上的中间交易”，不是更早的原交易。
	r2, err := n.Submit(last)
	if err != nil {
		t.Fatal(err)
	}
	if r2.TxID != lastID || r2.ReplacedID != midID || r2.EvictedID != "" {
		t.Fatalf("second replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			r2, lastID, midID)
	}

	// 逐跳替换链：orig -> mid -> last。原交易的关联仍停在中间交易，
	// 不能被第二次替换直接改指到最后一笔。
	assertReplacedHistory(t, n, orig, midID)
	assertReplacedHistory(t, n, mid, lastID)
	assertQueuedHistory(t, n, last)

	// 三笔交易共占同一个待处理位置：池内只有一个占位。
	if got := n.st.poolSize(); got != 1 {
		t.Fatalf("pool size = %d, want 1 (three ids share one pending slot)", got)
	}
	if got := n.st.Pool[orig.SenderHex()][seq]; got != lastID {
		t.Fatalf("pool slot = %q, want latest %s", got, lastID)
	}

	// 账户在该序号上只列出最新交易：queued / waiting-pack；已确认序号与缺口不变。
	assertOnlyQueuedPending(t, n, keys[0], lastID, 0, 0)

	// 替换不推进当前轮次、不增加确认高度。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round/height = %d/%d, want 1/0", n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("replacements must not produce a confirmed block")
	}

	// 恰好三条“曾被接受”的历史记录，没有多也没有少。
	if got, want := len(n.st.Entries), 3; got != want {
		t.Fatalf("entries = %d, want %d (orig, middle, last)", got, want)
	}
}

// TestConsecutiveReplacementsSucceedOnFullPool 钉住满池场景：原交易与另一发送者
// 的排队交易占满容量后，连续两次加费替换都成功，被挤出标识为空，另一笔交易的
// 内容、状态与该账户待处理结果始终不变；替换不推进轮次与确认高度，已替换的
// 历史交易也不重新占用池容量。
func TestConsecutiveReplacementsSucceedOnFullPool(t *testing.T) {
	keys := genKeys(t, 4)
	const capacity uint64 = 2
	n, _ := newTestNodeCap(t, keys, 10, capacity)

	orig := NewTransaction(keys[0].priv, 1, []byte("orig-full"), 1, 100)
	other := NewTransaction(keys[1].priv, 1, []byte("other-sender"), 2, 100)
	for _, tx := range []*Transaction{orig, other} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	if got := n.st.poolSize(); got != capacity {
		t.Fatalf("test setup: pool size = %d, want full %d", got, capacity)
	}

	mid := NewTransaction(keys[0].priv, 1, []byte("middle-full"), 5, 100)
	last := NewTransaction(keys[0].priv, 1, []byte("last-full"), 9, 100)
	origID, midID, lastID := orig.ID(), mid.ID(), last.ID()
	if origID == midID || midID == lastID {
		t.Fatal("test setup: replacement transactions must have distinct ids")
	}

	// 第一次替换：满池也成功，且不挤出另一发送者的交易。
	r1, err := n.Submit(mid)
	if err != nil {
		t.Fatalf("first replacement on full pool must succeed: %v", err)
	}
	if r1.TxID != midID || r1.ReplacedID != origID || r1.EvictedID != "" {
		t.Fatalf("first full-pool replacement result = %+v, want replace %s without eviction",
			r1, origID)
	}

	// 第二次替换：同样成功，被挤出标识为空。
	r2, err := n.Submit(last)
	if err != nil {
		t.Fatalf("second replacement on full pool must succeed: %v", err)
	}
	if r2.TxID != lastID || r2.ReplacedID != midID || r2.EvictedID != "" {
		t.Fatalf("second full-pool replacement result = %+v, want replace %s without eviction",
			r2, midID)
	}

	// 替换链逐跳保留；最新交易仍排队。
	assertReplacedHistory(t, n, orig, midID)
	assertReplacedHistory(t, n, mid, lastID)
	assertQueuedHistory(t, n, last)

	// 替换不新增占位，已替换历史也不重新占用池容量：池仍是恰好满的两个占位。
	if got := n.st.poolSize(); got != capacity {
		t.Fatalf("pool size after chained replacements = %d, want %d (replacements add no slot)",
			got, capacity)
	}

	// 另一发送者的交易内容、状态与账户待处理结果完全不变。
	otherInfo := mustTx(t, n, other.ID())
	if otherInfo.Status != StatusQueued {
		t.Fatalf("other-sender tx status = %s, want queued", otherInfo.Status)
	}
	assertTxMatches(t, otherInfo.Tx, other)
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{other.ID(): "waiting-pack"})

	// 替换发起账户只剩最新交易排队。
	assertOnlyQueuedPending(t, n, keys[0], lastID, 0, 0)

	// 不推进轮次、不增加确认高度，也不产生任何淘汰记录。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round/height = %d/%d, want 1/0", n.CurrentRound(), n.Height())
	}
	for _, tx := range []*Transaction{orig, mid} {
		if info := mustTx(t, n, tx.ID()); info.Status != StatusReplaced {
			t.Fatalf("history tx %q status = %s, want replaced (not dropped, not re-occupying)",
				tx.Content, info.Status)
		}
	}
}

// TestReplacementRejectionsKeepChainAndAllowHigherFee 覆盖尚未产生提议、最新交易
// 仍排队时的两种拒绝：标识不同但费用等于或低于当前交易的替换尝试返回
// fee-not-higher 且不留历史；按原始内容与签名重投已被替换的交易返回
// duplicate-transaction。拒绝不改变既有替换链、最新排队交易与其他账户交易，
// 随后提交合法的更高费用交易仍可正常替换。
func TestReplacementRejectionsKeepChainAndAllowHigherFee(t *testing.T) {
	keys := genKeys(t, 4)
	const capacity uint64 = 2
	n, _ := newTestNodeCap(t, keys, 10, capacity)

	orig := NewTransaction(keys[0].priv, 1, []byte("orig-rej"), 1, 100)
	mid := NewTransaction(keys[0].priv, 1, []byte("middle-rej"), 5, 100)
	// 另一发送者的排队交易占住第二个位置，保证拒绝发生在池仍满的背景下。
	other := NewTransaction(keys[1].priv, 1, []byte("other-rej"), 2, 100)
	if _, err := n.Submit(orig); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(other); err != nil {
		t.Fatal(err)
	}
	r, err := n.Submit(mid)
	if err != nil {
		t.Fatal(err)
	}
	if r.TxID != mid.ID() || r.ReplacedID != orig.ID() {
		t.Fatalf("setup replacement result = %+v", r)
	}
	origID, midID := orig.ID(), mid.ID()

	// 拒绝操作的冻结基准：条目数、轮次、高度、池占位与另一账户交易。
	entriesBefore := len(n.st.Entries)
	roundBefore := n.CurrentRound()
	heightBefore := n.Height()

	// 拒绝一：标识不同（内容重新签署）但费用等于当前交易——必须 fee-not-higher。
	equalFee := NewTransaction(keys[0].priv, 1, []byte("equal-fee"), mid.Fee, 100)
	if equalFee.ID() == midID || equalFee.ID() == origID {
		t.Fatal("test setup: equal-fee tx must have a fresh distinct id")
	}
	if res, err := n.Submit(equalFee); reason(err) != ReasonLowFee || res != nil {
		t.Fatalf("equal-fee replacement got res=%+v err=%v, want nil + %s", res, err, ReasonLowFee)
	}
	if _, err := n.Tx(equalFee.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("fee-rejected tx lookup = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}

	// 拒绝二：标识不同但费用低于当前交易——同样 fee-not-higher、不留历史。
	lowerFee := NewTransaction(keys[0].priv, 1, []byte("lower-fee"), mid.Fee-1, 100)
	if lowerFee.ID() == midID || lowerFee.ID() == origID || lowerFee.ID() == equalFee.ID() {
		t.Fatal("test setup: lower-fee tx must have a fresh distinct id")
	}
	if res, err := n.Submit(lowerFee); reason(err) != ReasonLowFee || res != nil {
		t.Fatalf("lower-fee replacement got res=%+v err=%v, want nil + %s", res, err, ReasonLowFee)
	}
	if _, err := n.Tx(lowerFee.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("fee-rejected tx lookup = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}

	// 拒绝三：按原始内容与签名重投已经被替换的原交易——duplicate-transaction，
	// 而不是 fee-not-higher；旧交易不能重新入池。
	replay := NewTransaction(keys[0].priv, 1, []byte("orig-rej"), 1, 100)
	if replay.ID() != origID {
		t.Fatalf("test setup: byte-identical replay id %s != original %s", replay.ID(), origID)
	}
	if res, err := n.Submit(replay); reason(err) != ReasonDuplicate || res != nil {
		t.Fatalf("replay of replaced tx got res=%+v err=%v, want nil + %s (not %s)",
			res, err, ReasonDuplicate, ReasonLowFee)
	}

	// 三次拒绝后：不新增历史、不推进轮次/高度、池占位不变。
	if got := len(n.st.Entries); got != entriesBefore {
		t.Fatalf("entries changed %d -> %d after rejections", entriesBefore, got)
	}
	if n.CurrentRound() != roundBefore || n.Height() != heightBefore {
		t.Fatalf("round/height changed: %d/%d, want %d/%d",
			n.CurrentRound(), n.Height(), roundBefore, heightBefore)
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("rejected submits must not produce a block")
	}
	if got := n.st.poolSize(); got != capacity {
		t.Fatalf("pool size = %d, want %d after rejections", got, capacity)
	}

	// 既有替换链与最新排队交易保持原样。
	assertReplacedHistory(t, n, orig, midID)
	assertQueuedHistory(t, n, mid)
	if got := n.st.Pool[orig.SenderHex()][1]; got != midID {
		t.Fatalf("pool slot = %q, want %s (rejected replay must not re-enter)", got, midID)
	}
	assertOnlyQueuedPending(t, n, keys[0], midID, 0, 0)

	// 其他账户的交易内容、状态与待处理结果不受影响。
	otherInfo := mustTx(t, n, other.ID())
	if otherInfo.Status != StatusQueued {
		t.Fatalf("other-sender tx status = %s, want queued", otherInfo.Status)
	}
	assertTxMatches(t, otherInfo.Tx, other)
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{other.ID(): "waiting-pack"})

	// 尚未产生提议，最新交易仍在排队：随后一笔合法的更高费用交易仍可正常替换。
	if _, ok := n.Proposal(); ok {
		t.Fatal("test setup: no proposal should exist before the final valid replacement")
	}
	last := NewTransaction(keys[0].priv, 1, []byte("last-rej"), mid.Fee+4, 100)
	res, err := n.Submit(last)
	if err != nil {
		t.Fatalf("valid higher-fee replacement must still succeed after rejections: %v", err)
	}
	if res.TxID != last.ID() || res.ReplacedID != midID || res.EvictedID != "" {
		t.Fatalf("final replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			res, last.ID(), midID)
	}

	// 替换链延长为 orig -> mid -> last，前一跳没有被改指。
	assertReplacedHistory(t, n, orig, midID)
	assertReplacedHistory(t, n, mid, last.ID())
	assertQueuedHistory(t, n, last)
	assertOnlyQueuedPending(t, n, keys[0], last.ID(), 0, 0)
	// 另一账户交易依旧不变，池仍恰好占两个位置。
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{other.ID(): "waiting-pack"})
	if got := n.st.poolSize(); got != capacity {
		t.Fatalf("pool size = %d, want %d after valid replacement", got, capacity)
	}
}
