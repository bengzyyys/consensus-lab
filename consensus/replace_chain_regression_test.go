package consensus

import "testing"

// 回归保障：同一发送者、同一未确认序号的排队交易被连续加费替换时，
// 每一次替换都是独立的一步，历史查询能沿替换关系逐步看清过程。
// 本文件沿用既有 Go 库入口（Submit/Tx/Account/Propose）与既有拒绝原因
// （fee-not-higher、duplicate-transaction），不引入任何新行为，
// 只锁定这些既有结果：替换关联只指向“下一次”接受的交易，不会跨步改指；
// 历史交易内容、费用、到期轮次与签名原样保留，且不带确认块信息；
// 替换不推进轮次、不增加确认高度、不重新占用池容量。

// 三笔同发送者同序号、标识与内容均可区分的交易依次替换（费用 1 -> 5 -> 9）。
// 重点是第二次替换之后：原交易的替换关联仍指向第一次接受的新交易，
// 中间那笔的关联指向最后接受的交易，更早的关联不能跨步改指最后一笔。
func TestReplaceChainKeepsStepByStepHistory(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 原交易与两笔费用依次严格提高的替换交易：同发送者同序号，
	// 内容不同因此标识不同，占用的是同一个待处理位置。
	orig := NewTransaction(keys[0].priv, 1, []byte("chain-orig"), 1, 100)
	mid := NewTransaction(keys[0].priv, 1, []byte("chain-mid"), 5, 100)
	last := NewTransaction(keys[0].priv, 1, []byte("chain-last"), 9, 100)
	origID, midID, lastID := orig.ID(), mid.ID(), last.ID()
	if origID == midID || midID == lastID || origID == lastID {
		t.Fatal("test setup: distinct contents must yield distinct ids")
	}
	// 同账户序号 3 的排队交易制造缺口 2，用于验证替换不改变缺口。
	gapTx := NewTransaction(keys[0].priv, 3, []byte("chain-gap"), 7, 100)

	res, err := n.Submit(orig)
	if err != nil {
		t.Fatal(err)
	}
	if res.TxID != origID || res.ReplacedID != "" || res.EvictedID != "" {
		t.Fatalf("first submit result = %+v, want fresh accept of %s", res, origID)
	}
	if _, err := n.Submit(gapTx); err != nil {
		t.Fatal(err)
	}

	// 第一次替换：返回本次接受的标识与刚被替换的原交易标识。
	res, err = n.Submit(mid)
	if err != nil {
		t.Fatal(err)
	}
	if res.TxID != midID || res.ReplacedID != origID || res.EvictedID != "" {
		t.Fatalf("first replacement result = %+v, want accept %s replacing %s", res, midID, origID)
	}

	// 第二次替换：刚被替换的是中间那笔，不是原交易。
	res, err = n.Submit(last)
	if err != nil {
		t.Fatal(err)
	}
	if res.TxID != lastID || res.ReplacedID != midID || res.EvictedID != "" {
		t.Fatalf("second replacement result = %+v, want accept %s replacing %s", res, lastID, midID)
	}

	// 替换不推进当前轮次、不增加确认高度。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round/height = %d/%d, want 1/0; replacement must not advance the chain",
			n.CurrentRound(), n.Height())
	}
	if blk, ok := n.LatestBlock(); ok {
		t.Fatalf("replacement must not confirm a block, got %+v", blk)
	}

	// 原交易：仍显示 replaced，替换关联指向第一次接受的新交易，
	// 不能跨步直接改指最后一笔；不带任何确认块信息；内容原样保留。
	origInfo, err := n.Tx(origID)
	if err != nil {
		t.Fatal(err)
	}
	if origInfo.Status != StatusReplaced || origInfo.ReplacedBy != midID {
		t.Fatalf("orig record = status %s replacedBy %s, want replaced/%s (must not skip to %s)",
			origInfo.Status, origInfo.ReplacedBy, midID, lastID)
	}
	if origInfo.BlockHeight != 0 || origInfo.BlockID != "" {
		t.Fatalf("replaced history must not carry block info: %+v", origInfo)
	}
	assertTxMatches(t, origInfo.Tx, orig)

	// 中间那笔：同样显示 replaced，关联指向最后接受的交易。
	midInfo, err := n.Tx(midID)
	if err != nil {
		t.Fatal(err)
	}
	if midInfo.Status != StatusReplaced || midInfo.ReplacedBy != lastID {
		t.Fatalf("mid record = status %s replacedBy %s, want replaced/%s",
			midInfo.Status, midInfo.ReplacedBy, lastID)
	}
	if midInfo.BlockHeight != 0 || midInfo.BlockID != "" {
		t.Fatalf("replaced history must not carry block info: %+v", midInfo)
	}
	assertTxMatches(t, midInfo.Tx, mid)

	// 最新交易：排队等待打包，没有替换关联，也不带确认块信息。
	lastInfo, err := n.Tx(lastID)
	if err != nil {
		t.Fatal(err)
	}
	if lastInfo.Status != StatusQueued || lastInfo.ReplacedBy != "" ||
		lastInfo.BlockHeight != 0 || lastInfo.BlockID != "" {
		t.Fatalf("latest record wrong: %+v, want queued with no links", lastInfo)
	}
	assertTxMatches(t, lastInfo.Tx, last)

	// 账户查询在该序号上只列出最新交易：queued + waiting-pack；
	// 已确认序号仍为 0，序号 3 的排队交易不变，缺口仍为 2。
	acct := n.Account(keys[0].pub)
	if acct.ConfirmedSequence != 0 || acct.Gap != 2 || len(acct.Pending) != 2 {
		t.Fatalf("account wrong after chained replacement: %+v", acct)
	}
	p1 := acct.Pending[0]
	if p1.ID != lastID || p1.Status != StatusQueued || p1.Note != "waiting-pack" {
		t.Fatalf("pending entry for replaced sequence = %+v, want latest %s queued/waiting-pack",
			p1, lastID)
	}
	p3 := acct.Pending[1]
	if p3.ID != gapTx.ID() || p3.Status != StatusQueued || p3.Note != "waiting-pack" {
		t.Fatalf("unrelated pending entry changed: %+v", p3)
	}
}

// 原交易与另一笔不同发送者的排队交易恰好占满交易池后，连续两次加费替换
// 仍都能成功：替换不新占位置，被挤出标识为空，另一笔交易的内容、状态与
// 账户待处理结果不变；已替换的历史交易不重新占用池容量。
func TestReplaceChainInFullPoolKeepsCapacityAndOthers(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 2)

	// 容量 2：keys[0] 的原交易与 keys[1] 的排队交易恰好占满。
	orig := NewTransaction(keys[0].priv, 1, []byte("full-orig"), 1, 100)
	other := NewTransaction(keys[1].priv, 1, []byte("full-other"), 50, 100)
	for _, tx := range []*Transaction{orig, other} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.st.poolSize(); got != 2 {
		t.Fatalf("test setup: pool size = %d, want exactly full at 2", got)
	}
	entriesBefore := len(n.st.Entries)

	// 第一次替换：满池也成功，不挤出任何交易。
	r1 := NewTransaction(keys[0].priv, 1, []byte("full-r1"), 5, 100)
	res, err := n.Submit(r1)
	if err != nil {
		t.Fatalf("first replacement in full pool must succeed: %v", err)
	}
	if res.TxID != r1.ID() || res.ReplacedID != orig.ID() || res.EvictedID != "" {
		t.Fatalf("first replacement result = %+v, want replace %s with empty evicted id", res, orig.ID())
	}
	if got := n.st.poolSize(); got != 2 {
		t.Fatalf("pool size after first replacement = %d, want 2 (replaced history must not occupy capacity)", got)
	}

	// 第二次替换：同样成功，被挤出标识仍为空。
	r2 := NewTransaction(keys[0].priv, 1, []byte("full-r2"), 9, 100)
	res, err = n.Submit(r2)
	if err != nil {
		t.Fatalf("second replacement in full pool must succeed: %v", err)
	}
	if res.TxID != r2.ID() || res.ReplacedID != r1.ID() || res.EvictedID != "" {
		t.Fatalf("second replacement result = %+v, want replace %s with empty evicted id", res, r1.ID())
	}

	// 替换不推进轮次、不增加确认高度；两段历史都保留但不占容量。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round/height = %d/%d, want 1/0", n.CurrentRound(), n.Height())
	}
	if got := n.st.poolSize(); got != 2 {
		t.Fatalf("pool size after second replacement = %d, want 2", got)
	}
	if got := len(n.st.Entries); got != entriesBefore+2 {
		t.Fatalf("entries = %d, want %d (two replacement histories kept)", got, entriesBefore+2)
	}

	// 替换链逐步可查：orig -> r1 -> r2，r2 排队。
	if info, _ := n.Tx(orig.ID()); info.Status != StatusReplaced || info.ReplacedBy != r1.ID() {
		t.Fatalf("orig record = %+v, want replaced by %s", info, r1.ID())
	}
	if info, _ := n.Tx(r1.ID()); info.Status != StatusReplaced || info.ReplacedBy != r2.ID() {
		t.Fatalf("r1 record = %+v, want replaced by %s", info, r2.ID())
	}
	r2Info, err := n.Tx(r2.ID())
	if err != nil {
		t.Fatal(err)
	}
	if r2Info.Status != StatusQueued || r2Info.ReplacedBy != "" {
		t.Fatalf("r2 record = %+v, want queued with no link", r2Info)
	}
	assertTxMatches(t, r2Info.Tx, r2)

	// 另一发送者的排队交易内容、状态原样，账户待处理结果不变。
	otherInfo, err := n.Tx(other.ID())
	if err != nil {
		t.Fatal(err)
	}
	if otherInfo.Status != StatusQueued || otherInfo.ReplacedBy != "" ||
		otherInfo.DropReason != "" || otherInfo.DropRound != 0 {
		t.Fatalf("other tx changed by replacements: %+v", otherInfo)
	}
	assertTxMatches(t, otherInfo.Tx, other)
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{other.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{r2.ID(): "waiting-pack"})
}

// 连续替换之后、尚未产生提议且最新交易仍排队时的两种拒绝：
// 费用等于或低于当前交易的新标识替换尝试返回 fee-not-higher 且不留历史；
// 按原始内容与签名重投已被替换的交易返回 duplicate-transaction。
// 拒绝后既有替换关联、最新排队交易与其他账户交易均保持原样，
// 随后合法的更高费用交易仍可正常替换。
func TestReplaceChainRejectionsLeaveHistoryIntact(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 另一发送者的排队交易：拒绝操作不得波及。
	other := NewTransaction(keys[1].priv, 1, []byte("bystander"), 3, 100)
	if _, err := n.Submit(other); err != nil {
		t.Fatal(err)
	}

	// 建立 orig -> mid -> last 的替换链（费用 1 -> 5 -> 9）。
	orig := NewTransaction(keys[0].priv, 1, []byte("rej-orig"), 1, 100)
	mid := NewTransaction(keys[0].priv, 1, []byte("rej-mid"), 5, 100)
	last := NewTransaction(keys[0].priv, 1, []byte("rej-last"), 9, 100)
	for _, tx := range []*Transaction{orig, mid, last} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	origID, midID, lastID := orig.ID(), mid.ID(), last.ID()

	// 场景前提：尚未产生提议，最新交易仍在排队。
	if _, ok := n.Proposal(); ok {
		t.Fatal("test setup: no proposal must exist yet")
	}
	if info, _ := n.Tx(lastID); info.Status != StatusQueued {
		t.Fatalf("test setup: latest status = %s, want queued", info.Status)
	}
	entriesBefore := len(n.st.Entries)

	// 拒绝一：标识不同、费用与当前交易相等（9）的替换尝试。
	equalFee := NewTransaction(keys[0].priv, 1, []byte("rej-equal"), 9, 100)
	if equalFee.ID() == lastID {
		t.Fatal("test setup: equal-fee tx must have a distinct id")
	}
	res, err := n.Submit(equalFee)
	if reason(err) != ReasonLowFee {
		t.Fatalf("equal-fee replacement reason = %v, want %s", err, ReasonLowFee)
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}
	if _, err := n.Tx(equalFee.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("equal-fee tx lookup = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}

	// 拒绝二：标识不同、费用低于当前交易（6 < 9，虽高于中间那笔的 5）的替换尝试。
	// 费用比较只针对当前排队交易，与任何历史交易无关。
	lowerFee := NewTransaction(keys[0].priv, 1, []byte("rej-lower"), 6, 100)
	res, err = n.Submit(lowerFee)
	if reason(err) != ReasonLowFee {
		t.Fatalf("lower-fee replacement reason = %v, want %s", err, ReasonLowFee)
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}
	if _, err := n.Tx(lowerFee.ID()); reason(err) != ReasonUnknownTx {
		t.Fatalf("lower-fee tx lookup = %v, want %s (no history kept)", err, ReasonUnknownTx)
	}

	// 拒绝三：按原始内容与签名重投已被替换的原交易。
	replayOrig := NewTransaction(keys[0].priv, 1, []byte("rej-orig"), 1, 100)
	if replayOrig.ID() != origID {
		t.Fatalf("test setup: byte-identical replay id %s != original %s", replayOrig.ID(), origID)
	}
	res, err = n.Submit(replayOrig)
	if reason(err) != ReasonDuplicate {
		t.Fatalf("replay of replaced orig reason = %v, want %s", err, ReasonDuplicate)
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 拒绝四：按原始内容与签名重投已被替换的中间交易。
	replayMid := NewTransaction(keys[0].priv, 1, []byte("rej-mid"), 5, 100)
	if replayMid.ID() != midID {
		t.Fatalf("test setup: byte-identical replay id %s != mid %s", replayMid.ID(), midID)
	}
	res, err = n.Submit(replayMid)
	if reason(err) != ReasonDuplicate {
		t.Fatalf("replay of replaced mid reason = %v, want %s", err, ReasonDuplicate)
	}
	if res != nil {
		t.Fatalf("rejected submit must return nil result, got %+v", res)
	}

	// 拒绝不留副作用：不新增记录、不推进轮次、不增加确认高度。
	if got := len(n.st.Entries); got != entriesBefore {
		t.Fatalf("entries changed %d -> %d after rejections", entriesBefore, got)
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("round/height = %d/%d, want 1/0 after rejections", n.CurrentRound(), n.Height())
	}

	// 已有替换关联保持原样：orig -> mid -> last，不跨步改指。
	origInfo, err := n.Tx(origID)
	if err != nil {
		t.Fatal(err)
	}
	if origInfo.Status != StatusReplaced || origInfo.ReplacedBy != midID {
		t.Fatalf("orig record after rejections = %+v, want replaced/%s", origInfo, midID)
	}
	assertTxMatches(t, origInfo.Tx, orig)
	midInfo, err := n.Tx(midID)
	if err != nil {
		t.Fatal(err)
	}
	if midInfo.Status != StatusReplaced || midInfo.ReplacedBy != lastID {
		t.Fatalf("mid record after rejections = %+v, want replaced/%s", midInfo, lastID)
	}
	assertTxMatches(t, midInfo.Tx, mid)

	// 最新排队交易与其他账户交易均保持原样。
	lastInfo, err := n.Tx(lastID)
	if err != nil {
		t.Fatal(err)
	}
	if lastInfo.Status != StatusQueued || lastInfo.ReplacedBy != "" {
		t.Fatalf("latest record after rejections = %+v, want queued with no link", lastInfo)
	}
	assertTxMatches(t, lastInfo.Tx, last)
	otherInfo, err := n.Tx(other.ID())
	if err != nil {
		t.Fatal(err)
	}
	if otherInfo.Status != StatusQueued {
		t.Fatalf("bystander tx changed: %+v", otherInfo)
	}
	assertTxMatches(t, otherInfo.Tx, other)
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{lastID: "waiting-pack"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{other.ID(): "waiting-pack"})

	// 随后提交合法的更高费用交易：仍按既有规则正常替换最新交易。
	final := NewTransaction(keys[0].priv, 1, []byte("rej-final"), 10, 100)
	res, err = n.Submit(final)
	if err != nil {
		t.Fatalf("valid higher-fee tx must still replace after rejections: %v", err)
	}
	if res.TxID != final.ID() || res.ReplacedID != lastID || res.EvictedID != "" {
		t.Fatalf("final replacement result = %+v, want accept %s replacing %s", res, final.ID(), lastID)
	}
	// 链条延长一步：last -> final；更早的关联仍不跨步改指。
	if info, _ := n.Tx(lastID); info.Status != StatusReplaced || info.ReplacedBy != final.ID() {
		t.Fatalf("last record = %+v, want replaced/%s", info, final.ID())
	}
	if info, _ := n.Tx(midID); info.ReplacedBy != lastID {
		t.Fatalf("mid link rewired to %s, want still %s", info.ReplacedBy, lastID)
	}
	if info, _ := n.Tx(origID); info.ReplacedBy != midID {
		t.Fatalf("orig link rewired to %s, want still %s", info.ReplacedBy, midID)
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{final.ID(): "waiting-pack"})
}
