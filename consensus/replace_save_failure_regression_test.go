package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住“合法加费替换”的原子落盘回归保障。
//
// 场景：同一发送者、同一正整数序号的两笔交易，签名有效且尚未到期；旧交易已在
// 池中排队，新交易内容不同、费用严格更高，旧交易未被任何未决候选引用，按现有
// 规则本应允许替换。但当这次提交因节点状态保存失败而结束时，替换必须整体不
// 生效：提交返回保存错误且结果为空（不能同时给出成功接收或成功替换的结果）；
// 旧交易查询仍显示排队，保留原内容、费用与标识，替代交易关联为空；新交易查询
// 仍得到 unknown-transaction，不留排队记录或替换历史；账户的已确认序号、原有
// 缺口与后续序号交易的排队位置全部保持操作前结果。磁盘状态文件逐字节不变、
// 不残留临时文件，重新打开状态目录看到的与当前查询一致——不能出现当前查询
// 恢复了旧交易、再次打开却看到新交易已替换成功的差异。
//
// 池容量已用满时同一保障同样成立：合法加费替换不新增占位，保存失败也不能为了
// 给新交易腾位置而挤出其他交易，其他账户的待处理记录与淘汰关联保持原样。
//
// 保存恢复正常后，再提交刚才失败的同一笔新交易，必须按一次正常替换成功处理，
// 不能因失败尝试的残留记录被认作重复交易：结果关联被替换的旧标识、不带被挤出
// 标识；旧交易此时才显示已被替换并指向新交易，新交易成为该序号唯一的待处理
// 交易；账户已确认序号仍不推进，池满时也不影响其他账户。
// 本组测试不新增公开入口，也不改变现有替换规则。

// assertStateFileUnchanged 断言失败的保存没有触碰磁盘：状态文件逐字节保持
// 操作前内容，目录中仅有 state.json，不残留临时文件。
func assertStateFileUnchanged(t *testing.T, dir string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("failed save must not leave temp files, dir contains %v", names)
	}
}

// assertOldTxQueued 断言旧交易在失败的替换后保持操作前形态：仍排队、完整内容
// 与原交易逐字段一致、费用与标识不变，替代交易关联为空，也不带确认块关联。
func assertOldTxQueued(t *testing.T, n *Node, old *Transaction) {
	t.Helper()
	info := n.mustTx(t, old.ID())
	if info.Status != StatusQueued {
		t.Fatalf("old tx status = %s, want queued after the failed replacement", info.Status)
	}
	assertTxMatches(t, info.Tx, old)
	if info.ReplacedBy != "" {
		t.Fatalf("old tx replacement link = %q, want empty after the failed replacement", info.ReplacedBy)
	}
	if info.BlockHeight != 0 || info.BlockID != "" {
		t.Fatalf("old tx must not reference a block: height=%d block=%q", info.BlockHeight, info.BlockID)
	}
}

// assertNewTxUnknown 断言失败的新交易没有留下任何记录：查询得到
// unknown-transaction，既无排队记录也无替换历史。
func assertNewTxUnknown(t *testing.T, n *Node, newID string) {
	t.Helper()
	if _, err := n.Tx(newID); reason(err) != ReasonUnknownTx {
		t.Fatalf("failed new tx must stay unknown, got %v (want %s)", err, ReasonUnknownTx)
	}
}

// assertReplaceRolledBack 断言（不限容场景）保存失败后节点完全处于操作前状态：
// 旧交易仍排队、新交易无记录、同账户后续序号交易仍按原序号排队，
// 已确认序号与原有缺口不变。
func assertReplaceRolledBack(t *testing.T, n *Node, key testKey, old, later *Transaction, newID string) {
	t.Helper()
	assertOldTxQueued(t, n, old)
	assertNewTxUnknown(t, n, newID)
	if info := n.mustTx(t, later.ID()); info.Status != StatusQueued {
		t.Fatalf("follow-up tx status = %s, want queued after the failed replacement", info.Status)
	}
	assertAccountQueue(t, n, key.pub, 0, map[string]string{
		old.ID():   "waiting-pack",
		later.ID(): "waiting-pack",
	})
	acct := n.Account(key.pub)
	if acct.Gap != 1 {
		t.Fatalf("account gap = %d, want 1 (unchanged by the failed replacement)", acct.Gap)
	}
	// 待处理列表按序号升序：序号 2 仍是旧交易，序号 3 的位置没有被失败的新交易占据。
	if acct.Pending[0].ID != old.ID() || acct.Pending[1].ID != later.ID() {
		t.Fatalf("pending order = [%s %s], want [old new-follow-up]", acct.Pending[0].ID, acct.Pending[1].ID)
	}
}

// assertReplaceApplied 断言（不限容场景）保存恢复后同一笔新交易按一次正常替换
// 成功处理后的状态：旧交易已被替换并指向新交易，新交易成为该序号唯一的待处理
// 交易，后续序号交易仍在原位，已确认序号与缺口不因此推进或改变。
func assertReplaceApplied(t *testing.T, n *Node, key testKey, old, newTx, later *Transaction) {
	t.Helper()
	oldInfo := n.mustTx(t, old.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newTx.ID() {
		t.Fatalf("old tx = {status:%s replacedBy:%s}, want replaced by %s",
			oldInfo.Status, oldInfo.ReplacedBy, newTx.ID())
	}
	assertTxMatches(t, oldInfo.Tx, old) // 被替换的旧交易保留原完整内容
	newInfo := n.mustTx(t, newTx.ID())
	if newInfo.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", newInfo.Status)
	}
	assertTxMatches(t, newInfo.Tx, newTx)
	if info := n.mustTx(t, later.ID()); info.Status != StatusQueued {
		t.Fatalf("follow-up tx status = %s, want queued", info.Status)
	}
	assertAccountQueue(t, n, key.pub, 0, map[string]string{
		newTx.ID(): "waiting-pack",
		later.ID(): "waiting-pack",
	})
	acct := n.Account(key.pub)
	if acct.Gap != 1 {
		t.Fatalf("account gap = %d, want 1 (replacement must not move the gap)", acct.Gap)
	}
	// 序号 2 位置现在唯一地属于新交易，序号 3 交易保持原位。
	if acct.Pending[0].ID != newTx.ID() || acct.Pending[1].ID != later.ID() {
		t.Fatalf("pending order = [%s %s], want [new follow-up]", acct.Pending[0].ID, acct.Pending[1].ID)
	}
}

// TestReplaceSaveFailureAtomic 覆盖合法加费替换保存失败的完整回归序列：
// 失败整体回滚（保存错误、结果为空，旧交易与新交易查询、账户序号/缺口/后续
// 序号交易全部照旧）-> 内存/磁盘/重开三处一致 -> 恢复后同一笔新交易按正常
// 替换成功，而非被当作重复交易。
func TestReplaceSaveFailureAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	// 旧交易排在序号 2（序号 1 缺口保留），同账户另有后续序号 3 交易排队。
	old := NewTransaction(keys[0].priv, 2, []byte("old"), 1, 100)
	later := NewTransaction(keys[0].priv, 3, []byte("later"), 3, 100)
	for _, tx := range []*Transaction{old, later} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}

	// 新交易：同发送者同序号、内容不同、费用严格更高、未到期、未被候选引用——
	// 按现有规则是一笔合法替换。
	newTx := NewTransaction(keys[0].priv, 2, []byte("new"), 5, 100)
	if newTx.ID() == old.ID() || newTx.Fee <= old.Fee {
		t.Fatal("setup: new tx must differ from old tx with a strictly higher fee")
	}
	assertReplaceRolledBack(t, n, keys[0], old, later, newTx.ID())

	// 快照替换前的磁盘文件：失败的保存不得改写它，也不得残留临时文件。
	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 合法替换在此刻保存节点状态失败：必须返回保存错误，且不能同时给出
	// 成功接收或成功替换的结果。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Submit(newTx)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("replacement submit must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a submit rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("submit result must be nil on save failure, got %+v", res)
	}

	// 内存：旧交易、新交易、后续序号交易、账户序号与缺口全部保持操作前结果。
	assertReplaceRolledBack(t, n, keys[0], old, later, newTx.ID())

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 重开节点：保存记录中没有本次替换造成的任何变化——不能出现当前查询
	// 恢复了旧交易、再次打开却看到新交易已替换成功的差异。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertReplaceRolledBack(t, reopened, keys[0], old, later, newTx.ID())

	// 保存恢复正常后，再提交刚才失败的同一笔新交易：必须按一次正常替换成功
	// 处理，不能因失败尝试残留记录而被认作重复交易。
	res, err = reopened.Submit(newTx)
	if err != nil {
		t.Fatalf("resubmit after recovery must succeed as a normal replacement: %v", err)
	}
	if res.TxID != newTx.ID() || res.ReplacedID != old.ID() || res.EvictedID != "" {
		t.Fatalf("replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			res, newTx.ID(), old.ID())
	}
	assertReplaceApplied(t, reopened, keys[0], old, newTx, later)

	// 替换结果随再次落盘持久化：再次重开看到完全一致的状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertReplaceApplied(t, durable, keys[0], old, newTx, later)
}

// assertFullPoolRolledBack 断言（满池场景）保存失败后节点完全处于操作前状态：
// 旧交易仍排队、新交易无记录；池内其他账户的待处理交易保持排队，此前被挤出的
// 交易保留 dropped 记录与淘汰关联（原因与发生轮次），没有为了给新交易腾位置
// 而挤出任何交易。
func assertFullPoolRolledBack(t *testing.T, n *Node, keys []testKey, old, otherC, d, dropped *Transaction, newID string) {
	t.Helper()
	assertOldTxQueued(t, n, old)
	assertNewTxUnknown(t, n, newID)

	// 其他账户的排队交易原样保留。
	for _, tx := range []*Transaction{otherC, d} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("unrelated tx %q status = %s, want queued after the failed replacement", tx.Content, info.Status)
		}
		assertTxMatches(t, info.Tx, tx)
	}
	// 既有的淘汰记录与关联保持原样：原因 pool-capacity、发生轮次 1，
	// 不带确认块或替换关联。
	dropInfo := n.mustTx(t, dropped.ID())
	if dropInfo.Status != StatusDropped || dropInfo.DropReason != DropReasonPoolCapacity || dropInfo.DropRound != 1 {
		t.Fatalf("dropped record changed: %+v", dropInfo)
	}
	if dropInfo.BlockHeight != 0 || dropInfo.BlockID != "" || dropInfo.ReplacedBy != "" {
		t.Fatalf("dropped tx must not carry block/replace links: %+v", dropInfo)
	}
	assertTxMatches(t, dropInfo.Tx, dropped)

	// 各账户查询：已确认序号均为 0；旧交易账户只有旧交易排队，被挤出账户
	// 没有待处理交易，其余账户各留自己的一笔。
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{old.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{otherC.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{d.ID(): "waiting-pack"})
}

// assertFullPoolReplaced 断言（满池场景）保存恢复后同一笔新交易替换成功后的
// 状态：旧交易被替换并指向新交易，新交易排队；替换不新增占位、不淘汰其他
// 交易——其他账户的排队交易与既有淘汰记录全部保持原样，已确认序号不推进。
func assertFullPoolReplaced(t *testing.T, n *Node, keys []testKey, old, newTx, otherC, d, dropped *Transaction) {
	t.Helper()
	oldInfo := n.mustTx(t, old.ID())
	if oldInfo.Status != StatusReplaced || oldInfo.ReplacedBy != newTx.ID() {
		t.Fatalf("old tx = {status:%s replacedBy:%s}, want replaced by %s",
			oldInfo.Status, oldInfo.ReplacedBy, newTx.ID())
	}
	newInfo := n.mustTx(t, newTx.ID())
	if newInfo.Status != StatusQueued {
		t.Fatalf("new tx status = %s, want queued", newInfo.Status)
	}
	assertTxMatches(t, newInfo.Tx, newTx)

	// 池满也不影响其他账户：排队交易与淘汰记录原样。
	for _, tx := range []*Transaction{otherC, d} {
		if info := n.mustTx(t, tx.ID()); info.Status != StatusQueued {
			t.Fatalf("unrelated tx %q status = %s, want queued", tx.Content, info.Status)
		}
	}
	dropInfo := n.mustTx(t, dropped.ID())
	if dropInfo.Status != StatusDropped || dropInfo.DropReason != DropReasonPoolCapacity || dropInfo.DropRound != 1 {
		t.Fatalf("dropped record changed by the replacement: %+v", dropInfo)
	}
	assertAccountQueue(t, n, keys[0].pub, 0, map[string]string{newTx.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{})
	assertAccountQueue(t, n, keys[2].pub, 0, map[string]string{otherC.ID(): "waiting-pack"})
	assertAccountQueue(t, n, keys[3].pub, 0, map[string]string{d.ID(): "waiting-pack"})
}

// TestReplaceSaveFailureAtomicFullPool 把池容量用满的情况纳入同一项保障：
// 合法加费替换不新增占位，保存失败不能为了给新交易腾位置而挤出其他交易；
// 恢复后同一笔新交易替换成功，结果不带被挤出标识，其他账户不受影响。
func TestReplaceSaveFailureAtomicFullPool(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNodeCap(t, keys, 10, 3)

	// 池容量 3：旧交易（账户0，费用 4）与另外两账户的交易把池占满。
	old := NewTransaction(keys[0].priv, 1, []byte("old"), 4, 100)
	otherB := NewTransaction(keys[1].priv, 1, []byte("other-b"), 2, 100)
	otherC := NewTransaction(keys[2].priv, 1, []byte("other-c"), 3, 100)
	for _, tx := range []*Transaction{old, otherB, otherC} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("setup submit %q: %v", tx.Content, err)
		}
	}
	// 账户3 的高费交易挤出费用最低的 otherB，留下一条既有淘汰记录；
	// 池仍为满：old(4)、otherC(3)、d(9)。
	d := NewTransaction(keys[3].priv, 1, []byte("d"), 9, 100)
	res, err := n.Submit(d)
	if err != nil || res.EvictedID != otherB.ID() {
		t.Fatalf("setup eviction: %+v %v", res, err)
	}

	// 满池下的合法加费替换：同发送者同序号、费用严格更高，不新增占位，
	// 也不应淘汰任何交易。
	newTx := NewTransaction(keys[0].priv, 1, []byte("new"), 10, 100)
	assertFullPoolRolledBack(t, n, keys, old, otherC, d, otherB, newTx.ID())

	before, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 满池时的合法替换在此刻保存失败：返回保存错误且结果为空。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err = n.Submit(newTx)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("replacement on a full pool must return the save error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a submit rejection %q: %v", reason(err), err)
	}
	if res != nil {
		t.Fatalf("submit result must be nil on save failure, got %+v", res)
	}

	// 内存：旧交易仍在，新交易无记录；没有为了给新交易腾位置而挤出其他
	// 交易——其他账户的待处理记录与淘汰关联全部保持原样。
	assertFullPoolRolledBack(t, n, keys, old, otherC, d, otherB, newTx.ID())

	// 磁盘：状态文件逐字节不变，目录中仅有 state.json。
	assertStateFileUnchanged(t, dir, before)

	// 重开节点：与当前查询完全一致的操作前状态。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertFullPoolRolledBack(t, reopened, keys, old, otherC, d, otherB, newTx.ID())

	// 保存恢复正常后，再提交刚才失败的同一笔新交易：按一次正常替换成功
	// 处理——结果关联被替换的旧标识，不带被挤出标识（满池也不淘汰）。
	res, err = reopened.Submit(newTx)
	if err != nil {
		t.Fatalf("resubmit on full pool after recovery must succeed as a normal replacement: %v", err)
	}
	if res.TxID != newTx.ID() || res.ReplacedID != old.ID() || res.EvictedID != "" {
		t.Fatalf("full-pool replacement result = %+v, want {TxID:%s ReplacedID:%s EvictedID:\"\"}",
			res, newTx.ID(), old.ID())
	}
	assertFullPoolReplaced(t, reopened, keys, old, newTx, otherC, d, otherB)

	// 替换结果随再次落盘持久化：再次重开看到完全一致的状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertFullPoolReplaced(t, durable, keys, old, newTx, otherC, d, otherB)
}
